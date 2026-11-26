package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	shelfMaxNameBytes   = 240
	shelfMaxFolderDepth = 32
)

type shelfDocument struct {
	Revision uint64                 `json:"revision"`
	Folders  map[string]shelfFolder `json:"folders"`
	Models   map[string]shelfModel  `json:"models"`
}
type shelfFolder struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
	Name   string `json:"name"`
}
type shelfModel struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Format string `json:"format"`
	Size   int64  `json:"size"`
}
type shelfError struct {
	Status        int
	Code, Message string
	Revision      uint64
}

func (e *shelfError) Error() string { return e.Message }
func shelfProblem(status int, code, message string) error {
	return &shelfError{Status: status, Code: code, Message: message}
}
func shelfFail(w http.ResponseWriter, err error) {
	var failure *shelfError
	if errors.As(err, &failure) {
		writeJSON(w, failure.Status, map[string]any{"error": failure.Message, "code": failure.Code, "revision": failure.Revision})
		return
	}
	workspaceFailure(w, 503, "shelf_unavailable", "Shelf storage is unavailable.", "retry_storage", 0)
}
func emptyShelf() shelfDocument {
	return shelfDocument{Folders: map[string]shelfFolder{}, Models: map[string]shelfModel{}}
}
func shelfOf(e *libraryEnvelope) shelfDocument {
	if e.Shelf == nil {
		return emptyShelf()
	}
	d := *e.Shelf
	if d.Folders == nil {
		d.Folders = map[string]shelfFolder{}
	}
	if d.Models == nil {
		d.Models = map[string]shelfModel{}
	}
	return d
}
func (s *Server) shelfSnapshot() (shelfDocument, error) {
	e, _, err := s.shelfEnvelopeSnapshot(false)
	if err != nil {
		return shelfDocument{}, err
	}
	return shelfOf(e), nil
}
func shelfName(name string) bool {
	return utf8.ValidString(name) && name != "" && strings.TrimSpace(name) == name && name != "." && name != ".." && len(name) <= shelfMaxNameBytes && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0
}
func (d shelfDocument) parentExists(parent string) bool {
	if parent == "" {
		return true
	}
	_, ok := d.Folders[parent]
	return ok
}
func (d shelfDocument) nameTaken(parent, name, except string) bool {
	for id, f := range d.Folders {
		if id != except && f.Parent == parent && strings.EqualFold(f.Name, name) {
			return true
		}
	}
	for id, m := range d.Models {
		if id != except && m.Parent == parent && strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

type shelfSiblingNames struct {
	used map[string]map[string]bool
	next map[string]int
}

func shelfFoldName(name string) string {
	return strings.Map(func(r rune) rune {
		first := r
		for next := unicode.SimpleFold(r); next != first; next = unicode.SimpleFold(next) {
			r = min(r, next)
		}
		return r
	}, name)
}
func newShelfSiblingNames(d shelfDocument) *shelfSiblingNames {
	names := &shelfSiblingNames{used: map[string]map[string]bool{}, next: map[string]int{}}
	for _, folder := range d.Folders {
		names.add(folder.Parent, folder.Name)
	}
	for _, model := range d.Models {
		names.add(model.Parent, model.Name)
	}
	return names
}
func (n *shelfSiblingNames) add(parent, name string) {
	if n.used[parent] == nil {
		n.used[parent] = map[string]bool{}
	}
	n.used[parent][shelfFoldName(name)] = true
}
func (n *shelfSiblingNames) reserve(parent, name, ext string) string {
	if !n.used[parent][shelfFoldName(name)] {
		n.add(parent, name)
		return name
	}
	if ext == name {
		ext = ""
	}
	base := strings.TrimSuffix(name, ext)
	key := parent + "\x00" + shelfFoldName(base) + "\x00" + shelfFoldName(ext)
	_, firstWidth := utf8.DecodeRuneInString(base)
	for i := max(2, n.next[key]); ; i++ {
		number := fmt.Sprintf(" %d", i)
		suffix := number + ext
		trimmed := base
		if firstWidth+len(suffix) > shelfMaxNameBytes {
			trimmed, suffix = name, number
		}
		for len(trimmed)+len(suffix) > shelfMaxNameBytes {
			_, width := utf8.DecodeLastRuneInString(trimmed)
			trimmed = trimmed[:len(trimmed)-width]
		}
		candidate := strings.TrimRightFunc(trimmed, unicode.IsSpace) + suffix
		if !n.used[parent][shelfFoldName(candidate)] {
			n.next[key] = i + 1
			n.add(parent, candidate)
			return candidate
		}
	}
}
func (d shelfDocument) checkRevision(expected uint64) error {
	if d.Revision != expected {
		return &shelfError{Status: 409, Code: "revision_conflict", Message: "The shelf changed. Reload it before editing.", Revision: d.Revision}
	}
	return nil
}
func (d shelfDocument) depth(parent string) int {
	n := 0
	for parent != "" {
		n++
		f, ok := d.Folders[parent]
		if !ok || n > shelfMaxFolderDepth {
			return shelfMaxFolderDepth + 1
		}
		parent = f.Parent
	}
	return n
}
func (d shelfDocument) subtreeDepth(id string) int {
	maxDepth := 0
	for child := range d.Folders {
		n := 0
		at := child
		for at != "" && n <= shelfMaxFolderDepth {
			if at == id {
				maxDepth = max(maxDepth, n)
				break
			}
			at = d.Folders[at].Parent
			n++
		}
	}
	return maxDepth
}
func (s *Server) changeShelf(expected uint64, edit func(*shelfDocument) error) (shelfDocument, error) {
	return s.changeShelfState(expected, func(tx *shelfTransaction) error { return edit(&tx.shelfDocument) })
}
func shelfExpected(r *http.Request) (uint64, error) {
	value := r.Header.Get("If-Match")
	if value == "" {
		return 0, shelfProblem(428, "revision_required", "If-Match with the shelf revision is required.")
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, shelfProblem(400, "revision_invalid", "If-Match must be a single shelf revision.")
	}
	return n, nil
}
func shelfAnswer(w http.ResponseWriter, status int, d shelfDocument) {
	shelfRevisionHeaders(w, d.Revision)
	writeJSON(w, status, d)
}
func shelfRevisionHeaders(w http.ResponseWriter, revision uint64) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(revision, 10)))
	w.Header().Set("Cache-Control", "no-store")
}
func (s *Server) shelfRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/shelf", s.listShelf)
	mux.HandleFunc("POST /api/shelf/uploads", s.uploadShelf)
	mux.HandleFunc("POST /api/shelf/folders", s.createShelfFolder)
	mux.HandleFunc("PATCH /api/shelf/folders/{id}", func(w http.ResponseWriter, r *http.Request) { s.editShelf(w, r, true) })
	mux.HandleFunc("PATCH /api/shelf/models/{id}", func(w http.ResponseWriter, r *http.Request) { s.editShelf(w, r, false) })
	mux.HandleFunc("DELETE /api/shelf/folders/{id}", func(w http.ResponseWriter, r *http.Request) { s.deleteShelf(w, r, true) })
	mux.HandleFunc("DELETE /api/shelf/models/{id}", func(w http.ResponseWriter, r *http.Request) { s.deleteShelf(w, r, false) })
	mux.HandleFunc("POST /api/shelf/trash/{token}/restore", s.restoreShelf)
	mux.HandleFunc("GET /api/shelf/models/{id}/thumbnail.png", s.shelfThumbnail)
	mux.HandleFunc("GET /api/shelf/models/{id}/geometry.stl", s.shelfGeometry)
	mux.HandleFunc("POST /api/shelf/models/{id}/add-to-plate", s.addShelfModelToPlate)
}
func (s *Server) listShelf(w http.ResponseWriter, r *http.Request) {
	e, pruned, err := s.shelfEnvelopeSnapshot(true)
	if err != nil {
		shelfFail(w, err)
		return
	}
	if pruned {
		go s.blobs.evict()
	}
	d := shelfOf(e)
	parent := r.URL.Query().Get("folder")
	if !d.parentExists(parent) {
		shelfFail(w, shelfProblem(404, "folder_missing", "Shelf folder not found."))
		return
	}
	if r.URL.Query().Get("tree") != "1" {
		for id, f := range d.Folders {
			if f.Parent != parent {
				delete(d.Folders, id)
			}
		}
		for id, m := range d.Models {
			if m.Parent != parent {
				delete(d.Models, id)
			}
		}
	}
	shelfAnswer(w, 200, d)
}
func (s *Server) createShelfFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedRevision *uint64 `json:"expectedRevision"`
		Parent           string  `json:"parent"`
		Name             string  `json:"name"`
	}
	if !workspaceDecode(w, r, &in) {
		return
	}
	if in.ExpectedRevision == nil || !shelfName(in.Name) {
		shelfFail(w, shelfProblem(400, "folder_invalid", "expectedRevision and a safe folder name are required."))
		return
	}
	var created shelfFolder
	d, err := s.changeShelf(*in.ExpectedRevision, func(d *shelfDocument) error {
		if !d.parentExists(in.Parent) {
			return shelfProblem(404, "folder_missing", "Parent shelf folder not found.")
		}
		if d.depth(in.Parent) >= shelfMaxFolderDepth {
			return shelfProblem(400, "folder_depth", "Shelf folders may be at most 32 levels deep.")
		}
		if d.nameTaken(in.Parent, in.Name, "") {
			return shelfProblem(409, "name_conflict", "That name is already in use in this folder.")
		}
		created = shelfFolder{ID: newID("folder"), Parent: in.Parent, Name: in.Name}
		d.Folders[created.ID] = created
		return nil
	})
	if err != nil {
		shelfFail(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(d.Revision, 10)))
	writeJSON(w, 201, map[string]any{"revision": d.Revision, "folder": created})
}
func (s *Server) editShelf(w http.ResponseWriter, r *http.Request, folder bool) {
	var in struct {
		ExpectedRevision *uint64 `json:"expectedRevision"`
		Name             *string `json:"name"`
		Parent           *string `json:"parent"`
	}
	if !workspaceDecode(w, r, &in) {
		return
	}
	if in.ExpectedRevision == nil || (in.Name == nil && in.Parent == nil) || in.Name != nil && !shelfName(*in.Name) {
		shelfFail(w, shelfProblem(400, "shelf_edit_invalid", "expectedRevision and a safe name or parent are required."))
		return
	}
	id := r.PathValue("id")
	d, err := s.changeShelf(*in.ExpectedRevision, func(d *shelfDocument) error {
		var name, parent string
		if folder {
			f, ok := d.Folders[id]
			if !ok {
				return shelfProblem(404, "folder_missing", "Shelf folder not found.")
			}
			name, parent = f.Name, f.Parent
		} else {
			m, ok := d.Models[id]
			if !ok {
				return shelfProblem(404, "model_missing", "Shelf model not found.")
			}
			name, parent = m.Name, m.Parent
		}
		if in.Name != nil {
			name = *in.Name
		}
		if in.Parent != nil {
			parent = *in.Parent
		}
		if !d.parentExists(parent) {
			return shelfProblem(404, "folder_missing", "Parent shelf folder not found.")
		}
		if d.nameTaken(parent, name, id) {
			return shelfProblem(409, "name_conflict", "That name is already in use in this folder.")
		}
		if folder {
			at := parent
			for at != "" {
				if at == id {
					return shelfProblem(409, "folder_cycle", "A folder cannot be moved inside itself.")
				}
				at = d.Folders[at].Parent
			}
			if d.depth(parent)+1+d.subtreeDepth(id) > shelfMaxFolderDepth {
				return shelfProblem(400, "folder_depth", "Shelf folders may be at most 32 levels deep.")
			}
			f := d.Folders[id]
			f.Parent, f.Name = parent, name
			d.Folders[id] = f
		} else {
			m := d.Models[id]
			m.Parent, m.Name = parent, name
			d.Models[id] = m
		}
		return nil
	})
	if err != nil {
		shelfFail(w, err)
		return
	}
	shelfAnswer(w, 200, d)
}
func (d shelfDocument) deletionTargets(id string, folder, recursive bool) (shelfDocument, error) {
	targets := emptyShelf()
	targets.Revision = d.Revision
	if !folder {
		model, ok := d.Models[id]
		if !ok {
			return targets, shelfProblem(404, "model_missing", "Shelf model not found.")
		}
		targets.Models[id] = model
		return targets, nil
	}
	root, ok := d.Folders[id]
	if !ok {
		return targets, shelfProblem(404, "folder_missing", "Shelf folder not found.")
	}
	children := map[string][]string{}
	for key, folder := range d.Folders {
		children[folder.Parent] = append(children[folder.Parent], key)
	}
	targets.Folders[id] = root
	queue := []string{id}
	for i := 0; i < len(queue); i++ {
		for _, child := range children[queue[i]] {
			if _, seen := targets.Folders[child]; !seen {
				targets.Folders[child] = d.Folders[child]
				queue = append(queue, child)
			}
		}
	}
	for key, model := range d.Models {
		if _, included := targets.Folders[model.Parent]; included {
			targets.Models[key] = model
		}
	}
	if !recursive && (len(targets.Folders) > 1 || len(targets.Models) > 0) {
		return targets, shelfProblem(409, "recursive_required", "This folder is not empty. Explicit recursive deletion is required.")
	}
	return targets, nil
}

