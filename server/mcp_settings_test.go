package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpSettingsProcess  = "profile:process"
	mcpSettingsFilament = "profile:filament"
	mcpSettingsSlot     = "slot:second"
)

type mcpSettingsFixture struct {
	server *Server
	http   *httptest.Server
}

func newMCPSettingsFixture(t *testing.T) *mcpSettingsFixture {
	t.Helper()
	options := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(mcpFixtureOptions), &options); err != nil {
		t.Fatal(err)
	}
	options["layer_height"] = json.RawMessage(`{"type":"Float","profile":"process","min":0.05,"max":0.4}`)
	options["sparse_infill_pattern"] = json.RawMessage(`{"type":"Enum","profile":"process","enum_values":["grid","gyroid"]}`)
	options["nozzle_temperature"] = json.RawMessage(`{"type":"Ints","profile":"filament","min":0,"max":350}`)
	dictionary := must(json.Marshal(options))
	if err := configureResolve(dictionary); err != nil {
		t.Fatal(err)
	}
	const header = `{"schema":"pancake.catalog.v1","version":"mcp-settings-test"}` + "\n"
	catalog, err := resolve.LoadCatalog(
		strings.NewReader(header+`{"id":"Bambu Lab X1 Carbon","vendor":"BBL","base":{"nozzle_diameter":"0.4","printable_area":"0x0,100x0,100x100,0x100","printable_height":"100"},"variants":[{"id":"Printer","nozzle":"0.4","extruders":1}]}`),
		strings.NewReader(header+`{"id":"Filament","base":{"filament_type":"PLA","nozzle_temperature":"210"}}`),
		strings.NewReader(header+`{"id":"Process","base":{"layer_height":"0.2","sparse_infill_pattern":"grid"}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	f := startMCPSettingsFixture(t, t.TempDir(), catalog, &cachedResponse{plain: dictionary})
	doc := emptyLibrary("settings-fixture")
	doc["printers"] = json.RawMessage(`[{"id":"profile:printer","name":"Printer","revision":1,"vendor":"BBL","model":"Bambu Lab X1 Carbon","nozzles":[{"variant":"Printer","diameter":0.4}],"swap":"ams","units":[{"id":0,"nozzle":0}],"slots":[{"id":"slot:first","filament":"profile:filament","colour":"#010203","tray":"ams0.0"},{"id":"slot:second","filament":"","tray":"ams0.1"}],"machine":{"printable_height":100}}]`)
	doc["processes"] = json.RawMessage(`[{"id":"profile:process","name":"Process","revision":1,"product":"Process","values":{"layer_height":0.2,"sparse_infill_pattern":"grid"}}]`)
	doc["filaments"] = json.RawMessage(`[{"id":"profile:filament","name":"Filament","revision":1,"product":"Filament","settings":{"nozzle_temperature":[210]}}]`)
	doc["calibrations"] = json.RawMessage(`[{"id":"calibration:original","revision":1,"filament":"profile:filament","printer":"profile:printer","extruder":0,"nozzle":0.4,"values":{"filament_flow_ratio":0.97}}]`)
	if err := f.server.library.write(&libraryEnvelope{Document: doc, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func startMCPSettingsFixture(t *testing.T, root string, catalog *resolve.Catalog, options *cachedResponse) *mcpSettingsFixture {
	t.Helper()
	s := &Server{
		cfg: Config{MCPEnabled: true, Options: options}, catalog: catalog,
		library:   &libraryStore{dir: root, catalog: catalog, settled: true, swap: catalog.Swap, extruders: func(string) int { return 1 }},
		workspace: &workspaceStore{root: filepath.Join(root, "workspace")},
		sends:     NewSendManager(nil),
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", newMCPService(s).handler())
	mux.HandleFunc("GET /api/events", s.liveEvents)
	s.libraryRoutes(mux)
	s.workspace.register(mux)
	f := &mcpSettingsFixture{server: s, http: httptest.NewServer(mux)}
	t.Cleanup(f.http.Close)
	return f
}

func (f *mcpSettingsFixture) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return mcpTestConnect(t, f.http, f.http.Client(), nil)
}

type mcpSettingsResult struct {
	Profile     map[string]json.RawMessage `json:"profile"`
	Preferences map[string]json.RawMessage `json:"preferences"`
	Defaults    struct {
		Process string         `json:"process"`
		Slots   []resolve.Slot `json:"slots"`
	} `json:"defaults"`
	ProfileID           string           `json:"profile_id"`
	Revision            string           `json:"revision"`
	RevisionID          string           `json:"revision_id"`
	LibraryRevision     uint64           `json:"library_revision"`
	PreferencesRevision uint64           `json:"preferences_revision"`
	DryRun              bool             `json:"dry_run"`
	Diff                []settingsChange `json:"diff"`
}

func mcpSettingsCall(t *testing.T, s *mcp.ClientSession, name string, args any) mcpSettingsResult {
	t.Helper()
	var result mcpSettingsResult
	mcpTestCall(t, s, name, args, &result)
	return result
}

type mcpSettingsHistory struct {
	Revision string `json:"revision"`
	History  []struct {
		ID       string                     `json:"id"`
		Revision uint64                     `json:"revision"`
		Snapshot map[string]json.RawMessage `json:"snapshot"`
		Diff     []settingsChange           `json:"diff"`
	} `json:"history"`
}

func mcpSettingsReadHistory(t *testing.T, s *mcp.ClientSession, target map[string]any) mcpSettingsHistory {
	t.Helper()
	var result mcpSettingsHistory
	mcpTestCall(t, s, "list_settings_history", target, &result)
	return result
}

func mcpSettingsDocument(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode settings object %s: %v", raw, err)
	}
	return doc
}

func mcpSettingsFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return raw
}

func mcpSettingsUnchanged(t *testing.T, path string, before []byte, changed <-chan struct{}) {
	t.Helper()
	if !bytes.Equal(before, mcpSettingsFile(t, path)) {
		t.Fatalf("%s changed after preview or refusal", filepath.Base(path))
	}
	select {
	case <-changed:
		t.Fatalf("%s published an event after preview or refusal", filepath.Base(path))
	default:
	}
}

func mcpSettingsHasDiff(diff []settingsChange, path string, before, after any) bool {
	return slices.ContainsFunc(diff, func(change settingsChange) bool {
		return change.Path == path && reflect.DeepEqual(change.Before, before) && reflect.DeepEqual(change.After, after)
	})
}

func mcpSettingsCheckCalibrations(t *testing.T, f *mcpSettingsFixture, field, sourceID string, saved, preview mcpSettingsResult) {
	t.Helper()
	var calibrations []map[string]json.RawMessage
	mcpTestDecode(t, mcpSettingsLibraryDocument(t, f)["calibrations"], &calibrations)
	var sources, copies []map[string]json.RawMessage
	for _, cal := range calibrations {
		switch stringValue(cal[field]) {
		case sourceID:
			sources = append(sources, cal)
		case saved.ProfileID:
			copies = append(copies, cal)
		}
	}
	if len(sources) == 0 || len(sources) != len(copies) {
		t.Fatalf("save-as kept %d source calibrations and added %d copies: %v", len(sources), len(copies), calibrations)
	}
	for _, cal := range copies {
		id := stringValue(cal["id"])
		if !strings.HasPrefix(id, "calibration:") || !slices.ContainsFunc(sources, func(source map[string]json.RawMessage) bool {
			if stringValue(source["id"]) == id {
				return false
			}
			for _, key := range []string{"filament", "printer", "extruder", "nozzle", "volume", "values"} {
				if key != field && !bytes.Equal(source[key], cal[key]) {
					return false
				}
			}
			return true
		}) {
			t.Fatalf("copied calibration %v has no matching source in %v", cal, sources)
		}
		if !mcpSettingsHasDiff(saved.Diff, "/calibration/"+id, nil, settingsJSON(cal)) {
			t.Fatalf("save-as omitted calibration %q from the diff: %v", id, saved.Diff)
		}
	}
	previewCount := 0
	for _, change := range preview.Diff {
		if strings.HasPrefix(change.Path, "/calibration/") {
			cal, ok := change.After.(map[string]any)
			if !ok || change.Before != nil || cal[field] != preview.ProfileID {
				t.Fatalf("save-as preview returned an incorrect calibration: %+v", change)
			}
			previewCount++
		}
	}
	if previewCount != len(sources) {
		t.Fatalf("save-as preview returned %d calibration additions, want %d: %v", previewCount, len(sources), preview.Diff)
	}
}

func TestMCPSettingsProfiles(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	path := filepath.Join(f.server.library.dir, libraryFile)
	for _, tc := range []struct {
		kind, id, section, key, before, after string
		newValue, diffBefore, diffAfter       any
	}{
		{"process", mcpSettingsProcess, "values", "layer_height", "0.2", "0.25", 0.25, 0.2, 0.25},
		{"printer", mcpFixturePrinter, "machine", "printable_height", "100", "125", 125, float64(100), float64(125)},
		{"filament", mcpSettingsFilament, "settings", "nozzle_temperature", "[210]", "[215]", []int{215}, []any{float64(210)}, []any{float64(215)}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			target := map[string]any{"kind": tc.kind, "profile_id": tc.id}
			assertOption := func(profile map[string]json.RawMessage, want string) {
				t.Helper()
				if got := string(mcpSettingsDocument(t, profile[tc.section])[tc.key]); got != want {
					t.Fatalf("%s.%s = %s, want %s", tc.section, tc.key, got, want)
				}
			}
			original := mcpSettingsCall(t, s, "get_profile", target)
			if original.Revision == "" || stringValue(original.Profile["id"]) != tc.id {
				t.Fatalf("get_profile returned %+v", original)
			}
			assertOption(original.Profile, tc.before)

			disk, changed := mcpSettingsFile(t, path), f.server.library.wait()
			update := maps.Clone(target)
			update["expected_revision"], update["changes"], update["dry_run"] = original.Revision, map[string]any{tc.key: tc.newValue}, true
			preview := mcpSettingsCall(t, s, "update_profile", update)
			assertOption(preview.Profile, tc.after)
			optionPath := "/" + tc.kind + "/" + tc.id + "/" + tc.section + "/" + tc.key
			if !preview.DryRun || preview.Revision != original.Revision ||
				!mcpSettingsHasDiff(preview.Diff, optionPath, tc.diffBefore, tc.diffAfter) {
				t.Fatalf("update preview lacks the option diff: %+v", preview)
			}
			mcpSettingsUnchanged(t, path, disk, changed)

			update["dry_run"] = false
			saved := mcpSettingsCall(t, s, "update_profile", update)
			assertOption(saved.Profile, tc.after)
			if saved.DryRun || saved.Revision == original.Revision || saved.LibraryRevision != original.LibraryRevision+1 {
				t.Fatalf("save returned an incorrect revision: %+v", saved)
			}
			select {
			case <-changed:
			default:
				t.Fatal("update did not publish a library event")
			}
			stored := mcpSettingsCall(t, s, "get_profile", target)
			assertOption(stored.Profile, tc.after)
			if stored.Revision != saved.Revision {
				t.Fatalf("read revision %q differs from save revision %q", stored.Revision, saved.Revision)
			}
			disk, changed = mcpSettingsFile(t, path), f.server.library.wait()
			stale := mcpTestCall(t, s, "update_profile", update, nil)
			mcpTestRefusal(t, stale, "revision_conflict")
			if !strings.Contains(mcpTestResultText(stale), saved.Revision) {
				t.Fatalf("stale write did not identify current revision %q: %s", saved.Revision, mcpTestResultText(stale))
			}
			mcpSettingsUnchanged(t, path, disk, changed)

			rename := maps.Clone(target)
			rename["expected_revision"], rename["name"] = saved.Revision, "Renamed "+tc.kind
			renamed := mcpSettingsCall(t, s, "rename_profile", rename)
			if renamed.ProfileID != tc.id || stringValue(renamed.Profile["name"]) != rename["name"] {
				t.Fatalf("rename changed the ID or kept the old name: %+v", renamed)
			}

			copyArgs := maps.Clone(target)
			copyArgs["expected_revision"], copyArgs["name"], copyArgs["dry_run"] = renamed.Revision, "Copied "+tc.kind, true
			disk, changed = mcpSettingsFile(t, path), f.server.library.wait()
			preview = mcpSettingsCall(t, s, "save_profile_as", copyArgs)
			if !preview.DryRun || preview.Revision != renamed.Revision ||
				!slices.ContainsFunc(preview.Diff, func(change settingsChange) bool {
					return change.Path == "/"+tc.kind+"/"+preview.ProfileID && change.Before == nil && change.After != nil
				}) {
				t.Fatalf("save-as preview omitted the new profile diff: %+v", preview)
			}
			mcpSettingsUnchanged(t, path, disk, changed)
			copyArgs["dry_run"] = false
			copied := mcpSettingsCall(t, s, "save_profile_as", copyArgs)
			assertOption(copied.Profile, tc.after)
			if copied.ProfileID == "" || copied.ProfileID == tc.id || stringValue(copied.Profile["name"]) != copyArgs["name"] {
				t.Fatalf("save-as returned an incorrect copy: %+v", copied)
			}
			if source := mcpSettingsCall(t, s, "get_profile", target); !reflect.DeepEqual(source.Profile, renamed.Profile) {
				t.Fatalf("save-as changed the source: %+v", source.Profile)
			}
			if tc.kind != "process" {
				mcpSettingsCheckCalibrations(t, f, tc.kind, tc.id, copied, preview)
			}
			if tc.kind == "printer" {
				var old, fresh []resolve.Slot
				mcpTestDecode(t, renamed.Profile["slots"], &old)
				mcpTestDecode(t, copied.Profile["slots"], &fresh)
				if len(old) != 2 || len(fresh) != len(old) {
					t.Fatalf("copy changed the slot count: %v -> %v", old, fresh)
				}
				for i, slot := range fresh {
					if !strings.HasPrefix(slot.ID, "slot:") || slices.ContainsFunc(old, func(source resolve.Slot) bool { return source.ID == slot.ID }) {
						t.Fatalf("copy reused a slot ID: %v -> %v", old, fresh)
					}
					slot.ID = old[i].ID
					if slot != old[i] {
						t.Fatalf("copy changed the slot assignment: %v -> %v", old[i], fresh[i])
					}
				}
			}

			history := mcpSettingsReadHistory(t, s, target)
			if history.Revision != copied.Revision || len(history.History) != 3 {
				t.Fatalf("unexpected source history after update, rename, and copy: %+v", history)
			}
			first := history.History[0]
			if first.ID == "" || first.Revision != 1 || stringValue(first.Snapshot["name"]) != stringValue(original.Profile["name"]) {
				t.Fatalf("history lost the original profile: %+v", first)
			}
			assertOption(first.Snapshot, tc.before)
			assertOption(history.History[1].Snapshot, tc.after)

			revert := maps.Clone(target)
			revert["expected_revision"], revert["revision_id"], revert["dry_run"] = copied.Revision, first.ID, true
			disk, changed = mcpSettingsFile(t, path), f.server.library.wait()
			preview = mcpSettingsCall(t, s, "revert_settings", revert)
			assertOption(preview.Profile, tc.before)
			if !preview.DryRun || !mcpSettingsHasDiff(preview.Diff, optionPath, tc.diffAfter, tc.diffBefore) {
				t.Fatalf("revert preview lacks the restored option: %+v", preview)
			}
			mcpSettingsUnchanged(t, path, disk, changed)

			revert["dry_run"] = false
			restored := mcpSettingsCall(t, s, "revert_settings", revert)
			stored = mcpSettingsCall(t, s, "get_profile", target)
			assertOption(stored.Profile, tc.before)
			if restored.ProfileID != tc.id || restored.Revision != stored.Revision || stringValue(stored.Profile["name"]) != stringValue(original.Profile["name"]) {
				t.Fatalf("revert did not restore the original profile: %+v", stored)
			}
			history = mcpSettingsReadHistory(t, s, target)
			if len(history.History) != 4 || history.History[3].Revision != 4 || stored.RevisionID != history.History[3].ID {
				t.Fatalf("restore did not append a revision: %+v", history)
			}
			copyRead := mcpSettingsCall(t, s, "get_profile", map[string]any{"kind": tc.kind, "profile_id": copied.ProfileID})
			if !reflect.DeepEqual(copyRead.Profile, copied.Profile) {
				t.Fatalf("revert changed the saved copy: %+v", copyRead)
			}
		})
	}
}

func TestMCPSettingsRejectInvalidChanges(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	path := filepath.Join(f.server.library.dir, libraryFile)
	process := mcpSettingsCall(t, s, "get_profile", map[string]any{"kind": "process", "profile_id": mcpSettingsProcess})
	for _, tc := range []struct {
		name, key, want string
		value           any
	}{
		{"type", "layer_height", "expected a number", "wide"},
		{"range", "layer_height", "at most", 2},
		{"enum", "sparse_infill_pattern", "enum values", "unknown"},
		{"unknown", "made_up_setting", "unknown setting", 1},
		{"blocked", "printhost_apikey", "blocked", "sentinel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk, changed := mcpSettingsFile(t, path), f.server.library.wait()
			result := mcpTestCall(t, s, "update_profile", map[string]any{
				"kind": "process", "profile_id": mcpSettingsProcess, "expected_revision": process.Revision,
				"changes": map[string]any{tc.key: tc.value},
			}, nil)
			mcpTestRefusal(t, result, tc.want)
			mcpSettingsUnchanged(t, path, disk, changed)
		})
	}
}

func TestMCPSettingsPrinterDefaults(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	target := map[string]any{"printer_id": mcpFixturePrinter}
	original := mcpSettingsCall(t, s, "get_printer_defaults", target)
	if original.Defaults.Process != "" || len(original.Defaults.Slots) != 2 || original.Defaults.Slots[1].ID != mcpSettingsSlot {
		t.Fatalf("unexpected initial printer defaults: %+v", original)
	}
	path := filepath.Join(f.server.library.dir, libraryFile)
	disk, changed := mcpSettingsFile(t, path), f.server.library.wait()
	args := maps.Clone(target)
	args["expected_revision"] = original.Revision
	args["process_id"] = mcpSettingsProcess
	args["slots"] = []map[string]any{{"slot_id": "slot:missing", "filament_id": mcpSettingsFilament}}
	mcpTestRefusal(t, mcpTestCall(t, s, "update_printer_defaults", args, nil), "unknown stable slot_id")
	mcpSettingsUnchanged(t, path, disk, changed)
	args["slots"] = []map[string]any{{"slot_id": mcpSettingsSlot, "filament_id": mcpSettingsFilament, "colour": "#102030", "tray": "ams0.3"}}
	args["dry_run"] = true
	preview := mcpSettingsCall(t, s, "update_printer_defaults", args)
	if !preview.DryRun || len(preview.Diff) == 0 {
		t.Fatalf("printer defaults preview omitted the diff: %+v", preview)
	}
	mcpSettingsUnchanged(t, path, disk, changed)
	args["dry_run"] = false
	saved := mcpSettingsCall(t, s, "update_printer_defaults", args)
	read := mcpSettingsCall(t, s, "get_printer_defaults", target)
	want := []resolve.Slot{original.Defaults.Slots[0], {ID: mcpSettingsSlot, Filament: mcpSettingsFilament, Colour: "#102030", Tray: "ams0.3"}}
	if read.Revision != saved.Revision || read.Revision == original.Revision || read.Defaults.Process != mcpSettingsProcess || !slices.Equal(read.Defaults.Slots, want) {
		t.Fatalf("saved defaults = %+v, want process %q and slots %+v", read.Defaults, mcpSettingsProcess, want)
	}
	change := mcpSettingsCall(t, s, "get_profile", map[string]any{"kind": "process", "profile_id": mcpSettingsProcess})
	mcpSettingsCall(t, s, "rename_profile", map[string]any{
		"kind": "process", "profile_id": mcpSettingsProcess, "expected_revision": change.Revision, "name": "Renamed default",
	})
	read = mcpSettingsCall(t, s, "get_printer_defaults", target)
	if read.Defaults.Process != mcpSettingsProcess || !slices.Equal(read.Defaults.Slots, want) {
		t.Fatalf("renaming the default process changed the references: %+v", read.Defaults)
	}
}

func mcpSettingsUI(t *testing.T, f *mcpSettingsFixture, method, path string, in, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		body = bytes.NewReader(must(json.Marshal(in)))
	}
	req, err := http.NewRequestWithContext(ctx, method, f.http.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%s %s returned %s %s: %v", method, path, res.Status, raw, err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

func mcpSettingsLibraryDocument(t *testing.T, f *mcpSettingsFixture) map[string]json.RawMessage {
	t.Helper()
	var response struct {
		Document map[string]json.RawMessage `json:"document"`
	}
	mcpSettingsUI(t, f, http.MethodGet, "/api/library", nil, &response)
	return response.Document
}

func mcpSettingsEvents(t *testing.T, f *mcpSettingsFixture) *bufio.Scanner {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.http.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("event stream returned %s, %q", res.Status, res.Header.Get("Content-Type"))
	}
	return bufio.NewScanner(res.Body)
}

func mcpSettingsEvent(t *testing.T, stream *bufio.Scanner, name string, revision uint64) {
	t.Helper()
	var event string
	for stream.Scan() {
		line := stream.Text()
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && event == name {
			var got struct {
				Revision *uint64 `json:"revision"`
			}
			if json.Unmarshal([]byte(data), &got) != nil || got.Revision == nil || *got.Revision != revision {
				t.Fatalf("event %s data = %q, want revision %d", name, data, revision)
			}
			return
		}
	}
	t.Fatalf("stream closed before %s event for revision %d: %v", name, revision, stream.Err())
}

func TestMCPSettingsPreferencesAndBrowserWrites(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	processTarget := map[string]any{"kind": "process", "profile_id": mcpSettingsProcess}
	preferencesTarget := map[string]any{"kind": "preferences"}
	initialProcess := mcpSettingsCall(t, s, "get_profile", processTarget)
	initial := mcpSettingsCall(t, s, "get_preferences", nil)
	if initial.Revision == "" || initial.PreferencesRevision != 0 {
		t.Fatalf("invalid initial preferences revision: %+v", initial)
	}
	defaults := map[string]json.RawMessage{
		"orca.printer": json.RawMessage(`""`), "orca.vendor": json.RawMessage(`""`),
		"orca.web.afterSlice": json.RawMessage(`"export"`), "orca.web.flowCalibration": json.RawMessage(`"auto"`),
		openDeviceAfterPrintKey: json.RawMessage(`true`), openDeviceDelayKey: json.RawMessage(`5`),
	}
	if !reflect.DeepEqual(initial.Preferences, defaults) {
		t.Fatalf("default preferences = %s, want %s", initial.Preferences, defaults)
	}
	stream := mcpSettingsEvents(t, f)
	mcpSettingsEvent(t, stream, "revision", initialProcess.LibraryRevision)
	mcpSettingsEvent(t, stream, "preferences", initial.PreferencesRevision)

	const privateKey = "orca.example"
	const privateValue = `{"keep":[1,2]}`
	mcpSettingsUI(t, f, http.MethodPut, "/api/preferences", map[string]any{
		"operationId": "browser-preferences", "expectedRevision": initial.PreferencesRevision,
		"document": map[string]any{openDeviceDelayKey: 12, privateKey: json.RawMessage(privateValue)},
	}, nil)
	mcpSettingsEvent(t, stream, "preferences", initial.PreferencesRevision+1)
	uiPrefs := mcpSettingsCall(t, s, "get_preferences", nil)
	if uiPrefs.Revision == initial.Revision || uiPrefs.PreferencesRevision != initial.PreferencesRevision+1 ||
		string(uiPrefs.Preferences[openDeviceDelayKey]) != "12" || uiPrefs.Preferences[privateKey] != nil {
		t.Fatalf("browser preference write was not reflected in MCP: %+v", uiPrefs)
	}

	uiLibrary := mcpSettingsLibraryDocument(t, f)
	uiLibrary["processes"] = json.RawMessage(`[{"id":"profile:process","name":"Browser process","revision":1,"product":"Process","values":{"layer_height":0.2,"sparse_infill_pattern":"grid"}}]`)
	mcpSettingsUI(t, f, http.MethodPut, "/api/library", map[string]any{
		"operationId": "browser-library", "expectedRevision": initialProcess.LibraryRevision, "document": uiLibrary,
	}, nil)
	mcpSettingsEvent(t, stream, "revision", initialProcess.LibraryRevision+1)
	uiProcess := mcpSettingsCall(t, s, "get_profile", processTarget)
	uiHistory := mcpSettingsReadHistory(t, s, processTarget)
	if uiProcess.Revision == initialProcess.Revision || uiProcess.LibraryRevision != initialProcess.LibraryRevision+1 ||
		stringValue(uiProcess.Profile["name"]) != "Browser process" || len(uiHistory.History) != 2 ||
		uiHistory.History[1].ID != uiProcess.RevisionID ||
		!mcpSettingsHasDiff(uiHistory.History[1].Diff, "/name", "Process", "Browser process") {
		t.Fatalf("browser library write was not reflected in MCP history: %+v, %+v", uiProcess, uiHistory)
	}

	path := filepath.Join(f.server.workspace.root, "workspace.json")
	disk, changed := mcpSettingsFile(t, path), f.server.workspace.preferencesWait()
	args := map[string]any{"expected_revision": initial.Revision, "changes": map[string]any{openDeviceDelayKey: 25}}
	stale := mcpTestCall(t, s, "update_preferences", args, nil)
	mcpTestRefusal(t, stale, "revision_conflict")
	if !strings.Contains(mcpTestResultText(stale), uiPrefs.Revision) {
		t.Fatalf("stale preferences write did not identify current revision %q: %s", uiPrefs.Revision, mcpTestResultText(stale))
	}
	mcpSettingsUnchanged(t, path, disk, changed)
	libraryPath := filepath.Join(f.server.library.dir, libraryFile)
	libraryDisk, libraryChanged := mcpSettingsFile(t, libraryPath), f.server.library.wait()
	stale = mcpTestCall(t, s, "update_profile", map[string]any{
		"kind": "process", "profile_id": mcpSettingsProcess, "expected_revision": initialProcess.Revision,
		"changes": map[string]any{"layer_height": 0.25},
	}, nil)
	mcpTestRefusal(t, stale, "revision_conflict")
	if !strings.Contains(mcpTestResultText(stale), uiProcess.Revision) {
		t.Fatalf("stale profile write did not identify current revision %q: %s", uiProcess.Revision, mcpTestResultText(stale))
	}
	mcpSettingsUnchanged(t, libraryPath, libraryDisk, libraryChanged)

	args["expected_revision"] = uiPrefs.Revision
	for _, tc := range []struct {
		key, want string
		value     any
	}{
		{openDeviceAfterPrintKey, "boolean", "false"},
		{openDeviceDelayKey, "between 0 and 60", 100},
		{"orca.web.afterSlice", "export or send", "suspend"},
		{"orca.vendor", "derived", "Other"},
		{privateKey, "blocked preference key", "change"},
	} {
		args["changes"] = map[string]any{tc.key: tc.value}
		mcpTestRefusal(t, mcpTestCall(t, s, "update_preferences", args, nil), tc.want)
		mcpSettingsUnchanged(t, path, disk, changed)
	}
	args["changes"] = map[string]any{
		"orca.printer": mcpFixturePrinter, "orca.web.afterSlice": "send", "orca.web.flowCalibration": "off",
		openDeviceAfterPrintKey: false, openDeviceDelayKey: 25,
	}
	args["dry_run"] = true
	preview := mcpSettingsCall(t, s, "update_preferences", args)
	if !preview.DryRun || !mcpSettingsHasDiff(preview.Diff, "/"+openDeviceDelayKey, float64(12), float64(25)) {
		t.Fatalf("preferences preview lacks the delay diff: %+v", preview)
	}
	mcpSettingsUnchanged(t, path, disk, changed)

	args["dry_run"] = false
	saved := mcpSettingsCall(t, s, "update_preferences", args)
	if saved.PreferencesRevision != uiPrefs.PreferencesRevision+1 || saved.Revision == uiPrefs.Revision {
		t.Fatalf("preferences write did not increment the revision: %+v", saved)
	}
	mcpSettingsEvent(t, stream, "preferences", saved.PreferencesRevision)
	want := map[string]json.RawMessage{
		"orca.printer": json.RawMessage(`"profile:printer"`), "orca.vendor": json.RawMessage(`"BBL"`),
		"orca.web.afterSlice": json.RawMessage(`"send"`), "orca.web.flowCalibration": json.RawMessage(`"off"`),
		openDeviceAfterPrintKey: json.RawMessage(`false`), openDeviceDelayKey: json.RawMessage(`25`),
	}
	read := mcpSettingsCall(t, s, "get_preferences", nil)
	if read.Revision != saved.Revision || !reflect.DeepEqual(read.Preferences, want) {
		t.Fatalf("MCP preferences = %+v, want %s at revision %q", read, want, saved.Revision)
	}
	renamed := mcpSettingsCall(t, s, "rename_profile", map[string]any{
		"kind": "process", "profile_id": mcpSettingsProcess, "expected_revision": uiProcess.Revision, "name": "MCP process",
	})
	mcpSettingsEvent(t, stream, "revision", renamed.LibraryRevision)

	history := mcpSettingsReadHistory(t, s, preferencesTarget)
	if history.Revision != saved.Revision || len(history.History) != 3 ||
		history.History[1].ID != uiPrefs.RevisionID || history.History[1].Snapshot[privateKey] != nil ||
		!mcpSettingsHasDiff(history.History[1].Diff, "/"+openDeviceDelayKey, float64(5), float64(12)) ||
		!mcpSettingsHasDiff(history.History[2].Diff, "/"+openDeviceDelayKey, float64(12), float64(25)) {
		t.Fatalf("MCP history omits or misrepresents previous preference writes: %+v", history)
	}
	disk, changed = mcpSettingsFile(t, path), f.server.workspace.preferencesWait()
	revert := map[string]any{
		"kind": "preferences", "expected_revision": saved.Revision,
		"revision_id": history.History[1].ID, "dry_run": true,
	}
	preview = mcpSettingsCall(t, s, "revert_settings", revert)
	if !preview.DryRun || !reflect.DeepEqual(preview.Preferences, uiPrefs.Preferences) {
		t.Fatalf("preferences revert preview returned %+v, want %+v", preview, uiPrefs)
	}
	mcpSettingsUnchanged(t, path, disk, changed)
	revert["dry_run"] = false
	restored := mcpSettingsCall(t, s, "revert_settings", revert)
	if restored.Revision == saved.Revision || !reflect.DeepEqual(restored.Preferences, uiPrefs.Preferences) {
		t.Fatalf("preferences revert returned %+v, want %+v", restored, uiPrefs.Preferences)
	}
	mcpSettingsEvent(t, stream, "preferences", restored.PreferencesRevision)
	var uiRead struct {
		Document map[string]json.RawMessage `json:"document"`
	}
	mcpSettingsUI(t, f, http.MethodGet, "/api/preferences", nil, &uiRead)
	if string(uiRead.Document[privateKey]) != privateValue || string(uiRead.Document[openDeviceDelayKey]) != "12" {
		t.Fatalf("browser read after restore = %s", uiRead.Document)
	}

	restarted := startMCPSettingsFixture(t, f.server.library.dir, f.server.catalog, f.server.cfg.Options).connect(t)
	read = mcpSettingsCall(t, restarted, "get_preferences", nil)
	history = mcpSettingsReadHistory(t, restarted, preferencesTarget)
	if read.Revision != restored.Revision || !reflect.DeepEqual(read.Preferences, uiPrefs.Preferences) ||
		len(history.History) != 4 || history.History[3].ID != read.RevisionID || history.History[3].Revision != restored.PreferencesRevision {
		t.Fatalf("preference values or history disappeared on restart: %+v, %+v", read, history)
	}
	stored := mcpSettingsCall(t, restarted, "get_profile", processTarget)
	libraryHistory := mcpSettingsReadHistory(t, restarted, processTarget)
	if stored.Revision != renamed.Revision || stringValue(stored.Profile["name"]) != "MCP process" ||
		len(libraryHistory.History) != 3 || libraryHistory.History[1].ID != uiProcess.RevisionID ||
		libraryHistory.History[2].ID != stored.RevisionID {
		t.Fatalf("library values or history disappeared on restart: %+v, %+v", stored, libraryHistory)
	}
}

func TestMCPSettingsRejectArchivedHistory(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	target := map[string]any{"kind": "process", "profile_id": mcpSettingsProcess}
	for _, action := range []string{"archive", "restore"} {
		doc := mcpSettingsLibraryDocument(t, f)
		var records []map[string]json.RawMessage
		mcpTestDecode(t, doc["processes"], &records)
		if len(records) != 1 {
			t.Fatalf("expected one process fixture, got %v", records)
		}
		if action == "archive" {
			records[0]["archived"] = json.RawMessage(`{"at":1760000000000,"operationId":"archive"}`)
		} else {
			delete(records[0], "archived")
		}
		doc["processes"] = must(json.Marshal(records))
		mcpSettingsUI(t, f, http.MethodPut, "/api/library", map[string]any{
			"operationId": action, "expectedRevision": resolve.LibraryRevision(doc), "document": doc,
		}, nil)
	}
	current := mcpSettingsCall(t, s, "get_profile", target)
	history := mcpSettingsReadHistory(t, s, target)
	if len(history.History) != 3 || history.History[1].Snapshot["archived"] == nil {
		t.Fatalf("browser archive and restore did not preserve the archived history: %+v", history)
	}
	args := maps.Clone(target)
	args["expected_revision"], args["revision_id"] = current.Revision, history.History[1].ID
	path := filepath.Join(f.server.library.dir, libraryFile)
	disk, changed := mcpSettingsFile(t, path), f.server.library.wait()
	mcpTestRefusal(t, mcpTestCall(t, s, "revert_settings", args, nil), "archived snapshots")
	mcpSettingsUnchanged(t, path, disk, changed)
}

func TestMCPSettingsRejectInvalidSlotColour(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	current := mcpSettingsCall(t, s, "get_printer_defaults", map[string]any{"printer_id": mcpFixturePrinter})
	path := filepath.Join(f.server.library.dir, libraryFile)
	disk, changed := mcpSettingsFile(t, path), f.server.library.wait()
	for _, preview := range []bool{true, false} {
		result := mcpTestCall(t, s, "update_printer_defaults", map[string]any{
			"printer_id": mcpFixturePrinter, "expected_revision": current.Revision, "dry_run": preview,
			"slots": []map[string]any{{"slot_id": mcpSettingsSlot, "colour": "use a rainbow"}},
		}, nil)
		mcpTestRefusal(t, result, "colour")
		mcpSettingsUnchanged(t, path, disk, changed)
	}
}

func TestMCPSettingsRedactNestedLegacyValues(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	blockedKey := "access_code"
	doc := mcpSettingsLibraryDocument(t, f)
	printer, err := libraryProfile(doc, "printer", mcpFixturePrinter)
	if err != nil {
		t.Fatal(err)
	}
	printer["machine"] = must(json.Marshal(map[string]any{"printable_height": map[string]any{blockedKey: "redaction-sentinel"}}))
	if err := replaceLibraryProfile(doc, "printer", printer, false); err != nil {
		t.Fatal(err)
	}
	mcpSettingsUI(t, f, http.MethodPut, "/api/library", map[string]any{
		"operationId": "legacy-object", "expectedRevision": resolve.LibraryRevision(doc), "document": doc,
	}, nil)
	for _, tool := range []string{"get_profile", "list_settings_history"} {
		result := mcpTestCall(t, s, tool, map[string]any{"kind": "printer", "profile_id": mcpFixturePrinter}, nil)
		if result.IsError {
			t.Fatalf("%s: %s", tool, mcpTestResultText(result))
		}
		raw := must(json.Marshal(result))
		if bytes.Contains(raw, []byte(blockedKey)) || bytes.Contains(raw, []byte("redaction-sentinel")) {
			t.Fatalf("%s exposed a blocked legacy field", tool)
		}
	}
}

func TestMCPSettingsRepeatedRevertAndHistoryLimit(t *testing.T) {
	f := newMCPSettingsFixture(t)
	s := f.connect(t)
	target := map[string]any{"kind": "process", "profile_id": mcpSettingsProcess}
	current := mcpSettingsCall(t, s, "get_profile", target)
	current = mcpSettingsCall(t, s, "update_profile", map[string]any{
		"kind": "process", "profile_id": mcpSettingsProcess, "expected_revision": current.Revision,
		"changes": map[string]any{"layer_height": 0.3},
	})
	history := mcpSettingsReadHistory(t, s, target)
	originalID := history.History[0].ID
	for range 66 {
		previous := current
		current = mcpSettingsCall(t, s, "revert_settings", map[string]any{
			"kind": "process", "profile_id": mcpSettingsProcess, "expected_revision": current.Revision,
			"revision_id": history.History[len(history.History)-1].ID,
		})
		if current.Revision == previous.Revision || current.LibraryRevision != previous.LibraryRevision+1 {
			t.Fatalf("revert did not advance the revision: %+v %+v", previous, current)
		}
		history = mcpSettingsReadHistory(t, s, target)
	}
	if len(history.History) != 64 {
		t.Fatal("history did not retain exactly the newest 64 snapshots")
	}
	for _, entry := range history.History {
		if entry.ID == originalID {
			t.Fatal("history retained a snapshot outside its limit")
		}
	}
	if history.History[len(history.History)-1].Revision != 68 {
		t.Fatalf("history lost a repeated revert: %+v", history.History[len(history.History)-1])
	}
}
