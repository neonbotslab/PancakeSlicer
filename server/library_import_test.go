package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

func exportedPreset(t *testing.T, typ, name string, record map[string]any) parsedPreset {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return parsedPreset{File: name + ".json", Name: name, Type: typ, Record: raw, Values: map[string]json.RawMessage{}, Meta: map[string]json.RawMessage{}}
}

func exportedPrinter(t *testing.T, filament, process string) parsedPreset {
	t.Helper()
	record := map[string]any{
		"id": "profile:old-printer", "name": "Voron 2.4 300", "revision": 0, "vendor": "Voron", "model": "Voron 2.4 300", "swap": "manual",
		"nozzles": []map[string]any{{"variant": "Voron 2.4 300 0.4 nozzle", "diameter": 0.4}},
		"slots":   []map[string]any{{"id": "slot:old-1", "filament": filament, "colour": "#ff0000"}},
	}
	if process != "" {
		record["process"] = process
	}
	return exportedPreset(t, "machine", "Voron 2.4 300", record)
}

func exportedFilament(t *testing.T, id string) parsedPreset {
	t.Helper()
	return exportedPreset(t, "filament", "PLA Red", map[string]any{"id": id, "name": "PLA Red", "revision": 0, "product": "Generic PLA"})
}

func blankLibrary() *resolve.Library {
	return &resolve.Library{Printers: map[string]*resolve.Printer{}, Filaments: map[string]*resolve.Filament{}, Alias: map[string]string{}, Processes: map[string]*resolve.Process{}}
}

