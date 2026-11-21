package main

import (
	"archive/zip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

func mcpTestPackage(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, body := range entries {
		entry, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPPrintPackageSinglePlate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    string
	}{
		{"single", map[string]string{"Metadata/plate_2.gcode": "G28"}, "Metadata/plate_2.gcode"},
		{"empty", map[string]string{"Metadata/slice_info.config": "<config/>"}, ""},
		{"multiple", map[string]string{"Metadata/plate_1.gcode": "G28", "Metadata/plate_2.gcode": "G28"}, ""},
		{"duplicate", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slice.gcode.3mf")
			if tc.name == "duplicate" {
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				z := zip.NewWriter(f)
				for range 2 {
					if _, err := z.Create("Metadata/plate_1.gcode"); err != nil {
						t.Fatal(err)
					}
				}
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				mcpTestPackage(t, path, tc.entries)
			}
			got, err := mcpPackagePlate(path)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ambiguous package accepted as %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("plate = %q, error = %v", got, err)
			}
		})
	}
}

func newMCPBambuFixture(t *testing.T) (*Server, *Job, PrinterStatus) {
	t.Helper()
	root := t.TempDir()
	cfg := Config{Resources: root, JobRoot: filepath.Join(root, "jobs"), MCPEnabled: true, MCPConfirmationTTL: time.Minute}
	jobs, err := NewJobManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jobs.Shutdown)
	catalog := mcpFixtureCatalog(t)
	doc := emptyLibrary("")
	doc["printers"] = json.RawMessage(`[{"id":"profile:printer","name":"H2D fixture","revision":1,"vendor":"BBL","model":"Fixture","nozzles":[{"variant":"Printer","diameter":0.4}],"swap":"ams","slots":[{"id":"slot:filament","filament":"profile:filament","tray":"ams0.0"}]}]`)
	doc["processes"] = json.RawMessage(`[{"id":"profile:process","name":"Process","revision":1,"product":"Process"}]`)
	doc["filaments"] = json.RawMessage(`[{"id":"profile:filament","name":"Filament","revision":1,"product":"Filament"}]`)
	library := &libraryStore{dir: root, catalog: catalog, settled: true}
	if err := library.write(&libraryEnvelope{Document: doc, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	p := &PrinterConfig{
		Printer: mcpFixturePrinter, Kind: "bambu-lan", Name: "H2D fixture", model: "H2D",
		Host: "localhost", hosts: &hostPolicy{ports: []uint16{8883}}, CredentialFile: filepath.Join(root, "access-code"),
	}
	if err := os.WriteFile(p.CredentialFile, []byte(mcpFixtureKey), 0600); err != nil {
		t.Fatal(err)
	}
	sends := NewSendManager(nil)
	sends.store = &printerStore{}
	sends.conns = []*PrinterConfig{p}
	j, err := jobs.NewJob("fixture")
	if err != nil {
		t.Fatal(err)
	}
	j.Request = JobRequest{Output: "gcode.3mf", Library: &resolve.Request{Printer: p.Printer, Print: "profile:process", Filaments: []string{"profile:filament"}, Slots: []string{"slot:filament"}}}
	mcpTestPackage(t, j.OutputPath("gcode.3mf"), map[string]string{
		"Metadata/plate_1.gcode":         "G28\n",
		"Metadata/model_settings.config": `<config><plate><metadata key="filament_maps" value="1"/></plate></config>`,
		"Metadata/slice_info.config":     `<config><plate><filament id="1" type="PLA" color="#123abc" nozzle_diameter="0.4" volume_type="Standard" used_for_object="true"/></plate></config>`,
	})
	j.state = StateDone
	jobs.mu.Lock()
	delete(jobs.uploads, j.ID)
	jobs.jobs[j.ID] = j
	jobs.mu.Unlock()
	status := PrinterStatus{
		Printer: p.Printer, Online: true, State: "idle", At: time.Now(),
		Trays: []AMSTray{{AMS: 0, ID: 0, Type: "PLA", Color: "#123abc", Extruder: ptr(0)}},
		Nozzles: []NozzleStatus{
			{ID: 0, Extruder: 0, Diameter: 0.4, Type: "stainless_steel", Flow: "Standard"},
			{ID: 1, Extruder: 1, Diameter: 0.4, Type: "stainless_steel", Flow: "Standard"},
		},
	}
	sends.hub.subs = 1
	sends.hub.monitors[p.Printer] = &monitor{cfg: p, cancel: func() {}, poke: make(chan struct{}, 1), link: LinkOnline}
	sends.hub.status[p.Printer] = status
	return &Server{cfg: cfg, catalog: catalog, library: library, jobs: jobs, sends: sends}, j, status
}

func TestMCPBambuValidationPreservesTopology(t *testing.T) {
	s, j, status := newMCPBambuFixture(t)
	lib, err := s.libraryDocument()
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := readBambuJobMapping(j.OutputPath("gcode.3mf"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.printerSendPlan(lib, sendPlanInput{Source: j.Request.Library, TargetID: status.Printer, Sliced: mapping, TargetLive: status})
	if err != nil {
		t.Fatal(err)
	}
	body := sendRequest{Printer: status.Printer, Start: ptr(true), FlowCaliMode: json.RawMessage(`"auto"`)}
	for _, slot := range plan.Slots {
		body.ExpectedPlan = append(body.ExpectedPlan, slot.Proof)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*PrinterStatus)
		allowed bool
	}{
		{"ready", func(*PrinterStatus) {}, true},
		{"nozzle changed", func(s *PrinterStatus) { s.Nozzles[0].Diameter = 0.6 }, false},
		{"tray changed", func(s *PrinterStatus) { s.Trays[0].Type = "PETG" }, false},
		{"routing changed", func(s *PrinterStatus) { s.Trays[0].Extruder = ptr(1) }, false},
		{"offline", func(s *PrinterStatus) { s.Online = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := status
			current.Nozzles = append([]NozzleStatus(nil), status.Nozzles...)
			current.Trays = append([]AMSTray(nil), status.Trays...)
			tc.mutate(&current)
			s.sends.hub.status[status.Printer] = current
			r := httptest.NewRequest(http.MethodPost, "/api/jobs/"+j.ID+"/send", nil)
			w := httptest.NewRecorder()
			validated := s.validateSend(w, r, j, body, j.OutputPath("gcode.3mf"), false)
			if (validated != nil) != tc.allowed {
				t.Fatalf("validation = %v, response %d %s", validated, w.Code, w.Body)
			}
			if j.transferView() != nil || len(s.sends.buckets) != 0 {
				t.Fatal("validation launched a transfer")
			}
		})
	}
}

func TestMCPBambuConfirmationRefusesChangedNozzle(t *testing.T) {
	s, job, status := newMCPBambuFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.mcpHandler())
	mux.HandleFunc("GET /api/jobs/{id}/send", s.getSend)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fixture := &mcpPrintFixture{http: server}
	session := fixture.connect(t, nil)
	hub := s.sends.hub
	mon := hub.monitors[status.Printer]

	for _, tc := range []struct {
		name   string
		change func(*NozzleStatus)
	}{
		{"type", func(n *NozzleStatus) { n.Type = "hardened_steel" }},
		{"id", func(n *NozzleStatus) { n.ID = 2 }},
		{"abnormal status", func(n *NozzleStatus) { n.Abnormal = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub.set(mon, status)
			var prepared mcpTestPrepared
			mcpTestCall(t, session, "prepare_print", mcpPrepareInput{
				JobID: job.ID, Printer: status.Printer, Filename: "approved.gcode.3mf", FlowCalibration: "off",
			}, &prepared)
			if !prepared.ApprovalRequired || prepared.Token == "" || prepared.Digest == "" {
				t.Fatalf("prepare_print returned an invalid approval: %+v", prepared)
			}
			changed := status
			changed.Nozzles = append([]NozzleStatus(nil), status.Nozzles...)
			tc.change(&changed.Nozzles[0])
			hub.set(mon, changed)
			mcpTestRefusal(t, mcpTestCall(t, session, "confirm_print", prepared.confirm(), nil), "nozzle topology changed")
			if code, transfer := fixture.transfer(t, job.ID); code != http.StatusNotFound {
				t.Fatalf("changed nozzle resulted in a transfer: HTTP %d, %+v", code, transfer)
			}
		})
	}
}