func (s *Server) retainedShelfBlobs() (map[string]bool, error) {
	e, _, err := s.shelfEnvelopeSnapshot(true)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	retain := func(models map[string]shelfModel) error {
		for _, m := range models {
			if !blobDigest.MatchString(m.SHA256) {
				return errors.New("invalid shelf digest")
			}
			keep[m.SHA256] = true
		}
		return nil
	}
	if err := retain(shelfOf(e).Models); err != nil {
		return nil, err
	}
	for _, tombstone := range e.ShelfTrash {
		if err := retain(tombstone.Models); err != nil {
			return nil, err
		}
	}
	if s.workspace == nil {
		return keep, nil
	}
	entries, err := os.ReadDir(filepath.Join(s.blobs.root))
	if errors.Is(err, os.ErrNotExist) {
		return keep, nil
	}
	if err != nil {
		return nil, err
	}
	for _, shard := range entries {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.blobs.root, shard.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			sum := file.Name()
			if !blobDigest.MatchString(sum) {
				continue
			}
			_, err := os.Stat(s.workspace.assetPath("sha256-" + sum))
			if err == nil {
				keep[sum] = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	return keep, nil
}
func (s *Server) shelfSource(id string, expected *uint64) (shelfModel, string, func(), error) {
	s.blobs.mu.Lock()
	defer s.blobs.mu.Unlock()
	d, err := s.shelfSnapshot()
	if err != nil {
		return shelfModel{}, "", nil, err
	}
	if expected != nil {
		if err = d.checkRevision(*expected); err != nil {
			return shelfModel{}, "", nil, err
		}
	}
	model, ok := d.Models[id]
	if !ok {
		return shelfModel{}, "", nil, shelfProblem(404, "model_missing", "Shelf model not found.")
	}
	if !blobDigest.MatchString(model.SHA256) || !shelfModelFormat(model.Format) {
		return shelfModel{}, "", nil, errors.New("invalid shelf model")
	}
	dir, err := os.MkdirTemp("", "shelf-source-*")
	if err != nil {
		return shelfModel{}, "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	dst := filepath.Join(dir, "model"+model.Format)
	if err = s.blobs.stage(model.SHA256, dst, false); err != nil {
		cleanup()
		return shelfModel{}, "", nil, err
	}
	return model, dst, cleanup, nil
}
func copyShelfBlob(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		f, err := os.Open(dst)
		if err != nil {
			return err
		}
		err = errors.Join(f.Sync(), f.Close())
		if err != nil {
			return err
		}
		if err = syncWorkspaceDirectory(filepath.Dir(dst)); err != nil {
			return err
		}
		return syncWorkspaceDirectory(filepath.Dir(filepath.Dir(dst)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(filepath.Dir(dst), ".shelf-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, in)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Chmod(f.Name(), 0400); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), dst); err != nil {
		return err
	}
	if err = syncWorkspaceDirectory(filepath.Dir(dst)); err != nil {
		return err
	}
	return syncWorkspaceDirectory(filepath.Dir(filepath.Dir(dst)))
}
