package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpFixturePrinter  = "profile:printer"
	mcpFixtureFilename = "approved.gcode"
	mcpFixtureGCode    = "G28\nG1 X5 Y5\n"
	mcpFixtureKey      = "fixture-printer-api-key"
	mcpFixtureOptions  = `{
		"nozzle_diameter":{"type":"Floats","profile":"machine"},
		"printable_area":{"type":"Points","profile":"machine"},
		"printable_height":{"type":"Float","profile":"machine"},
		"manual_filament_change":{"type":"Bool","profile":"machine"},
		"layer_height":{"type":"Float","profile":"process"},
		"print_settings_id":{"type":"String","profile":"process"},
		"printer_settings_id":{"type":"String","profile":"machine"},
		"filament_type":{"type":"String","profile":"filament"},
		"filament_extruder_variant":{"type":"Strings","profile":"filament"},
		"filament_self_index":{"type":"Ints","profile":"filament"},
		"filament_settings_id":{"type":"Strings","profile":"filament"}
	}`
)

func mcpFixtureCatalog(t *testing.T) *resolve.Catalog {
	t.Helper()
	if err := configureResolve([]byte(mcpFixtureOptions)); err != nil {
		t.Fatal(err)
	}
	const header = `{"schema":"pancake.catalog.v1","version":"fixture"}` + "\n"
	catalog, err := resolve.LoadCatalog(
		strings.NewReader(header+`{"id":"Fixture","vendor":"BBL","base":{"nozzle_diameter":"0.4","printable_area":"0x0,100x0,100x100,0x100","printable_height":"100"},"variants":[{"id":"Printer","nozzle":"0.4","extruders":1}]}`),
		strings.NewReader(header+`{"id":"Filament","base":{"filament_type":"PLA"}}`),
		strings.NewReader(header+`{"id":"Process","base":{"layer_height":"0.2"}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type mcpTestRoundTripper func(*http.Request) (*http.Response, error)

func (f mcpTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mcpTestConnect(t *testing.T, server *httptest.Server, client *http.Client, options *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "pancakeslicer-test", Version: "1"}, options)
	session, err := sdk.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL + "/mcp", HTTPClient: client, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session
}

func mcpTestCall(t *testing.T, session *mcp.ClientSession, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil {
		if result.IsError {
			t.Fatalf("%s: %s", name, mcpTestResultText(result))
		}
		mcpTestDecode(t, result.StructuredContent, out)
	}
	return result
}

func mcpTestDecode(t *testing.T, value any, out any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

func mcpTestRefusal(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	message := mcpTestResultText(result)
	if !result.IsError || !strings.Contains(strings.ToLower(message), want) {
		t.Fatalf("confirmation error = %q, want error containing %q", message, want)
	}
}

func mcpTestResultText(result *mcp.CallToolResult) string {
	var message strings.Builder
	for _, item := range result.Content {
		if content, ok := item.(*mcp.TextContent); ok {
			message.WriteString(content.Text)
		}
	}
	return message.String()
}

type mcpTestUpload struct {
	filename string
	gcode    string
	print    string
	key      string
}

type mcpPrintFixture struct {
	server  *Server
	http    *httptest.Server
	uploads chan mcpTestUpload
	now     atomic.Int64
}

func newMCPPrintFixture(t *testing.T) *mcpPrintFixture {
	t.Helper()
	return newMCPPrintFixtureWithUploadGate(t, nil)
}

func newMCPPrintFixtureWithUploadGate(t *testing.T, uploadGate <-chan struct{}) *mcpPrintFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("MCP slicer fixture uses a POSIX shell")
	}
	fixture := &mcpPrintFixture{uploads: make(chan mcpTestUpload, 8)}
	fixture.now.Store(time.Now().UnixNano())
	root := t.TempDir()
	cfg := Config{
		MCPEnabled: true, MCPConfirmationTTL: time.Minute, MaxUploadSize: 1 << 20,
		UploadMinRate: 256 << 10,
		JobRoot:       filepath.Join(root, "jobs"), DataDir: root, Tool: filepath.Join(root, "slicer"), MaxQueue: -1,
	}
	tool := `#!/bin/sh
set -eu
found_model=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      shift
      printf '%s' '` + mcpFixtureGCode + `' > "$1"
      ;;
    --json)
      shift
      printf '%s' '{"totals":{"estimated_time_s":17},"statistics":{"total_weight_g":1.25},"warnings":["Fixture warning"]}' > "$1"
      ;;
    */input/fixture.stl)
      test -s "$1"
      found_model=true
      ;;
    */input/plate.json)
      grep -q 'fixture.stl' "$1"
      test -s "${1%/*}/fixture.stl"
      found_model=true
      ;;
  esac
  shift
