package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

func newLibrarySettingsFixture(t *testing.T) (*Server, map[string]json.RawMessage) {
	t.Helper()
	catalog := mcpFixtureCatalog(t)
	dir := t.TempDir()
	library := &libraryStore{
		dir: dir, catalog: catalog, swap: catalog.Swap,
		extruders: func(string) int { return 1 },
	}
	doc := emptyLibrary("original")
	doc["processes"] = json.RawMessage(`[{"id":"profile:one","name":"One","revision":12,"product":"Process"},{"id":"profile:two","name":"Two","revision":3,"product":"Process"}]`)
	if err := library.write(&libraryEnvelope{Document: doc, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	return &Server{library: library, catalog: catalog, workspace: &workspaceStore{root: dir}}, doc
}

func librarySettingsDisk(t *testing.T, l *libraryStore) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(l.dir, libraryFile))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func librarySettingsSnapshot(t *testing.T, l *libraryStore) *libraryEnvelope {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, err := l.readSettingsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLibrarySettingsPreviewAndCommit(t *testing.T) {
	s, doc := newLibrarySettingsFixture(t)
	before := librarySettingsDisk(t, s.library)
	changed := s.library.wait()

	next := maps.Clone(doc)
	next["processes"] = json.RawMessage(`[{"id":"profile:one","name":"Renamed","revision":999,"product":"Process"},{"id":"profile:two","name":"Two","revision":999,"edited":"2024-01-01T00:00:00Z","product":"Process"}]`)
	in := librarySettingsWrite{ExpectedRevision: 1, ExpectedETag: mcpHash(doc), Document: next, OperationID: "mcp-test", DryRun: true}
	preview, err := s.writeLibrarySettings(in)
	if err != nil {
		t.Fatal(err)
	}
	if preview["revision"] != uint64(1) || preview["dry_run"] != true || len(preview["diff"].([]settingsChange)) == 0 {
		t.Fatalf("unexpected preview %v", preview)
	}
	planned, err := s.catalog.ReadLibrary(preview["document"].(map[string]json.RawMessage))
	if err != nil {
		t.Fatal(err)
	}
	if planned.Revision != 2 || *planned.Processes["profile:one"].Revision != 13 || *planned.Processes["profile:two"].Revision != 3 || planned.Processes["profile:two"].Edited != "" {
		t.Fatalf("unexpected planned revisions and timestamps %+v %+v", planned.Processes["profile:one"], planned.Processes["profile:two"])
	}
	if !bytes.Equal(before, librarySettingsDisk(t, s.library)) || s.library.settled {
		t.Fatal("preview wrote or migrated the stored library")
	}
	select {
	case <-changed:
		t.Fatal("preview sent a library change notification")
	default:
	}

	in.DryRun = false
	result, err := s.writeLibrarySettings(in)
	if err != nil {
		t.Fatal(err)
	}
	if result["revision"] != uint64(2) {
		t.Fatalf("commit revision %v", result["revision"])
	}
	select {
	case <-changed:
	default:
		t.Fatal("commit did not send a library change notification")
	}
	stored := librarySettingsSnapshot(t, s.library)
	history := stored.History["process/profile:one"]
	if len(stored.History) != 1 || len(history) != 2 || history[0].Revision != 12 || history[1].Revision != 13 || stored.Operations[in.OperationID].Revision != 2 {
		t.Fatalf("unexpected stored history or receipt %+v %+v", stored.History, stored.Operations)
	}
	var original, updated resolve.Process
	if json.Unmarshal(history[0].Document, &original) != nil || json.Unmarshal(history[1].Document, &updated) != nil || original.Name != "One" || updated.Name != "Renamed" {
		t.Fatalf("unexpected stored snapshots %s %s", history[0].Document, history[1].Document)
	}

	after := librarySettingsDisk(t, s.library)
	replay, err := s.writeLibrarySettings(in)
	if err != nil || replay["revision"] != uint64(2) || !bytes.Equal(after, librarySettingsDisk(t, s.library)) {
		t.Fatalf("replay returned %v, %v", replay, err)
	}
	in.OperationID = "stale"
	_, err = s.writeLibrarySettings(in)
	var failure *settingsWriteError
	if !errors.As(err, &failure) || failure.Code != "revision_conflict" || failure.Revision != 2 {
		t.Fatalf("stale write returned %v", err)
	}
}

func TestLibrarySettingsHTTPUsesSameHistory(t *testing.T) {
	s, doc := newLibrarySettingsFixture(t)
	s.sends = &SendManager{}
	mux := http.NewServeMux()
	s.libraryRoutes(mux)
	put := func(operation string, revision uint64, candidate map[string]json.RawMessage, status int) *httptest.ResponseRecorder {
		t.Helper()
		body := must(json.Marshal(map[string]any{"expectedRevision": revision, "operationId": operation, "document": candidate, "dryRun": true}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/library", bytes.NewReader(body)))
		if w.Code != status {
			t.Fatalf("PUT returned %d, want %d (%s)", w.Code, status, w.Body)
		}
		return w
	}
	noOp := maps.Clone(doc)
	noOp["processes"] = json.RawMessage(`[{"id":"profile:one","name":"One","revision":19,"product":"Process"},{"id":"profile:two","name":"Two","revision":0,"product":"Process"}]`)
	before := librarySettingsDisk(t, s.library)
	put("no-op", 1, noOp, http.StatusOK)
	if !bytes.Equal(before, librarySettingsDisk(t, s.library)) {
		t.Fatal("metadata-only update wrote the library")
	}
	conflict := maps.Clone(doc)
	conflict["processes"] = json.RawMessage(`[{"id":"profile:one","name":"Two","revision":12,"product":"Process"},{"id":"profile:two","name":"Two","revision":3,"product":"Process"}]`)
	w := put("conflict", 1, conflict, http.StatusConflict)
	var rejection libraryError
	if json.Unmarshal(w.Body.Bytes(), &rejection) != nil || rejection.Code != "name_taken" || rejection.Recovery != "rename" || rejection.Revision != 1 {
		t.Fatalf("unexpected name conflict %s", w.Body)
	}
	next := maps.Clone(doc)
	next["processes"] = json.RawMessage(`[{"id":"profile:one","name":"One","revision":12,"product":"Process"}]`)
	put("delete", 1, next, http.StatusOK)
	stored := librarySettingsSnapshot(t, s.library)
	history := stored.History["process/profile:two"]
	if len(history) != 2 || history[0].Revision != 3 || history[1].Revision != 4 || string(history[1].Document) != "null" || stored.Operations["delete"].Revision != 2 {
		t.Fatalf("unexpected HTTP deletion record %+v %+v", stored.History, stored.Operations)
	}
	put("delete", 1, next, http.StatusOK)
	w = put("other", 1, next, http.StatusConflict)
	if json.Unmarshal(w.Body.Bytes(), &rejection) != nil || rejection.Code != "revision_conflict" || rejection.Revision != 2 {
		t.Fatalf("unexpected revision conflict %s", w.Body)
	}
}

func TestLibrarySettingsSnapshotRejectsMigrationAndStaleETag(t *testing.T) {
	s, doc := newLibrarySettingsFixture(t)
	in := librarySettingsWrite{ExpectedRevision: 1, ExpectedETag: mcpHash(doc), Document: doc, DryRun: true}
	different := maps.Clone(doc)
	different["processes"] = json.RawMessage(`[]`)
	if err := s.library.write(&libraryEnvelope{Document: different, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.writeLibrarySettings(in)
	var failure *settingsWriteError
	if !errors.As(err, &failure) || failure.Code != "revision_conflict" || failure.Revision != 1 || !strings.Contains(failure.Message, mcpHash(different)) {
		t.Fatalf("same-revision write with stale etag returned %v", err)
	}

	different["schema"] = json.RawMessage(`4`)
	if err := s.library.write(&libraryEnvelope{Document: different, Operations: map[string]libraryReceipt{}}); err != nil {
		t.Fatal(err)
	}
	before := librarySettingsDisk(t, s.library)
	_, err = s.writeLibrarySettings(in)
	if !errors.As(err, &failure) || failure.Code != "library_upgrade_needed" || failure.Revision != 1 {
		t.Fatalf("legacy preview returned %v", err)
	}
	if s.library.settled || !bytes.Equal(before, librarySettingsDisk(t, s.library)) {
		t.Fatal("legacy preview changed the store")
	}
}

func TestLibrarySettingsPreviewChecksDraftReferencesWithoutMigration(t *testing.T) {
	s, doc := newLibrarySettingsFixture(t)
	data := newWorkspaceData()
	data.Drafts["draft"] = workspaceDraft{ID: "draft", Document: json.RawMessage(`{"name":"Fixture","doc":{"process":"profile:one"}}`)}
	workspaceBefore := must(json.Marshal(data))
	path := filepath.Join(s.workspace.root, "workspace.json")
	if err := os.WriteFile(path, workspaceBefore, 0600); err != nil {
		t.Fatal(err)
	}
	before := librarySettingsDisk(t, s.library)
	next := maps.Clone(doc)
	next["processes"] = json.RawMessage(`[]`)
	_, err := s.writeLibrarySettings(librarySettingsWrite{ExpectedRevision: 1, ExpectedETag: mcpHash(doc), Document: next, DryRun: true})
	var failure *settingsWriteError
	if !errors.As(err, &failure) || failure.Code != "profile_referenced" || !strings.Contains(failure.Message, "Fixture (draft) uses One") {
		t.Fatalf("referenced profile deletion returned %v", err)
	}
	workspaceAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, librarySettingsDisk(t, s.library)) || !bytes.Equal(workspaceBefore, workspaceAfter) {
		t.Fatal("preview changed the library or workspace")
	}
}