func rowOf(t *testing.T, rows []importRow, kind string) importRow {
	t.Helper()
	for _, r := range rows {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("no %s row in %+v", kind, rows)
	return importRow{}
}

func recordFields(t *testing.T, raw json.RawMessage) (id string, aliases []string, slots []resolve.Slot, process string) {
	t.Helper()
	var rec struct {
		ID      string         `json:"id"`
		Aliases []string       `json:"aliases"`
		Slots   []resolve.Slot `json:"slots"`
		Process string         `json:"process"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.ID, rec.Aliases, rec.Slots, rec.Process
}

func TestImportedPrinterKeepsItsSlotFilamentAndPrintProfile(t *testing.T) {
	presets := []parsedPreset{
		exportedPrinter(t, "profile:old-filament", "profile:old-process"),
		exportedFilament(t, "profile:old-filament"),
		exportedPreset(t, "process", "Standard", map[string]any{"id": "profile:old-process", "name": "Standard", "revision": 0}),
	}
	lib := blankLibrary()
	rows := (&Server{}).planImport(presets, lib)

	filament := rowOf(t, rows, "filament")
	profile := rowOf(t, rows, "process")
	printer := rowOf(t, rows, "printer")
	selected := []importRecord{printer.importRecord, filament.importRecord, profile.importRecord}
	remapImportedPrinters(selected, lib)
	filamentID, _, _, _ := recordFields(t, filament.Record)
	processID, _, _, _ := recordFields(t, profile.Record)
	_, _, slots, process := recordFields(t, selected[0].Record)

	if slots[0].Filament != filamentID {
		t.Errorf("slot holds %q, the imported filament is %q", slots[0].Filament, filamentID)
	}
	if process != processID {
		t.Errorf("printer's print profile is %q, the imported one is %q", process, processID)
	}
	if printer.Note != "" {
		t.Errorf("nothing was dropped, but the row says %q", printer.Note)
	}
	imported := blankLibrary()
	imported.Filaments[filamentID] = &resolve.Filament{ID: filamentID}
	if f := imported.Filament(slots[0].Filament); f == nil || !f.Live() {
		t.Errorf("the slot's filament %q is not in the library after the import", slots[0].Filament)
	}
}

func TestReplacedFilamentStaysReachableUnderItsExportedID(t *testing.T) {
	revision := uint64(1)
	lib := blankLibrary()
	lib.Filaments["profile:existing"] = &resolve.Filament{ID: "profile:existing", Name: "PLA Red", Revision: &revision, Product: "Generic PLA"}
	presets := []parsedPreset{
		exportedPrinter(t, "profile:old-filament", ""),
		exportedFilament(t, "profile:old-filament"),
	}
	rows := (&Server{}).planImport(presets, lib)

	filament := rowOf(t, rows, "filament")
	if len(filament.Matches) != 1 {
		t.Fatalf("expected the existing PLA Red as a match, got %+v", filament.Matches)
	}
	id, aliases, _, _ := recordFields(t, filament.Matches[0].Record)
	if id != "profile:existing" {
		t.Fatalf("replacing keeps the existing id, got %q", id)
	}
	if len(aliases) != 1 || aliases[0] != "profile:old-filament" {
		t.Fatalf("replaced filament should carry the exported id as alias, got %v", aliases)
	}

	filament.Record = filament.Matches[0].Record
	selected := []importRecord{rowOf(t, rows, "printer").importRecord, filament.importRecord}
	remapImportedPrinters(selected, lib)
	_, _, slots, _ := recordFields(t, selected[0].Record)
	replaced := blankLibrary()
	replaced.Filaments[id] = &resolve.Filament{ID: id}
	replaced.Alias[aliases[0]] = id
	if f := replaced.Filament(slots[0].Filament); f == nil || f.ID != id {
		t.Errorf("slot %q does not resolve to the replaced filament", slots[0].Filament)
	}
}

func TestImportedPrinterUsesSelectedPrintProfile(t *testing.T) {
	if err := configureResolve([]byte(`{"layer_height":{"type":"Float","profile":"process","default":"0.2"}}`)); err != nil {
		t.Fatal(err)
	}
	catalog := &resolve.Catalog{}
	store := &libraryStore{dir: t.TempDir(), catalog: catalog}
	doc := emptyLibrary("")
	doc["processes"] = json.RawMessage(`[{"id":"profile:existing","name":"Standard","revision":1}]`)
	if err := store.write(&libraryEnvelope{Document: doc, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	server := &Server{catalog: catalog, library: store}
	lib, err := server.libraryDocument()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	server.libraryIORoutes(mux)
	for _, action := range []string{"add", "replace", "skip"} {
		t.Run(action, func(t *testing.T) {
			presets := []parsedPreset{
				exportedPrinter(t, "", "profile:old-process"),
				exportedPreset(t, "process", "Standard", map[string]any{"id": "profile:old-process", "name": "Standard", "revision": 0}),
			}
			rows := server.planImport(presets, lib)
			profile := rowOf(t, rows, "process")
			if len(profile.Matches) != 1 || profile.Matches[0].ID != "profile:existing" {
				t.Fatalf("expected a replacement choice even for identical settings, got %+v", profile.Matches)
			}
			selected := []importRecord{rowOf(t, rows, "printer").importRecord}
			want := ""
			switch action {
			case "add":
				selected = append(selected, profile.importRecord)
				want = "profile:old-process"
			case "replace":
				profile.Record = profile.Matches[0].Record
				selected = append(selected, profile.importRecord)
				want = "profile:existing"
			}
			body, err := json.Marshal(selected)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/library/import/resolve", bytes.NewReader(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("resolve returned %d: %s", response.Code, response.Body)
			}
			var result struct {
				Records  []importRecord
				Problems []string
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != len(selected) {
				t.Fatalf("unexpected resolve response %s: %v", response.Body, err)
			}
			_, _, _, got := recordFields(t, result.Records[0].Record)
			if cleared := len(result.Problems) > 0; got != want || cleared != (want == "") {
				t.Errorf("printer process = %q, cleared = %t; want %q, cleared = %t", got, cleared, want, want == "")
			}
		})
	}
}

func TestImportedPrinterDropsReferencesToMissingOrSkippedProfiles(t *testing.T) {
	lib := blankLibrary()
	presets := []parsedPreset{
		exportedPrinter(t, "profile:old-filament", "profile:old-process"),
		exportedFilament(t, "profile:old-filament"),
	}
	rows := (&Server{}).planImport(presets, lib)

	printer := rowOf(t, rows, "printer")
	if printer.Status != "new" {
		t.Fatalf("the printer should still import, got %s: %s", printer.Status, printer.Note)
	}
	selected := []importRecord{printer.importRecord}
	cleared := remapImportedPrinters(selected, lib)
	if !cleared[0] {
		t.Error("expected the import to report cleared dependencies")
	}
	_, _, slots, process := recordFields(t, selected[0].Record)
	if len(slots) != 1 || slots[0].ID != "slot:old-1" || slots[0].Colour != "#ff0000" {
		t.Fatalf("the slot itself must survive, got %+v", slots)
	}
	if slots[0].Filament != "" {
		t.Errorf("dangling slot filament kept: %q", slots[0].Filament)
	}
	if process != "" {
		t.Errorf("dangling print profile kept: %q", process)
	}
	if !strings.Contains(printer.Note, "pick them again") {
		t.Errorf("the row should tell the user to pick again, got %q", printer.Note)
	}
}

func TestImportedPrinterKeepsSkippedReferencesAvailableInLibrary(t *testing.T) {
	revision := uint64(1)
	lib := blankLibrary()
	lib.Filaments["profile:local-filament"] = &resolve.Filament{
		ID: "profile:local-filament", Name: "PLA Red", Revision: &revision, Product: "Generic PLA",
		Aliases: []string{"profile:old-filament"},
	}
	lib.Alias["profile:old-filament"] = "profile:local-filament"
	lib.Processes["profile:old-process"] = &resolve.Process{ID: "profile:old-process", Name: "Standard", Revision: &revision}
	presets := []parsedPreset{
		exportedPrinter(t, "profile:old-filament", "profile:old-process"),
		exportedFilament(t, "profile:old-filament"),
		exportedPreset(t, "process", "Standard", map[string]any{"id": "profile:old-process", "name": "Standard", "revision": 0}),
	}
	rows := (&Server{}).planImport(presets, lib)
	selected := []importRecord{rowOf(t, rows, "printer").importRecord}
	cleared := remapImportedPrinters(selected, lib)
	_, _, slots, process := recordFields(t, selected[0].Record)
	if cleared[0] || slots[0].Filament != "profile:old-filament" || process != "profile:old-process" {
		t.Errorf("live references changed: slot filament = %q, process = %q, cleared = %t", slots[0].Filament, process, cleared[0])
	}
}
