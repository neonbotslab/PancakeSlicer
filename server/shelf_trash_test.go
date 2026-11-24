package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shelfTestExpire(t *testing.T, s *Server, undo shelfUndo) {
	t.Helper()
	s.library.mu.Lock()
	defer s.library.mu.Unlock()
	e, _, err := s.library.readEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	tombstone, ok := e.ShelfTrash[undo.Token]
	if !ok {
		t.Fatal("undo receipt is not stored")
	}
	tombstone.ExpiresAt = time.Now().Add(-time.Second)
	e.ShelfTrash[undo.Token] = tombstone
	if err := durableJSON(filepath.Join(s.library.dir, libraryFile), e); err != nil {
		t.Fatal(err)
	}
}

func shelfTestRemove(t *testing.T, h http.Handler, path string, revision uint64) shelfRemoval {
	t.Helper()
	w := shelfJSON(t, h, http.MethodDelete, "/api/shelf/"+path, nil, &revision)
	shelfRequireStatus(t, w, http.StatusOK)
	var removed shelfRemoval
	if err := json.Unmarshal(w.Body.Bytes(), &removed); err != nil || removed.Tree.Revision != revision+1 || removed.Undo.Token == "" || removed.Undo.ExpiresAt.IsZero() {
		t.Fatalf("invalid deletion response %s", w.Body)
	}
	return removed
}

func shelfTestRestoreURL(undo shelfUndo) string {
	return "/api/shelf/trash/" + url.PathEscape(undo.Token) + "/restore"
}