done
test "$found_model" = true
`
	if err := os.WriteFile(cfg.Tool, []byte(tool), 0700); err != nil {
		t.Fatal(err)
	}

	addr := cameraFixtureAddress(t)
	octoprint := http.NewServeMux()
	octoprint.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "{}")
	})
	octoprint.HandleFunc("GET /api/job", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"state":"Operational"}`)
	})
	octoprint.HandleFunc("GET /api/printer", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "{}")
	})
	octoprint.HandleFunc("POST /api/files/local", func(w http.ResponseWriter, r *http.Request) {
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		raw, err := io.ReadAll(file)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.uploads <- mcpTestUpload{header.Filename, string(raw), r.FormValue("print"), r.Header.Get("X-Api-Key")}
		if uploadGate != nil {
			select {
			case <-uploadGate:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
	})
	printer := cameraFixtureServer(t, addr, octoprint.ServeHTTP)
	printer.Start()
	policy, err := parseHostPolicy(addr.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadPrinterStore(filepath.Join(root, "printers"), "", policy)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := store.config(storedPrinter{
		ID: "b0060123456789ab", Printer: mcpFixturePrinter, Name: "Fixture OctoPrint", Kind: "octoprint", URL: printer.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connection.CredentialFile, []byte(mcpFixtureKey), 0600); err != nil {
		t.Fatal(err)
	}
	store.printers = []*PrinterConfig{connection}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	cfg.PrinterStore = store
	catalog := mcpFixtureCatalog(t)
	doc := emptyLibrary("")
	doc["printers"] = json.RawMessage(`[{"id":"profile:printer","name":"Printer","revision":1,"vendor":"BBL","model":"Fixture","nozzles":[{"variant":"Printer","diameter":0.4}],"swap":"manual","slots":[{"id":"slot:filament","filament":"profile:filament"}]}]`)
	doc["processes"] = json.RawMessage(`[{"id":"profile:process","name":"Process","revision":1,"product":"Process"}]`)
	doc["filaments"] = json.RawMessage(`[{"id":"profile:filament","name":"Filament","revision":1,"product":"Filament"}]`)
	library := &libraryStore{dir: root, catalog: catalog, settled: true}
	if err := library.write(&libraryEnvelope{Document: doc, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	jobs, err := NewJobManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jobs.Shutdown)
	s := &Server{
		cfg: cfg, jobs: jobs, catalog: catalog, library: library, sends: NewSendManager(store),
		blobs:     &blobStore{root: filepath.Join(root, "blobs"), limit: blobCacheBytes},
		workspace: &workspaceStore{root: filepath.Join(root, "workspace")},
	}
	s.filamentSlots = newFilamentSlotStore(filepath.Join(root, "printers"))
	t.Cleanup(func() {
		hub := s.sends.hub
		hub.mu.Lock()
		defer hub.mu.Unlock()
		if hub.idle != nil {
			hub.idle.Stop()
		}
		for _, monitor := range hub.monitors {
			monitor.cancel()
		}
	})
	service := newMCPService(s)
	service.now = func() time.Time { return time.Unix(0, fixture.now.Load()) }
	mux := http.NewServeMux()
	mux.Handle("/mcp", service.handler())
	mux.HandleFunc("GET /api/jobs/{id}/send", s.getSend)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.deleteJob)
	fixture.server, fixture.http = s, httptest.NewServer(gzipJSON(s.middleware(revalidate(mux))))
	t.Cleanup(fixture.http.Close)
	return fixture
}

func (f *mcpPrintFixture) connect(t *testing.T, options *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	return mcpTestConnect(t, f.http, f.http.Client(), options)
}

func (f *mcpPrintFixture) readyPrinter(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var result struct {
			Printers []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Kind   string `json:"kind"`
				Online bool   `json:"online"`
				State  string `json:"state"`
			} `json:"printers"`
		}
		call := mcpTestCall(t, session, "list_printers", nil, &result)
		raw, err := json.Marshal(call.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), mcpFixtureKey) || strings.Contains(string(raw), `"url"`) || strings.Contains(string(raw), `"host"`) {
			t.Fatalf("list_printers exposed connection credentials or address: %s", raw)
		}
		if len(result.Printers) == 1 {
			p := result.Printers[0]
			if p.ID != mcpFixturePrinter || p.Name != "Fixture OctoPrint" || p.Kind != "octoprint" {
				t.Fatalf("list_printers returned an unexpected printer: %+v", p)
			}
			if p.Online && p.State == "idle" {
				return p.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("list_printers did not report an idle printer: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *mcpPrintFixture) slice(t *testing.T, session *mcp.ClientSession, printer string) *Job {
	t.Helper()
	const model = "solid fixture\nfacet normal 0 0 1\nouter loop\nvertex 5 5 0\nvertex 10 5 0\nvertex 5 10 0\nendloop\nendfacet\nendsolid fixture\n"
	var file struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	}
	mcpTestCall(t, session, "upload_model", map[string]string{
		"name": "fixture.stl", "data": base64.StdEncoding.EncodeToString([]byte(model)),
	}, &file)
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(model))); file.Name != "fixture.stl" || file.SHA256 != want || file.Size != len(model) {
		t.Fatalf("upload_model returned an unexpected reference: %+v", file)
	}
	var accepted JobView
	mcpTestCall(t, session, "slice_plate", map[string]any{
		"library": resolve.Request{Printer: printer, Print: "profile:process", Filaments: []string{"profile:filament"}, Slots: []string{"slot:filament"}},
		"files":   []map[string]string{{"name": file.Name, "sha256": file.SHA256}},
		"output":  "gcode",
	}, &accepted)
	job := f.server.jobs.Get(accepted.ID)
	if job == nil {
		t.Fatalf("slice_plate did not return a submitted job: %+v", accepted)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var view JobView
		mcpTestCall(t, session, "get_slice_job", map[string]string{"job_id": accepted.ID}, &view)
		if view.LifecycleState == StateDone {
			return job
		}
		if view.LifecycleState.Finished() || time.Now().After(deadline) {
			t.Fatalf("slice job ended in %q: %s", view.LifecycleState, view.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type mcpTestPrepared struct {
	Token            string         `json:"token"`
	Digest           string         `json:"digest"`
	Summary          map[string]any `json:"summary"`
	ApprovalRequired bool           `json:"approval_required"`
}

func mcpTestPrepare(t *testing.T, session *mcp.ClientSession, job, printer string) mcpTestPrepared {
	t.Helper()
	var prepared mcpTestPrepared
	mcpTestCall(t, session, "prepare_print", mcpPrepareInput{
		JobID: job, Printer: printer, Filename: mcpFixtureFilename, FlowCalibration: "off",
		BedLeveling: true, VibrationCalibration: true,
	}, &prepared)
	if !prepared.ApprovalRequired || len(prepared.Token) < 16 || len(prepared.Digest) != 64 {
		t.Fatalf("invalid print approval envelope: %+v", prepared)
	}
	return prepared
}

func (p mcpTestPrepared) confirm() mcpConfirmInput {
	return mcpConfirmInput{Token: p.Token, Digest: p.Digest, HumanApproved: true}
}

func (f *mcpPrintFixture) transfer(t *testing.T, job string) (int, Transfer) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.http.URL+"/api/jobs/"+job+"/send", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.http.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var tr Transfer
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, tr
}

func (f *mcpPrintFixture) assertUnsent(t *testing.T, job string) {
	t.Helper()
	if status, transfer := f.transfer(t, job); status != http.StatusNotFound {
		t.Fatalf("unapproved job has a transfer: HTTP %d, %+v", status, transfer)
	}
	select {
	case upload := <-f.uploads:
		t.Fatalf("printer received an unapproved upload: %+v", upload)
	default:
	}
}

func (f *mcpPrintFixture) assertUploaded(t *testing.T, job, transfer string) {
	t.Helper()
	select {
	case upload := <-f.uploads:
		want := mcpTestUpload{mcpFixtureFilename, mcpFixtureGCode, "true", mcpFixtureKey}
		if upload != want {
			t.Fatalf("printer received %+v, want %+v", upload, want)
		}
	case <-time.After(5 * time.Second):
		_, state := f.transfer(t, job)
		t.Fatalf("printer got no upload, transfer %+v", state)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, state := f.transfer(t, job)
		if status == http.StatusOK && state.ID == transfer && state.State == TransferDone {
			break
		}
		if state.State == TransferFailed || time.Now().After(deadline) {
			t.Fatalf("confirmed transfer did not finish: HTTP %d, %+v", status, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case upload := <-f.uploads:
		t.Fatalf("printer received a duplicate upload: %+v", upload)
	default:
	}
}

func TestMCPPrintWorkflow(t *testing.T) {
	f := newMCPPrintFixture(t)
	mcpSaveProjectFixture(t, f.server.workspace)
	defer mcpRequireUnchangedWorkspace(t, f.server.workspace)
	session := f.connect(t, nil)
	printer := f.readyPrinter(t, session)
	job := f.slice(t, session, printer)
	prepared := mcpTestPrepare(t, session, job.ID, printer)
	for key, want := range map[string]any{
		"job_id": job.ID, "printer": printer, "printer_name": "Fixture OctoPrint", "filename": mcpFixtureFilename,
		"artifact_sha256":  fmt.Sprintf("%x", sha256.Sum256([]byte(mcpFixtureGCode))),
		"estimated_time_s": float64(17), "filament_grams": 1.25, "start": true,
		"bed_leveling": true, "vibration_calibration": true, "flow_calibration": "off", "timelapse": false,
	} {
		if got := prepared.Summary[key]; got != want {
			t.Errorf("summary[%q] = %v, want %v", key, got, want)
		}
	}
	if warnings, _ := prepared.Summary["warnings"].([]any); len(warnings) != 1 || warnings[0] != "Fixture warning" {
		t.Errorf("summary warnings = %v", prepared.Summary["warnings"])
	}
	if prepared.Summary["settings"] == nil || prepared.Summary["library"] == nil {
		t.Errorf("summary omitted resolved slicing inputs: %+v", prepared.Summary)
	}
	f.assertUnsent(t, job.ID)
	var transfer Transfer
	mcpTestCall(t, session, "confirm_print", prepared.confirm(), &transfer)
	if transfer.ID == "" || transfer.Filename != mcpFixtureFilename || transfer.Printer != printer || !transfer.Start {
		t.Fatalf("confirm_print returned unexpected transfer: %+v", transfer)
	}
	mcpTestRefusal(t, mcpTestCall(t, session, "confirm_print", prepared.confirm(), nil), "already used")
	f.assertUploaded(t, job.ID, transfer.ID)
}

func TestMCPConfirmationRefusals(t *testing.T) {
	f := newMCPPrintFixture(t)
	session := f.connect(t, nil)
	printer := f.readyPrinter(t, session)
	job := f.slice(t, session, printer)
	for _, tt := range []struct {
		name   string
		error  string
		change func(*testing.T, *mcpConfirmInput)
	}{
		{"missing approval", "approval", func(_ *testing.T, in *mcpConfirmInput) { in.HumanApproved = false }},
		{"digest mismatch", "does not match", func(_ *testing.T, in *mcpConfirmInput) { in.Digest = strings.Repeat("0", 64) }},
		{"expired token", "expired", func(t *testing.T, _ *mcpConfirmInput) {
			old := f.now.Add(int64(2 * time.Minute))
			t.Cleanup(func() { f.now.Store(old - int64(2*time.Minute)) })
		}},
		{"source artifact changed", "artifact mismatch", func(t *testing.T, _ *mcpConfirmInput) {
			path := job.OutputPath("gcode")
			if err := os.WriteFile(path, []byte("G90\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(path, []byte(mcpFixtureGCode), 0600); err != nil {
					t.Error(err)
				}
			})
		}},
		{"slice request changed", "job or printer changed", func(t *testing.T, _ *mcpConfirmInput) {
			job.mu.Lock()
			old := job.Request.Filename
			job.Request.Filename = "other"
			job.mu.Unlock()
			t.Cleanup(func() {
				job.mu.Lock()
				job.Request.Filename = old
				job.mu.Unlock()
			})
		}},
		{"connection changed", "job or printer changed", func(t *testing.T, _ *mcpConfirmInput) {
			sends := f.server.sends
			sends.mu.Lock()
			old := sends.conns
			modified := *old[0]
			modified.Name = "Changed connection"
			sends.conns = []*PrinterConfig{&modified}
			sends.mu.Unlock()
			t.Cleanup(func() {
				sends.mu.Lock()
				sends.conns = old
				sends.mu.Unlock()
			})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prepared := mcpTestPrepare(t, session, job.ID, printer)
			input := prepared.confirm()
			tt.change(t, &input)
			mcpTestRefusal(t, mcpTestCall(t, session, "confirm_print", input, nil), tt.error)
			f.assertUnsent(t, job.ID)
			mcpTestRefusal(t, mcpTestCall(t, session, "confirm_print", prepared.confirm(), nil), "already used")
		})
	}

	t.Run("session mismatch", func(t *testing.T) {
		f.readyPrinter(t, session)
		prepared := mcpTestPrepare(t, session, job.ID, printer)
		other := f.connect(t, nil)
		mcpTestRefusal(t, mcpTestCall(t, other, "confirm_print", prepared.confirm(), nil), "confirmation token")
		f.assertUnsent(t, job.ID)
		input := prepared.confirm()
		input.HumanApproved = false
		mcpTestRefusal(t, mcpTestCall(t, session, "confirm_print", input, nil), "approval")
	})
}

func TestMCPConfirmationElicitation(t *testing.T) {
	f := newMCPPrintFixture(t)
	session := f.connect(t, nil)
	printer := f.readyPrinter(t, session)
	job := f.slice(t, session, printer)

	for _, tt := range []struct {
		name, action, refusal string
		expire                bool
	}{
		{name: "decline", action: "decline", refusal: "elicitation"},
		{name: "cancel", action: "cancel", refusal: "elicitation"},
		{name: "accept with approval unchecked", action: "accept", refusal: "elicitation"},
		{name: "expire during approval", action: "accept", refusal: "expired during approval", expire: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			elicited := make(chan *mcp.ElicitParams, 1)
			now := f.now.Load()
			t.Cleanup(func() { f.now.Store(now) })
			client := f.connect(t, &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				elicited <- r.Params
				result := &mcp.ElicitResult{Action: tt.action}
				if tt.expire {
					f.now.Add(int64(2 * time.Minute))
				}
				if tt.action == "accept" {
					result.Content = map[string]any{"approve": tt.expire}
				}
				return result, nil
			}})
			prepared := mcpTestPrepare(t, client, job.ID, printer)
			mcpTestRefusal(t, mcpTestCall(t, client, "confirm_print", prepared.confirm(), nil), tt.refusal)
			select {
			case params := <-elicited:
				mcpTestApprovalPrompt(t, params, prepared)
			default:
				t.Fatal("confirmation was refused without requesting the advertised human approval")
			}
			mcpTestRefusal(t, mcpTestCall(t, client, "confirm_print", prepared.confirm(), nil), "already used")
			f.assertUnsent(t, job.ID)
		})
	}

	t.Run("accept with concurrent duplicate", func(t *testing.T) {
		elicited := make(chan *mcp.ElicitParams, 1)
		approve := make(chan struct{})
		client := f.connect(t, &mcp.ClientOptions{ElicitationHandler: func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			select {
			case elicited <- r.Params:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case <-approve:
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}})
		prepared := mcpTestPrepare(t, client, job.ID, printer)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		type confirmation struct {
			result *mcp.CallToolResult
			err    error
		}
		completed := make(chan confirmation, 1)
		go func() {
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "confirm_print", Arguments: prepared.confirm()})
			completed <- confirmation{result, err}
		}()
		select {
		case params := <-elicited:
			mcpTestApprovalPrompt(t, params, prepared)
		case early := <-completed:
			t.Fatalf("confirmation returned before human approval: %+v, error %v", early.result, early.err)
		case <-ctx.Done():
			t.Fatal("confirmation did not request human approval")
		}
		f.assertUnsent(t, job.ID)
		mcpTestRefusal(t, mcpTestCall(t, client, "confirm_print", prepared.confirm(), nil), "already used")
		close(approve)
		select {
		case first := <-completed:
			if first.err != nil || first.result.IsError {
				t.Fatalf("approved confirmation failed: %+v, error %v", first.result, first.err)
			}
			var transfer Transfer
			mcpTestDecode(t, first.result.StructuredContent, &transfer)
			f.assertUploaded(t, job.ID, transfer.ID)
		case <-ctx.Done():
			t.Fatal("approved confirmation did not return")
		}
	})
}

