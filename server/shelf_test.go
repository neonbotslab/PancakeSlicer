package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const shelfTestSTL = "solid sample\nfacet normal 0 0 1\nouter loop\nvertex 0 0 0\nvertex 15 0 0\nvertex 0 15 10\nendloop\nendfacet\nendsolid sample\n"

func shelfFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	return shelfFixtureAt(t, t.TempDir())
}
func shelfFixtureAt(t *testing.T, root string) (*Server, http.Handler) {
	t.Helper()
	catalog := mcpFixtureCatalog(t)
	s := &Server{cfg: Config{DataDir: root, MaxUploadSize: 8 << 20}, catalog: catalog, library: &libraryStore{dir: root, catalog: catalog, settled: true}, workspace: &workspaceStore{root: filepath.Join(root, "workspace")}, blobs: &blobStore{root: filepath.Join(root, "blobs"), limit: blobCacheBytes}}
	s.blobs.retained = s.retainedShelfBlobs
	mux := http.NewServeMux()
	s.shelfRoutes(mux)
	s.libraryRoutes(mux)
	return s, s.middleware(mux)
}
func shelfJSON(t *testing.T, h http.Handler, method, url string, body any, revision *uint64) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, url, reader)
	r.Header.Set("Content-Type", "application/json")
	if revision != nil {
		r.Header.Set("If-Match", fmt.Sprintf("\"%d\"", *revision))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func shelfUpload(t *testing.T, h http.Handler, revision uint64, folder string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, data := range files {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/shelf/uploads?folder="+folder, &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("If-Match", fmt.Sprintf("\"%d\"", revision))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func shelfZIP(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	z := zip.NewWriter(&raw)
	for name, data := range entries {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}
func shelfRequireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
}
func TestShelfZIPFoldersDedupAndThumbnails(t *testing.T) {
	s, h := shelfFixture(t)
	data := shelfZIP(t, map[string][]byte{"parts/sub/model.stl": []byte(shelfTestSTL), "parts/copy.stl": []byte(shelfTestSTL), "empty/": nil, "readme.txt": []byte("skip")})
	w := shelfUpload(t, h, 0, "", map[string][]byte{"models.zip": data})
	shelfRequireStatus(t, w, 201)
	var report struct {
		Revision uint64          `json:"revision"`
		Imported []shelfImported `json:"imported"`
		Skipped  []shelfSkipped  `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Revision != 1 || len(report.Imported) != 2 || len(report.Skipped) != 1 || report.Skipped[0].Path != "models.zip/readme.txt" {
		t.Fatalf("report %+v", report)
	}
	d, err := s.shelfSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]shelfFolder{}
	for _, f := range d.Folders {
		names[f.Name] = f
	}
	if len(names) != 4 || names["models"].Parent != "" || names["sub"].Parent != names["parts"].ID || names["empty"].Parent != names["models"].ID || names["parts"].Parent != names["models"].ID {
		t.Fatalf("folders %+v", names)
	}
	sums := map[string]bool{}
	for _, m := range d.Models {
		sums[m.SHA256] = true
		if m.Name == "model.stl" && m.Parent != names["sub"].ID {
			t.Fatalf("lost subfolder %+v", m)
		}
	}
	if len(sums) != 1 {
		t.Fatal("identical models did not deduplicate")
	}
	entries := 0
	filepath.WalkDir(s.blobs.root, func(_ string, f os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if blobDigest.MatchString(f.Name()) {
			entries++
		}
		return nil
	})
	if entries != 1 {
		t.Fatalf("blob files %d", entries)
	}
	model := report.Imported[0].Model
	thumbnailURL := "/api/shelf/models/" + model.ID + "/thumbnail.png"
	w = shelfJSON(t, h, "GET", thumbnailURL, nil, nil)
	shelfRequireStatus(t, w, 200)
	image, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if image.Bounds().Dx() < 256 || image.Bounds().Dx() > 512 {
		t.Fatalf("thumbnail bounds %v", image.Bounds())
	}
	bg := image.At(0, 0)
	seen := false
	for y := 0; y < image.Bounds().Dy(); y++ {
		for x := 0; x < image.Bounds().Dx(); x++ {
			if image.At(x, y) != bg {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatal("thumbnail has no rendered geometry")
	}
	before := append([]byte{}, w.Body.Bytes()...)
	if err = os.Remove(s.shelfThumbnailPath(model.SHA256, model.Format)); err != nil {
		t.Fatal(err)
	}
	w = shelfJSON(t, h, "GET", thumbnailURL, nil, nil)
	shelfRequireStatus(t, w, 200)
	if !bytes.Equal(before, w.Body.Bytes()) {
		t.Fatal("lazy thumbnail differs")
	}
	req := httptest.NewRequest("GET", thumbnailURL, nil)
	req.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	h.ServeHTTP(cached, req)
	shelfRequireStatus(t, cached, 304)
	w = shelfUpload(t, h, 1, names["parts"].ID, map[string][]byte{"copy.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	d, _ = s.shelfSnapshot()
	found := false
	for _, m := range d.Models {
		found = found || m.Name == "copy 2.stl"
	}
	if !found {
		t.Fatal("collision did not use numbered suffix")
	}
}
func TestShelfZIPRejectsUnsafeEntriesAtomically(t *testing.T) {
	for _, name := range []string{"../escape.stl", "/absolute.stl", "folder/../escape.stl", "C:/escape.stl", "folder\\escape.stl", "folder//escape.stl", "./escape.stl"} {
		t.Run(name, func(t *testing.T) {
			s, h := shelfFixture(t)
			w := shelfUpload(t, h, 0, "", map[string][]byte{"unsafe.zip": shelfZIP(t, map[string][]byte{"safe.stl": []byte(shelfTestSTL), name: []byte(shelfTestSTL)})})
			shelfRequireStatus(t, w, 400)
			d, _ := s.shelfSnapshot()
			if d.Revision != 0 || len(d.Models) != 0 {
				t.Fatalf("partial import %+v", d)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		s, h := shelfFixture(t)
		var body bytes.Buffer
		z := zip.NewWriter(&body)
		header := &zip.FileHeader{Name: "link.stl", Method: zip.Store}
		header.SetMode(os.ModeSymlink | 0777)
		f, _ := z.CreateHeader(header)
		f.Write([]byte("target"))
		z.Close()
		w := shelfUpload(t, h, 0, "", map[string][]byte{"link.zip": body.Bytes()})
		shelfRequireStatus(t, w, 400)
		d, _ := s.shelfSnapshot()
		if len(d.Models) != 0 {
			t.Fatal("symlink imported")
		}
	})
	t.Run("invalid model", func(t *testing.T) {
		s, h := shelfFixture(t)
		w := shelfUpload(t, h, 0, "", map[string][]byte{"invalid.zip": shelfZIP(t, map[string][]byte{"good.stl": []byte(shelfTestSTL), "bad.stl": []byte("invalid")})})
		shelfRequireStatus(t, w, 400)
		d, _ := s.shelfSnapshot()
		if len(d.Models) != 0 {
			t.Fatal("partial invalid import")
		}
	})
}
func TestShelfZIPLimits(t *testing.T) {
	cases := []struct {
		name    string
		limits  shelfLimits
		entries map[string][]byte
		status  int
	}{
		{"count", shelfLimits{FileBytes: 1024, TotalBytes: 4096, Entries: 1, Ratio: 200}, map[string][]byte{"one.stl": []byte(shelfTestSTL), "two.stl": []byte(shelfTestSTL)}, 413},
		{"wrapper count", shelfLimits{FileBytes: 1024, TotalBytes: 4096, Entries: 1, Ratio: 200}, map[string][]byte{"one.stl": []byte(shelfTestSTL)}, 413},
		{"file", shelfLimits{FileBytes: 8, TotalBytes: 4096, Entries: 10, Ratio: 200}, map[string][]byte{"one.stl": []byte(shelfTestSTL)}, 413},
		{"total", shelfLimits{FileBytes: 1024, TotalBytes: int64(len(shelfTestSTL) + 1), Entries: 10, Ratio: 200}, map[string][]byte{"one.stl": []byte(shelfTestSTL), "two.stl": []byte(shelfTestSTL)}, 413},
		{"ratio", shelfLimits{FileBytes: 1 << 20, TotalBytes: 2 << 20, Entries: 10, Ratio: 10}, map[string][]byte{"bomb.txt": bytes.Repeat([]byte("a"), 65536)}, 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "models.zip")
			os.WriteFile(file, shelfZIP(t, tc.entries), 0600)
			batch := shelfImportBatch{}
			err := batch.expandZIP(t.Context(), file, "models.zip", dir, tc.limits)
			failure, ok := err.(*shelfError)
			if !ok || failure.Status != tc.status {
				t.Fatalf("error %v", err)
			}
		})
	}
	t.Run("request", func(t *testing.T) {
		_, h := shelfFixture(t)
		w := shelfUpload(t, h, 0, "", map[string][]byte{"huge.zip": bytes.Repeat([]byte("z"), 9<<20)})
		shelfRequireStatus(t, w, 413)
	})
}
func TestShelfTreeMutationsAndConcurrentRevision(t *testing.T) {
	s, h := shelfFixture(t)
	w := shelfJSON(t, h, "POST", "/api/shelf/folders", map[string]any{"expectedRevision": 0, "name": "A"}, nil)
	shelfRequireStatus(t, w, 201)
	var response struct {
		Folder shelfFolder `json:"folder"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	a := response.Folder.ID
	w = shelfJSON(t, h, "POST", "/api/shelf/folders", map[string]any{"expectedRevision": 1, "parent": a, "name": "B"}, nil)
	shelfRequireStatus(t, w, 201)
	json.Unmarshal(w.Body.Bytes(), &response)
	b := response.Folder.ID
	w = shelfJSON(t, h, "PATCH", "/api/shelf/folders/"+a, map[string]any{"expectedRevision": 2, "parent": b}, nil)
	shelfRequireStatus(t, w, 409)
	w = shelfUpload(t, h, 2, b, map[string][]byte{"one.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	d, _ := s.shelfSnapshot()
	var model shelfModel
	for _, m := range d.Models {
		model = m
	}
	rev := uint64(3)
	w = shelfJSON(t, h, "DELETE", "/api/shelf/folders/"+a, nil, nil)
	shelfRequireStatus(t, w, 428)
	w = shelfJSON(t, h, "DELETE", "/api/shelf/folders/"+a, nil, &rev)
	shelfRequireStatus(t, w, 409)
	w = shelfJSON(t, h, "PATCH", "/api/shelf/models/"+model.ID, map[string]any{"expectedRevision": 3, "parent": "", "name": "renamed.obj"}, nil)
	shelfRequireStatus(t, w, 200)
	d, _ = s.shelfSnapshot()
	if d.Models[model.ID].Format != ".stl" {
		t.Fatal("rename changed content parser format")
	}
	w = shelfJSON(t, h, "GET", "/api/shelf?folder="+a, nil, nil)
	shelfRequireStatus(t, w, 200)
	var listing shelfDocument
	json.Unmarshal(w.Body.Bytes(), &listing)
	if len(listing.Folders) != 1 || len(listing.Models) != 0 {
		t.Fatalf("folder listing %+v", listing)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, name := range []string{"C", "D"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			codes <- shelfJSON(t, h, "POST", "/api/shelf/folders", map[string]any{"expectedRevision": 4, "name": name}, nil).Code
		}(name)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent status %v", counts)
	}
	rev = 5
	w = shelfJSON(t, h, "DELETE", "/api/shelf/folders/"+a+"?recursive=true", nil, &rev)
	shelfRequireStatus(t, w, 200)
	d, _ = s.shelfSnapshot()
	if len(d.Models) != 1 || len(d.Folders) != 1 {
		t.Fatalf("delete touched moved model %+v", d)
	}
}
func TestShelfOldLibraryFixtureAndProfilePreservation(t *testing.T) {
	s, h := shelfFixture(t)
	raw := []byte(`{"document":{"schema":13,"revision":7,"epoch":"old","printers":[],"filaments":[],"processes":[],"calibrations":[]},"operations":{},"history":{}}`)
	p := filepath.Join(s.library.dir, libraryFile)
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	w := shelfJSON(t, h, "GET", "/api/shelf?tree=1", nil, nil)
	shelfRequireStatus(t, w, 200)
	after, _ := os.ReadFile(p)
	if !bytes.Equal(raw, after) {
		t.Fatal("reading old library wrote metadata")
	}
	w = shelfUpload(t, h, 0, "", map[string][]byte{"model.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	var before, updated libraryEnvelope
	json.Unmarshal(raw, &before)
	after, _ = os.ReadFile(p)
	json.Unmarshal(after, &updated)
	b, _ := json.Marshal(before.Document)
	a, _ := json.Marshal(updated.Document)
	if !bytes.Equal(a, b) || updated.Writer != nil {
		t.Fatal("shelf upload changed profile document or writer")
	}
	updated.Document["revision"] = json.RawMessage("8")
	if err := s.library.write(&updated); err != nil {
		t.Fatal(err)
	}
	d, _ := s.shelfSnapshot()
	if d.Revision != 1 || len(d.Models) != 1 {
		t.Fatal("profile save dropped shelf")
	}
}
func TestShelfEvictionRetainsShelfAndProjectAssets(t *testing.T) {
	s, h := shelfFixture(t)
	s.blobs.limit = 1
	w := shelfUpload(t, h, 0, "", map[string][]byte{"keep.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	d, _ := s.shelfSnapshot()
	var model shelfModel
	for _, m := range d.Models {
		model = m
	}
	s.blobs.evict()
	if _, err := os.Stat(s.blobs.path(model.SHA256)); err != nil {
		t.Fatal("evicted shelf blob", err)
	}
	projectSource := filepath.Join(t.TempDir(), "project.stl")
	os.WriteFile(projectSource, []byte(shelfTestSTL+"\n"), 0600)
	sum, _ := mcpFileHash(projectSource)
	copyShelfBlob(projectSource, s.blobs.path(sum))
	copyShelfBlob(projectSource, s.workspace.assetPath("sha256-"+sum))
	s.blobs.evict()
	if _, err := os.Stat(s.blobs.path(sum)); err != nil {
		t.Fatal("evicted project asset blob", err)
	}
	removed, err := s.shelfRemove(model.ID, false, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	s.blobs.evict()
	if _, err := os.Stat(s.blobs.path(model.SHA256)); err != nil {
		t.Fatal("evicted undoable shelf blob", err)
	}
	shelfTestExpire(t, s, removed.Undo)
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		s.blobs.evict()
		if _, err := os.Stat(s.blobs.path(model.SHA256)); os.IsNotExist(err) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(s.blobs.path(model.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("unreferenced blob retained %v", err)
	}
	if _, err := os.Stat(s.blobs.path(sum)); err != nil {
		t.Fatal("project asset collected after shelf delete", err)
	}
	t.Run("metadata failure closes GC", func(t *testing.T) {
		orphan := filepath.Join(t.TempDir(), "orphan")
		os.WriteFile(orphan, []byte("orphan"), 0600)
		digest, _ := mcpFileHash(orphan)
		copyShelfBlob(orphan, s.blobs.path(digest))
		os.WriteFile(filepath.Join(s.library.dir, libraryFile), []byte("{"), 0600)
		s.blobs.evict()
		if _, err := os.Stat(s.blobs.path(digest)); err != nil {
			t.Fatal("GC deleted bytes with unreadable metadata")
		}
	})
}
func TestShelfAuthorizationAndMissingRevision(t *testing.T) {
	s, h := shelfFixture(t)
	s.cfg.BearerToken = "fixture-access"
	for _, endpoint := range []string{"/api/shelf", "/api/shelf?tree=1", "/api/shelf/models/model:missing/thumbnail.png"} {
		w := shelfJSON(t, h, "GET", endpoint, nil, nil)
		shelfRequireStatus(t, w, 401)
	}
	w := shelfJSON(t, h, "POST", "/api/shelf/folders", map[string]any{"expectedRevision": 0, "name": "private"}, nil)
	shelfRequireStatus(t, w, 401)
	r := httptest.NewRequest("GET", "/api/shelf", nil)
	r.Header.Set("Authorization", "Bearer fixture-access")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	shelfRequireStatus(t, w, 200)
	s.cfg.BearerToken = ""
	for _, header := range []string{"*", "W/\"0\"", "\"0\", \"1\"", "-1"} {
		r := httptest.NewRequest("DELETE", "/api/shelf/models/nope", nil)
		r.Header.Set("If-Match", header)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		shelfRequireStatus(t, w, 400)
	}
	r = httptest.NewRequest("POST", "/api/shelf/uploads", strings.NewReader("body"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	shelfRequireStatus(t, w, 428)
}

func TestShelfUploadLongDirectoryCollision(t *testing.T) {
	s, h := shelfFixture(t)
	w := shelfUpload(t, h, 0, "", map[string][]byte{"original.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	d, _ := s.shelfSnapshot()
	var id string
	for key := range d.Models {
		id = key
	}
	name := "." + strings.Repeat("x", 235)
	w = shelfJSON(t, h, "PATCH", "/api/shelf/models/"+id, map[string]any{"expectedRevision": 1, "name": name}, nil)
	shelfRequireStatus(t, w, 200)
	data := shelfZIP(t, map[string][]byte{"part.stl": []byte(shelfTestSTL)})
	w = shelfUpload(t, h, 2, "", map[string][]byte{name + ".zip": data})
	shelfRequireStatus(t, w, 201)
	d, _ = s.shelfSnapshot()
	if len(d.Folders) != 1 || len(d.Models) != 2 {
		t.Fatalf("collision import failed %+v", d)
	}
	for _, folder := range d.Folders {
		if !shelfName(folder.Name) || folder.Name != name+" 2" {
			t.Fatalf("unsafe collision suffix %q", folder.Name)
		}
	}
}

func TestShelfEdgeOnPreviewAndFormatCache(t *testing.T) {
	s, h := shelfFixture(t)
	edgeOn := []byte("solid edge\nfacet normal 1 -1 0\nouter loop\nvertex 0 0 0\nvertex 10 10 0\nvertex 0 0 10\nendloop\nendfacet\nendsolid edge\n")
	w := shelfUpload(t, h, 0, "", map[string][]byte{"edge.stl": edgeOn})
	shelfRequireStatus(t, w, 201)
	var report struct {
		Imported []shelfImported `json:"imported"`
	}
	json.Unmarshal(w.Body.Bytes(), &report)
	if len(report.Imported) != 1 || report.Imported[0].Preview != "placeholder" {
		t.Fatalf("edge-on upload %+v", report)
	}
	model := report.Imported[0].Model
	w = shelfJSON(t, h, "GET", "/api/shelf/models/"+model.ID+"/thumbnail.png", nil, nil)
	shelfRequireStatus(t, w, 200)
	if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.shelfThumbnailPath(model.SHA256, model.Format)); err != nil {
		t.Fatal(err)
	}
	w = shelfJSON(t, h, "GET", "/api/shelf/models/"+model.ID+"/thumbnail.png", nil, nil)
	shelfRequireStatus(t, w, 200)
	w = shelfUpload(t, h, 1, "", map[string][]byte{"visible.stl": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	json.Unmarshal(w.Body.Bytes(), &report)
	visible := report.Imported[0].Model
	before := shelfJSON(t, h, "GET", "/api/shelf/models/"+visible.ID+"/thumbnail.png", nil, nil)
	shelfRequireStatus(t, before, 200)
	w = shelfUpload(t, h, 2, "", map[string][]byte{"unavailable.step": []byte(shelfTestSTL)})
	shelfRequireStatus(t, w, 201)
	json.Unmarshal(w.Body.Bytes(), &report)
	step := report.Imported[0].Model
	if step.SHA256 != visible.SHA256 {
		t.Fatal("fixture does not exercise a shared blob")
	}
	after := shelfJSON(t, h, "GET", "/api/shelf/models/"+visible.ID+"/thumbnail.png", nil, nil)
	shelfRequireStatus(t, after, 200)
	if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatal("STEP placeholder replaced the STL preview")
	}
}

func TestShelfZIPMaximumFolderDepth(t *testing.T) {
	s, h := shelfFixture(t)
	folder := strings.TrimSuffix(strings.Repeat("a/", 31), "/")
	data := shelfZIP(t, map[string][]byte{folder + "/model.stl": []byte(shelfTestSTL)})
	w := shelfUpload(t, h, 0, "", map[string][]byte{"deep.zip": data})
	shelfRequireStatus(t, w, 201)
	d, _ := s.shelfSnapshot()
	if len(d.Folders) != 32 || len(d.Models) != 1 {
		t.Fatalf("maximum-depth folders not retained %+v", d)
	}
	tooDeep := shelfZIP(t, map[string][]byte{folder + "/a/model.stl": []byte(shelfTestSTL)})
	w = shelfUpload(t, h, 1, "", map[string][]byte{"deep.zip": tooDeep})
	shelfRequireStatus(t, w, 400)
	for _, f := range d.Folders {
		if d.depth(f.ID) == 32 {
			w = shelfUpload(t, h, 1, f.ID, map[string][]byte{"empty.zip": shelfZIP(t, nil)})
			shelfRequireStatus(t, w, 400)
		}
	}
}

func TestShelfLimitsImplicitFolderExpansion(t *testing.T) {
	s, h := shelfFixture(t)
	entries := map[string][]byte{}
	for i := 0; i < 65; i++ {
		folder := fmt.Sprintf("branch-%d/", i) + strings.Repeat("nested/", 30)
		entries[folder+"part.stl"] = []byte(shelfTestSTL)
	}
	w := shelfUpload(t, h, 0, "", map[string][]byte{"expanded.zip": shelfZIP(t, entries)})
	shelfRequireStatus(t, w, 413)
	var failure struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &failure)
	if failure.Code != "upload_tree_limit" {
		t.Fatalf("unexpected limit response %s", w.Body)
	}
	d, _ := s.shelfSnapshot()
	if d.Revision != 0 || len(d.Models) != 0 || len(d.Folders) != 0 {
		t.Fatal("expanded tree partially imported")
	}
}

func TestShelfUnicodeCollisionsAndDistinctArchives(t *testing.T) {
	s, h := shelfFixture(t)
	for i, name := range []string{"Σ.stl", "ς.stl", "σ.stl"} {
		w := shelfUpload(t, h, uint64(i), "", map[string][]byte{name: []byte(shelfTestSTL)})
		shelfRequireStatus(t, w, 201)
	}
	d, _ := s.shelfSnapshot()
	names := map[string]bool{}
	for _, model := range d.Models {
		names[model.Name] = true
	}
	for _, name := range []string{"Σ.stl", "ς 2.stl", "σ 3.stl"} {
		if !names[name] {
			t.Fatalf("missing folded collision name %q in %v", name, names)
		}
	}
	w := shelfUpload(t, h, 3, "", map[string][]byte{
		"folder.zip": shelfZIP(t, map[string][]byte{"Σ/model.stl": []byte(shelfTestSTL), "ς/model.stl": []byte(shelfTestSTL)}),
		"folder.ZIP": shelfZIP(t, map[string][]byte{"Σ/model.stl": []byte(shelfTestSTL), "docs/notes.txt": []byte("skip")}),
		"empty.zip":  shelfZIP(t, nil),
	})
	shelfRequireStatus(t, w, 201)
	d, _ = s.shelfSnapshot()
	roots, nestedNames := map[string]bool{}, map[string]int{}
	for _, f := range d.Folders {
		if f.Parent == "" {
			roots[f.Name] = true
		} else {
			nestedNames[shelfFoldName(f.Name)]++
		}
	}
	if len(d.Folders) != 7 || len(d.Models) != 6 || len(roots) != 3 || !roots["folder"] || !roots["folder 2"] || !roots["empty"] || nestedNames[shelfFoldName("Σ")] != 2 || nestedNames[shelfFoldName("Σ 2")] != 1 {
		t.Fatalf("archives or archive paths merged %+v", d)
	}
}