func TestShelfRestoreModelAcrossRestart(t *testing.T) {
	s, h := shelfFixture(t)
	w := shelfUpload(t, h, 0, "", map[string][]byte{"part.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	d, err := s.shelfSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var original shelfModel
	for _, model := range d.Models {
		original = model
	}
	before := time.Now()
	removed := shelfTestRemove(t, h, "models/"+original.ID, 1)
	if len(removed.Tree.Models) != 0 || removed.Undo.ExpiresAt.Before(before.Add(shelfTrashRetention)) || removed.Undo.ExpiresAt.After(time.Now().Add(shelfTrashRetention)) {
		t.Fatalf("unexpected removal %+v", removed)
	}
	w = shelfJSON(t, h, http.MethodGet, "/api/shelf/models/"+original.ID+"/thumbnail.png", nil, nil)
	shelfRequireStatus(t, w, http.StatusNotFound)
	newServer, restarted := shelfFixtureAt(t, s.library.dir)
	w = shelfJSON(t, restarted, http.MethodGet, "/api/shelf?tree=1", nil, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), removed.Undo.Token) || strings.Contains(w.Body.String(), original.ID) || strings.Contains(w.Body.String(), "shelfTrash") {
		t.Fatalf("listing exposed deleted entries or receipts %s", w.Body)
	}
	w = shelfUpload(t, restarted, 2, "", map[string][]byte{"PART.stl": []byte(shelfTestSTL + "\n")})
	shelfRequireStatus(t, w, 201)
	restore := shelfTestRestoreURL(removed.Undo)
	w = shelfJSON(t, restarted, http.MethodPost, restore, map[string]uint64{"expectedRevision": 2}, nil)
	shelfRequireStatus(t, w, http.StatusConflict)
	w = shelfJSON(t, restarted, http.MethodPost, restore, map[string]uint64{"expectedRevision": 3}, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	original.Name = "part 2.stl"
	var restored shelfDocument
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil || restored.Revision != 4 || len(restored.Models) != 2 || restored.Models[original.ID] != original {
		t.Fatalf("restored model did not retain identity and content %s", w.Body)
	}
	_, file, cleanup, err := newServer.shelfSource(original.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	contents, err := os.ReadFile(file)
	if err != nil || string(contents) != shelfTestSTL {
		t.Fatalf("restored bytes %q: %v", contents, err)
	}
	w = shelfJSON(t, restarted, http.MethodPost, restore, map[string]uint64{"expectedRevision": 4}, nil)
	shelfRequireStatus(t, w, http.StatusNotFound)
}

func TestShelfRestoreSubtreeAfterParentDeletion(t *testing.T) {
	s, h := shelfFixture(t)
	w := shelfJSON(t, h, http.MethodPost, "/api/shelf/folders", map[string]any{"expectedRevision": 0, "name": "Parent"}, nil)
	shelfRequireStatus(t, w, http.StatusCreated)
	var created struct{ Folder shelfFolder }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	parent := created.Folder.ID
	archive := shelfZIP(t, map[string][]byte{"parts/model.stl": []byte(shelfTestSTL), "empty/": nil})
	w = shelfUpload(t, h, 1, parent, map[string][]byte{"Archive.stl.zip": archive})
	shelfRequireStatus(t, w, http.StatusCreated)
	before, err := s.shelfSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var root shelfFolder
	for _, folder := range before.Folders {
		if folder.Parent == parent {
			root = folder
		}
	}
	removed := shelfTestRemove(t, h, "folders/"+root.ID+"?recursive=true", 2)
	if len(removed.Tree.Folders) != 1 || len(removed.Tree.Models) != 0 {
		t.Fatalf("subtree was not removed %+v", removed.Tree)
	}
	shelfTestRemove(t, h, "folders/"+parent, 3)
	w = shelfJSON(t, h, http.MethodPost, "/api/shelf/folders", map[string]any{"expectedRevision": 4, "name": root.Name}, nil)
	shelfRequireStatus(t, w, http.StatusCreated)
	w = shelfJSON(t, h, http.MethodPost, shelfTestRestoreURL(removed.Undo), map[string]uint64{"expectedRevision": 5}, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	var after shelfDocument
	root.Parent, root.Name = "", "Archive.stl 2"
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil || after.Revision != 6 || len(after.Folders) != 4 || len(after.Models) != 1 || after.Folders[root.ID] != root {
		t.Fatalf("restored tree %s", w.Body)
	}
	for id, folder := range before.Folders {
		if id != parent && id != root.ID && after.Folders[id] != folder {
			t.Fatalf("changed descendant folder %+v", folder)
		}
	}
	for id, model := range before.Models {
		if after.Models[id] != model {
			t.Fatalf("changed descendant model %+v", model)
		}
	}
}

func TestShelfTrashExpiryKeepsOtherReferences(t *testing.T) {
	s, h := shelfFixture(t)
	s.blobs.limit = 1
	w := shelfUpload(t, h, 0, "", map[string][]byte{"one.stl": []byte(shelfTestSTL), "two.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, http.StatusCreated)
	d, err := s.shelfSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var removed []shelfRemoval
	var digest string
	for _, model := range d.Models {
		digest = model.SHA256
		removed = append(removed, shelfTestRemove(t, h, "models/"+model.ID, uint64(len(removed)+1)))
	}
	first, second := removed[0].Undo, removed[1].Undo
	shelfTestExpire(t, s, first)
	w = shelfJSON(t, h, http.MethodPost, shelfTestRestoreURL(first), map[string]uint64{"expectedRevision": 3}, nil)
	shelfRequireStatus(t, w, http.StatusNotFound)
	w = shelfJSON(t, h, http.MethodGet, "/api/shelf?tree=1", nil, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	if w.Header().Get("ETag") != `"3"` {
		t.Fatalf("expiry cleanup changed the shelf revision: %s", w.Body)
	}
	e, _, err := s.shelfEnvelopeSnapshot(false)
	if err != nil || len(e.ShelfTrash) != 1 || e.ShelfTrash[second.Token].RootID == "" {
		t.Fatalf("expiry cleanup left the wrong trash: %+v (%v)", e, err)
	}
	s.blobs.evict()
	if _, err := os.Stat(s.blobs.path(digest)); err != nil {
		t.Fatal("expiry collected a blob shared with another receipt", err)
	}
	w = shelfJSON(t, h, http.MethodPost, shelfTestRestoreURL(second), map[string]uint64{"expectedRevision": 3}, nil)
	shelfRequireStatus(t, w, http.StatusOK)
}

func TestShelfRestoreSubtreeAfterParentMovesTooDeep(t *testing.T) {
	s, h := shelfFixture(t)
	w := shelfJSON(t, h, http.MethodPost, "/api/shelf/folders", map[string]any{"expectedRevision": 0, "name": "Parent"}, nil)
	shelfRequireStatus(t, w, http.StatusCreated)
	var created struct{ Folder shelfFolder }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	parent := created.Folder.ID
	w = shelfUpload(t, h, 1, parent, map[string][]byte{"Child.zip": shelfZIP(t, map[string][]byte{"empty/": nil})})
	shelfRequireStatus(t, w, http.StatusCreated)
	w = shelfUpload(t, h, 2, "", map[string][]byte{"Deep.zip": shelfZIP(t, map[string][]byte{strings.Repeat("deep/", 30): nil})})
	shelfRequireStatus(t, w, http.StatusCreated)
	d, err := s.shelfSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var child, deep string
	for id, folder := range d.Folders {
		if folder.Parent == parent {
			child = id
		}
		if d.depth(id) == 31 {
			deep = id
		}
	}
	removed := shelfTestRemove(t, h, "folders/"+child+"?recursive=true", 3)
	w = shelfJSON(t, h, http.MethodPatch, "/api/shelf/folders/"+parent, map[string]any{"expectedRevision": 4, "parent": deep}, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	w = shelfJSON(t, h, http.MethodPost, shelfTestRestoreURL(removed.Undo), map[string]uint64{"expectedRevision": 5}, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	var restored shelfDocument
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil || restored.Folders[child].ID != child || restored.depth(child) != 1 || restored.depth(parent) != 32 {
		t.Fatalf("restored subtree exceeded permitted depth: %s", w.Body)
	}
}
