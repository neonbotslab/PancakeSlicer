package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

const printerContextFixture = `{"ams_slots":[4,0],"loaded_filaments":[{"extruder":1,"type":"PETG","color":"#123abc","external":true}]}`

func TestPrinterFilamentContextJobArguments(t *testing.T) {
	var job Job
	job.Dir = t.TempDir()
	if err := json.Unmarshal([]byte(`{"output":"gcode.3mf","files":["model.stl"],"printer_filament_context":`+printerContextFixture+`}`), &job.Request); err != nil {
		t.Fatal(err)
	}
	manager := &JobManager{}
	args := manager.toolArgs(&job)
	i := slices.Index(args, "--printer-filament-context")
	if i < 0 || i+1 >= len(args) || args[i+1] != filepath.Join(job.Dir, "input", "printer-filament-context.json") {
		t.Fatalf("missing printer filament context in job arguments: %v", args)
	}
}

func TestPrinterFilamentContextRejectsInvalidInventory(t *testing.T) {
	for name, body := range map[string]string{
		"empty context":       "",
		"null context":        "null",
		"missing inventory":   "{}",
		"extra capacity":      strings.Replace(printerContextFixture, "4,0", "4,0,0", 1),
		"null capacity":       strings.Replace(printerContextFixture, "4,0", "4,null", 1),
		"oversized capacity":  strings.Replace(printerContextFixture, "4,0", "65,0", 1),
		"unknown side":        strings.Replace(printerContextFixture, `"extruder":1`, `"extruder":2`, 1),
		"unspecified side":    strings.Replace(printerContextFixture, `"extruder":1,`, "", 1),
		"unspecified routing": strings.Replace(printerContextFixture, `,"external":true`, "", 1),
		"blank material":      strings.Replace(printerContextFixture, "PETG", "   ", 1),
		"invalid color":       strings.Replace(printerContextFixture, "#123abc", "#1234", 1),
		"unfitted AMS":        strings.Replace(printerContextFixture, `"external":true`, `"external":false`, 1),
		"repeated external":   strings.Replace(printerContextFixture, `}]`, `},{"extruder":1,"type":"PLA","color":"#333333","external":true}]`, 1),
		"extra printer data":  strings.Replace(printerContextFixture, `{"ams_slots"`, `{"printer":"fixture","ams_slots"`, 1),
		"extra tray data":     strings.Replace(printerContextFixture, `"type":"PETG"`, `"type":"PETG","name":"fixture"`, 1),
		"trailing input":      printerContextFixture + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePrinterFilamentContext(body); err == nil {
				t.Fatal("invalid printer filament context was accepted")
			}
		})
	}
}

type sliceUploadRecorder struct{ *httptest.ResponseRecorder }

func (sliceUploadRecorder) SetReadDeadline(time.Time) error { return nil }