func mcpTestApprovalPrompt(t *testing.T, params *mcp.ElicitParams, prepared mcpTestPrepared) {
	t.Helper()
	_, summary, ok := strings.Cut(params.Message, "\n")
	if !ok {
		t.Fatalf("elicitation did not include the prepared summary: %s", params.Message)
	}
	var shown map[string]any
	if err := json.Unmarshal([]byte(summary), &shown); err != nil {
		t.Fatalf("elicitation summary was not valid JSON: %v", err)
	}
	shownJSON, err := json.Marshal(shown)
	if err != nil {
		t.Fatal(err)
	}
	preparedJSON, err := json.Marshal(prepared.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if string(shownJSON) != string(preparedJSON) {
		t.Fatalf("elicitation summary = %s, want %s", shownJSON, preparedJSON)
	}
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	mcpTestDecode(t, params.RequestedSchema, &schema)
	if schema.Properties["approve"].Type != "boolean" || len(schema.Required) != 1 || schema.Required[0] != "approve" {
		t.Fatalf("elicitation did not require a boolean approval: %+v", schema)
	}
}

func TestMCPEndpointAccess(t *testing.T) {
	catalog := mcpFixtureCatalog(t)
	const allowedOrigin = "https://allowed.example"
	for _, tt := range []struct {
		name         string
		enabled      bool
		serverKey    string
		mcpKey       string
		bearer       string
		accessCookie bool
		origins      []string
		want         int
	}{
		{name: "disabled", serverKey: "web", want: http.StatusNotFound},
		{name: "anonymous enabled", enabled: true, want: http.StatusOK},
		{name: "MCP token missing", enabled: true, mcpKey: "mcp", want: http.StatusUnauthorized},
		{name: "MCP token wrong", enabled: true, mcpKey: "mcp", bearer: "wrong", want: http.StatusUnauthorized},
		{name: "MCP token valid", enabled: true, mcpKey: "mcp", bearer: "mcp", want: http.StatusOK},
		{name: "server bearer missing", enabled: true, serverKey: "web", want: http.StatusUnauthorized},
		{name: "server bearer valid", enabled: true, serverKey: "web", bearer: "web", want: http.StatusOK},
		{name: "server access cookie valid", enabled: true, serverKey: "web", accessCookie: true, want: http.StatusOK},
		{name: "shared key valid", enabled: true, serverKey: "shared", mcpKey: "shared", bearer: "shared", want: http.StatusOK},
		{name: "distinct keys require an access cookie", enabled: true, serverKey: "web", mcpKey: "mcp", bearer: "mcp", want: http.StatusUnauthorized},
		{name: "distinct keys require the MCP key", enabled: true, serverKey: "web", mcpKey: "mcp", bearer: "web", accessCookie: true, want: http.StatusUnauthorized},
		{name: "distinct MCP bearer and server cookie", enabled: true, serverKey: "web", mcpKey: "mcp", bearer: "mcp", accessCookie: true, want: http.StatusOK},
		{name: "unlisted origin", enabled: true, origins: []string{"https://unlisted.example"}, want: http.StatusForbidden},
		{name: "origin includes a path", enabled: true, origins: []string{allowedOrigin + "/page"}, want: http.StatusForbidden},
		{name: "multiple origins", enabled: true, origins: []string{allowedOrigin, allowedOrigin}, want: http.StatusForbidden},
		{name: "allowed origin", enabled: true, origins: []string{allowedOrigin}, want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<p>SPA fixture</p>"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{
				JobRoot: filepath.Join(root, "jobs"), DataDir: root, StaticDir: root, Options: cacheResponse([]byte(mcpFixtureOptions)),
				MaxUploadSize: 1 << 20, MCPEnabled: tt.enabled, MCPToken: tt.mcpKey, BearerToken: tt.serverKey, MCPOrigins: []string{allowedOrigin},
			}
			jobs, err := NewJobManager(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(jobs.Shutdown)
			httpServer := httptest.NewServer(NewServer(cfg, jobs, catalog))
			t.Cleanup(httpServer.Close)
			client := httpServer.Client()
			if !tt.enabled {
				res, err := client.Get(httpServer.URL + "/mcp")
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				if res.StatusCode != http.StatusNotFound {
					t.Fatalf("disabled MCP endpoint served HTTP %d for GET", res.StatusCode)
				}
			}

			headers := make(http.Header)
			for _, origin := range tt.origins {
				headers.Add("Origin", origin)
			}
			if tt.bearer != "" {
				headers.Set("Authorization", "Bearer "+tt.bearer)
			}
			if tt.accessCookie {
				res, err := client.Post(httpServer.URL+"/api/access", "application/json", strings.NewReader(fmt.Sprintf(`{"token":%q}`, tt.serverKey)))
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				cookies := res.Cookies()
				if res.StatusCode != http.StatusNoContent || len(cookies) != 1 {
					t.Fatalf("server access cookie not issued: HTTP %d, cookies %v", res.StatusCode, cookies)
				}
				headers.Set("Cookie", cookies[0].Name+"="+cookies[0].Value)
			}
			if tt.want != http.StatusOK {
				body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header = headers.Clone()
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("Content-Type", "application/json")
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if res.StatusCode != tt.want {
					body, _ := io.ReadAll(res.Body)
					t.Fatalf("initialize HTTP %d, want %d: %s", res.StatusCode, tt.want, body)
				}
				return
			}

			closed := make(chan int, 1)
			sdkClient := &http.Client{Transport: mcpTestRoundTripper(func(req *http.Request) (*http.Response, error) {
				req = req.Clone(req.Context())
				for key, values := range headers {
					req.Header[key] = values
				}
				res, err := client.Transport.RoundTrip(req)
				if err == nil && req.Method == http.MethodDelete {
					closed <- res.StatusCode
				}
				return res, err
			})}
			session := mcpTestConnect(t, httpServer, sdkClient, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			registered := map[string]*mcp.ToolAnnotations{}
			for _, tool := range tools.Tools {
				registered[tool.Name] = tool.Annotations
			}
			for name, readOnly := range map[string]bool{"list_printers": true, "slice_plate": false, "prepare_print": false, "confirm_print": false} {
				if annotations := registered[name]; annotations == nil || annotations.ReadOnlyHint != readOnly {
					t.Errorf("tool %q missing or has wrong read-only annotation: %+v", name, annotations)
				}
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case status := <-closed:
				if status < 200 || status >= 300 {
					t.Fatalf("session shutdown HTTP %d", status)
				}
			default:
				t.Fatal("session did not issue an HTTP DELETE")
			}
		})
	}
}
