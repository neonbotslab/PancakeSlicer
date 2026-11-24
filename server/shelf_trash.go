package main

import (
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"time"
)

const shelfTrashRetention = 24 * time.Hour

type shelfTombstone struct {
	RootID    string                 `json:"rootId"`
	ExpiresAt time.Time              `json:"expiresAt"`
	Folders   map[string]shelfFolder `json:"folders,omitempty"`
	Models    map[string]shelfModel  `json:"models,omitempty"`
}

type shelfUndo struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type shelfRemoval struct {
	Tree shelfDocument `json:"tree"`
	Undo shelfUndo     `json:"undo"`
}

type shelfTransaction struct {
	shelfDocument
	trash map[string]shelfTombstone
	now   time.Time
}

func pruneShelfTrash(trash map[string]shelfTombstone, now time.Time) bool {
	pruned := false
	for token, tombstone := range trash {
		if !now.Before(tombstone.ExpiresAt) {
			delete(trash, token)
			pruned = true
		}
	}
	return pruned
}

func (s *Server) shelfEnvelopeSnapshot(prune bool) (*libraryEnvelope, bool, error) {
	s.library.mu.Lock()
	defer s.library.mu.Unlock()
	e, _, err := s.library.readEnvelope()
	if err != nil {
		return nil, false, err
	}
	pruned := prune && pruneShelfTrash(e.ShelfTrash, time.Now())
	if pruned {
		if err := durableJSON(filepath.Join(s.library.dir, libraryFile), e); err != nil {
			return nil, false, err
		}
	}
	return e, pruned, nil
}

func (s *Server) changeShelfState(expected uint64, edit func(*shelfTransaction) error) (shelfDocument, error) {
	pruned := false
	defer func() {
		if pruned {
			go s.blobs.evict()
		}
	}()
	s.blobs.mu.Lock()
	defer s.blobs.mu.Unlock()
	s.library.mu.Lock()
	defer s.library.mu.Unlock()
	e, _, err := s.library.readEnvelope()
	if err != nil {
		return shelfDocument{}, err
	}
	tx := shelfTransaction{shelfDocument: shelfOf(e), trash: e.ShelfTrash, now: time.Now().UTC()}
	if err = tx.checkRevision(expected); err != nil {
		return shelfDocument{}, err
	}
	if err = edit(&tx); err != nil {
		return shelfDocument{}, err
	}
	tx.Revision++
	e.Shelf = &tx.shelfDocument
	e.ShelfTrash = tx.trash
	expired := pruneShelfTrash(e.ShelfTrash, tx.now)
	if err = durableJSON(filepath.Join(s.library.dir, libraryFile), e); err != nil {
		return shelfDocument{}, err
	}
	pruned = expired
	return tx.shelfDocument, nil
}

func (s *Server) shelfDelete(id string, folder bool, expected uint64, recursive bool) (shelfDocument, error) {
	removed, err := s.shelfRemove(id, folder, expected, recursive)
	return removed.Tree, err
}

func (s *Server) shelfRemove(id string, folder bool, expected uint64, recursive bool) (shelfRemoval, error) {
	var undo shelfUndo
	d, err := s.changeShelfState(expected, func(tx *shelfTransaction) error {
		targets, err := tx.deletionTargets(id, folder, recursive)
		if err != nil {
			return err
		}
		undo = shelfUndo{Token: newID("undo"), ExpiresAt: tx.now.Add(shelfTrashRetention)}
		if tx.trash == nil {
			tx.trash = map[string]shelfTombstone{}
		}
		tx.trash[undo.Token] = shelfTombstone{RootID: id, ExpiresAt: undo.ExpiresAt, Folders: targets.Folders, Models: targets.Models}
		for child := range targets.Folders {
			delete(tx.Folders, child)
		}
		for model := range targets.Models {
			delete(tx.Models, model)
		}
		return nil
	})
	if err != nil {
		return shelfRemoval{}, err
	}
	return shelfRemoval{Tree: d, Undo: undo}, nil
}

func (s *Server) deleteShelf(w http.ResponseWriter, r *http.Request, folder bool) {
	expected, err := shelfExpected(r)
	if err != nil {
		shelfFail(w, err)
		return
	}
	recursive := r.URL.Query().Get("recursive")
	if recursive != "" && recursive != "false" && recursive != "true" {
		shelfFail(w, shelfProblem(400, "recursive_invalid", "recursive must be true or false."))
		return
	}
	removed, err := s.shelfRemove(r.PathValue("id"), folder, expected, recursive == "true")
	if err != nil {
		shelfFail(w, err)
		return
	}
	shelfRevisionHeaders(w, removed.Tree.Revision)
	writeJSON(w, http.StatusOK, removed)
}

func (s *Server) shelfRestore(token string, expected uint64) (shelfDocument, error) {
	return s.changeShelfState(expected, func(tx *shelfTransaction) error {
		tombstone, ok := tx.trash[token]
		if !ok || !tx.now.Before(tombstone.ExpiresAt) {
			return shelfProblem(404, "undo_unavailable", "This shelf deletion can no longer be undone.")
		}
		names := newShelfSiblingNames(tx.shelfDocument)
		if folder, ok := tombstone.Folders[tombstone.RootID]; ok {
			subtree := shelfDocument{Folders: tombstone.Folders}
			if tx.depth(folder.Parent)+1+subtree.subtreeDepth(folder.ID) > shelfMaxFolderDepth {
				folder.Parent = ""
			}
			folder.Name = names.reserve(folder.Parent, folder.Name, "")
			tombstone.Folders[folder.ID] = folder
		} else {
			model, ok := tombstone.Models[tombstone.RootID]
			if !ok {
				return errors.New("invalid shelf trash root")
			}
			if !tx.parentExists(model.Parent) {
				model.Parent = ""
			}
			model.Name = names.reserve(model.Parent, model.Name, filepath.Ext(model.Name))
			tombstone.Models[model.ID] = model
		}
		maps.Copy(tx.Folders, tombstone.Folders)
		maps.Copy(tx.Models, tombstone.Models)
		delete(tx.trash, token)
		return nil
	})
}

func (s *Server) restoreShelf(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedRevision *uint64 `json:"expectedRevision"`
	}
	if !workspaceDecode(w, r, &in) {
		return
	}
	if in.ExpectedRevision == nil {
		shelfFail(w, shelfProblem(400, "revision_required", "expectedRevision with the shelf revision is required."))
		return
	}
	d, err := s.shelfRestore(r.PathValue("token"), *in.ExpectedRevision)
	if err != nil {
		shelfFail(w, err)
		return
	}
	shelfAnswer(w, http.StatusOK, d)
}