func TestPrinterFilamentContextMultipart(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const options = `{
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
	if err := configureResolve([]byte(options)); err != nil {
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
	doc := emptyLibrary("")
	doc["printers"] = json.RawMessage(`[{"id":"profile:printer","name":"Printer","revision":1,"vendor":"BBL","model":"Fixture","nozzles":[{"variant":"Printer","diameter":0.4}],"swap":"manual","slots":[{"id":"slot:filament","filament":"profile:filament"}]}]`)
	doc["processes"] = json.RawMessage(`[{"id":"profile:process","name":"Process","revision":1,"product":"Process"}]`)
	doc["filaments"] = json.RawMessage(`[{"id":"profile:filament","name":"Filament","revision":1,"product":"Filament"}]`)
	library := &libraryStore{dir: root, catalog: catalog, settled: true}
	if err := library.write(&libraryEnvelope{Document: doc, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	blobs := &blobStore{root: filepath.Join(root, "blobs")}
	const model = "solid fixture\nfacet normal 0 0 1\nouter loop\nvertex 5 5 0\nvertex 10 5 0\nvertex 5 10 0\nendloop\nendfacet\nendsolid fixture\n"
	sum := sha256.Sum256([]byte(model))
	digest := hex.EncodeToString(sum[:])
	write(blobs.path(digest), model)
	manager := &JobManager{cfg: Config{JobRoot: filepath.Join(root, "jobs"), MaxQueue: -1}, jobs: map[string]*Job{}, uploads: map[string]*Job{}}
	server := &Server{cfg: Config{MaxUploadSize: 1 << 20}, jobs: manager, catalog: catalog, library: library, blobs: blobs}
	submit := func(context ...string) *httptest.ResponseRecorder {
		t.Helper()
		var body strings.Builder
		form := multipart.NewWriter(&body)
		for key, value := range map[string]string{
			"library": `{"printer":"profile:printer","print":"profile:process","filaments":["profile:filament"],"slots":["slot:filament"]}`, "output": "gcode.3mf",
			"files": `[{"name":"fixture.stl","sha256":"` + digest + `"}]`,
		} {
			if err := form.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
		for _, raw := range context {
			if err := form.WriteField("printer_filament_context", raw); err != nil {
				t.Fatal(err)
			}
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body.String()))
		request.Header.Set("Content-Type", form.FormDataContentType())
		recorder := httptest.NewRecorder()
		server.createJob(sliceUploadRecorder{recorder}, request)
		return recorder
	}
	if response := submit(printerContextFixture); response.Code != 400 || !strings.Contains(response.Body.String(), "requires a slicing engine") {
		t.Fatalf("unsupported engine accepted inventory: %d %s", response.Code, response.Body)
	}
	if response := submit(); response.Code != 202 {
		t.Fatalf("ordinary slice failed on old engine: %d %s", response.Code, response.Body)
	}
	server.cfg.Engine.PrinterFilamentContext = true
	if response := submit(""); response.Code != 400 {
		t.Fatalf("empty inventory was accepted: %d %s", response.Code, response.Body)
	}
	if response := submit(printerContextFixture, printerContextFixture); response.Code != 400 {
		t.Fatalf("ambiguous inventory was accepted: %d %s", response.Code, response.Body)
	}
	response := submit(printerContextFixture)
	if response.Code != 202 {
		t.Fatalf("valid inventory was rejected: %d %s", response.Code, response.Body)
	}
	job := manager.Get(filepath.Base(response.Header().Get("Location")))
	if job == nil || job.Request.PrinterFilamentContext == nil {
		t.Fatal("accepted job lost the printer filament context")
	}
	data, err := os.ReadFile(filepath.Join(job.Dir, "input", printerFilamentContextFile))
	if err != nil || string(data) != strings.Replace(printerContextFixture, "#123abc", "#123ABC", 1) {
		t.Fatalf("unexpected printer filament input %s, error %v", data, err)
	}
	fingerprint := func(raw string) string {
		t.Helper()
		result := submit(raw)
		if result.Code != 202 {
			t.Fatalf("valid inventory was rejected: %d %s", result.Code, result.Body)
		}
		accepted := manager.Get(filepath.Base(result.Header().Get("Location")))
		if accepted == nil || accepted.Request.Submission == nil || accepted.Request.Submission.ConfigSHA256 == "" {
			t.Fatal("accepted slice lacked a submission fingerprint")
		}
		return accepted.Request.Submission.ConfigSHA256
	}
	if same := fingerprint(string(data)); same != job.Request.Submission.ConfigSHA256 {
		t.Fatal("canonical inventory changed the submission fingerprint")
	}
	if changed := fingerprint(strings.Replace(printerContextFixture, "#123abc", "#FFFFFF", 1)); changed == job.Request.Submission.ConfigSHA256 {
		t.Fatal("changed inventory reused the submission fingerprint")
	}
}

func TestPrinterFilamentContextIsPartOfCacheAndRetryInputs(t *testing.T) {
	manager := &JobManager{cfg: Config{JobRoot: t.TempDir(), MaxQueue: -1}, jobs: map[string]*Job{}, uploads: map[string]*Job{}}
	cache, err := NewResultCache(t.TempDir(), 1<<20, "fixture-engine")
	if err != nil {
		t.Fatal(err)
	}
	const model = "model.stl"
	makeJob := func(raw string) *Job {
		t.Helper()
		context, err := parsePrinterFilamentContext(raw)
		if err != nil {
			t.Fatal(err)
		}
		job, err := manager.NewJob("fixture")
		if err != nil {
			t.Fatal(err)
		}
		job.Request = JobRequest{Output: "gcode.3mf", Files: []string{model}, PrinterFilamentContext: context}
		input := filepath.Join(job.Dir, "input")
		if err := os.WriteFile(filepath.Join(input, model), []byte("fixture model"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := context.write(input); err != nil {
			t.Fatal(err)
		}
		return job
	}
	key := func(job *Job) string {
		t.Helper()
		value, err := cache.Key(filepath.Join(job.Dir, "input"), []byte("fixture config"))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	const ams = `{"extruder":0,"type":"PLA","color":"#112233","external":false}`
	const external = `{"extruder":1,"type":"PETG","color":"#123abc","external":true}`
	const first = `{"ams_slots":[4,0],"loaded_filaments":[` + external + `,` + ams + `]}`
	parent := makeJob(first)
	same := makeJob(`{"ams_slots":[4,0],"loaded_filaments":[` + ams + `,` + strings.Replace(external, "#123abc", "#123ABC", 1) + `]}`)
	changed := makeJob(strings.Replace(first, "#123abc", "#FFFFFF", 1))
	if key(parent) != key(same) || key(parent) == key(changed) {
		t.Fatal("cache identity did not track canonical printer filament context")
	}
	parent.state = StateDone
	manager.jobs[parent.ID] = parent
	retry, err := manager.Retry(parent, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if retry.Request.PrinterFilamentContext == nil || key(retry) != key(parent) {
		t.Fatal("retry changed the original printer filament context")
	}
}
