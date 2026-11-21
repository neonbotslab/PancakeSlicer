package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	mcpFixtureProjectID       = "saved-project"
	mcpFixtureProjectRevision = 7
)

func mcpSaveProjectFixture(t *testing.T, store *workspaceStore) {
	t.Helper()
	data := newWorkspaceData()
	data.Projects[mcpFixtureProjectID] = workspaceProject{
		Revision: mcpFixtureProjectRevision,
		Record: json.RawMessage(`{
			"meta":{"id":"saved-project","name":"Fixture project","savedAt":100,"objects":2,"revision":1},
			"doc":{"plateIndex":2,"objects":[{"id":"fixture"}],"events":[{"type":"pause"}],
				"dirty":{"process":{"nonplanar_mode":"slant"}},
				"projectPlates":[
					{"index":1,"name":"First plate","objects":[{"id":"first"}],"events":[]},
					{"index":2,"name":"Edited plate","objects":[],"events":[{"type":"stale"}]}
				]}
		}`),
	}
	data.Projects["archived-project"] = workspaceProject{
		Revision: 3, Archived: true,
		Record: json.RawMessage(`{"meta":{"id":"archived-project","name":"Archived","savedAt":90,"objects":0},"doc":{}}`),
	}
	writer := &workspaceStore{root: store.root}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writer.save(data); err != nil {
		t.Fatal(err)
	}
}

func mcpRequireUnchangedWorkspace(t *testing.T, store *workspaceStore) {
	t.Helper()
	path := filepath.Join(store.root, "workspace.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("MCP calls changed the saved workspace file")
		}
	})
}

func TestMCPSavedProjectReads(t *testing.T) {
	fixture := newMCPPrintFixture(t)
	mcpSaveProjectFixture(t, fixture.server.workspace)
	mcpRequireUnchangedWorkspace(t, fixture.server.workspace)
	session := fixture.connect(t, nil)
	input := map[string]string{"project_id": mcpFixtureProjectID}
	type projectMeta struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Revision uint64 `json:"revision"`
	}
	wantProject := projectMeta{mcpFixtureProjectID, "Fixture project", mcpFixtureProjectRevision}
	var listing struct {
		Projects []projectMeta `json:"projects"`
	}
	mcpTestCall(t, session, "list_projects", nil, &listing)
	if !slices.Equal(listing.Projects, []projectMeta{wantProject}) {
		t.Fatalf("list_projects = %+v, want only %+v", listing.Projects, wantProject)
	}
	var project struct {
		ID       string      `json:"project_id"`
		Revision uint64      `json:"project_revision"`
		Meta     projectMeta `json:"meta"`
		Doc      struct {
			ActivePlate int `json:"plateIndex"`
		} `json:"doc"`
	}
	mcpTestCall(t, session, "get_project", input, &project)
	if project.ID != mcpFixtureProjectID || project.Revision != mcpFixtureProjectRevision || project.Meta != wantProject || project.Doc.ActivePlate != 2 {
		t.Fatalf("get_project = %+v", project)
	}
	type plateSummary struct {
		Index       int    `json:"plate_index"`
		Name        string `json:"name"`
		ObjectCount int    `json:"object_count"`
	}
	var plates struct {
		Revision    uint64         `json:"project_revision"`
		ActivePlate int            `json:"active_plate_index"`
		Plates      []plateSummary `json:"plates"`
	}
	mcpTestCall(t, session, "list_plates", input, &plates)
	if plates.Revision != mcpFixtureProjectRevision || plates.ActivePlate != 2 || !slices.Equal(plates.Plates, []plateSummary{{1, "First plate", 1}, {2, "Edited plate", 1}}) {
		t.Fatalf("list_plates = %+v", plates)
	}
	for _, tc := range []struct {
		index int
		id    string
	}{{1, "first"}, {2, "fixture"}} {
		var plate struct {
			Revision uint64 `json:"project_revision"`
			Index    int    `json:"plate_index"`
			Plate    struct {
				Objects []struct{ ID string }   `json:"objects"`
				Events  []struct{ Type string } `json:"events"`
			} `json:"plate"`
		}
		mcpTestCall(t, session, "get_plate", map[string]any{"project_id": mcpFixtureProjectID, "plate_index": tc.index}, &plate)
		if plate.Revision != mcpFixtureProjectRevision || plate.Index != tc.index || len(plate.Plate.Objects) != 1 || plate.Plate.Objects[0].ID != tc.id {
			t.Fatalf("get_plate(%d) = %+v", tc.index, plate)
		}
		if tc.index == 2 && (len(plate.Plate.Events) != 1 || plate.Plate.Events[0].Type != "pause") {
			t.Fatalf("get_plate(%d) events = %+v, want active plate's pause", tc.index, plate.Plate.Events)
		}
	}
	for _, id := range []string{"missing-project", "archived-project"} {
		mcpTestRefusal(t, mcpTestCall(t, session, "get_project", map[string]string{"project_id": id}, nil), "not found")
	}
	mcpTestRefusal(t, mcpTestCall(t, session, "get_plate", map[string]any{"project_id": mcpFixtureProjectID, "plate_index": 3}, nil), "no plate 3")
}

func TestMCPSavedProjectSliceIsolation(t *testing.T) {
	fixture := newMCPPrintFixture(t)
	store := fixture.server.workspace
	mcpSaveProjectFixture(t, store)
	mcpRequireUnchangedWorkspace(t, store)
	session := fixture.connect(t, nil)
	const model = "solid fixture\nfacet normal 0 0 1\nouter loop\nvertex 5 5 0\nvertex 10 5 0\nvertex 5 10 0\nendloop\nendfacet\nendsolid fixture\n"
	var file mcpBlobRef
	mcpTestCall(t, session, "upload_model", map[string]string{
		"name": "fixture.stl", "data": base64.StdEncoding.EncodeToString([]byte(model)),
	}, &file)
	slice := map[string]any{
		"project_id": mcpFixtureProjectID, "project_revision": mcpFixtureProjectRevision - 1, "plate_index": 2,
		"library": map[string]any{"printer": mcpFixturePrinter, "print": "profile:process", "filaments": []string{"profile:filament"}, "slots": []string{"slot:filament"}},
		"files":   []mcpBlobRef{file},
	}
	mcpTestRefusal(t, mcpTestCall(t, session, "slice_plate", slice, nil), "revision changed")
	slice["project_revision"] = mcpFixtureProjectRevision
	mcpTestRefusal(t, mcpTestCall(t, session, "slice_plate", slice, nil), "prepared layout")
	slice["plate"] = json.RawMessage(`{"objects":[{"file":"fixture.stl","name":"fixture","extruder":1,"instances":[{"x":25,"y":30,"z":0,"rot":0,"printable":true}]}]}`)
	jobs := fixture.server.jobs
	jobs.mu.Lock()
	workers := jobs.cfg.MaxJobs
	jobs.cfg.MaxJobs = 0
	jobs.mu.Unlock()
	var accepted struct {
		ID       string `json:"id"`
		Project  string `json:"project_id"`
		Revision uint64 `json:"project_revision"`
		Plate    int    `json:"plate_index"`
	}
	mcpTestCall(t, session, "slice_plate", slice, &accepted)
	if accepted.ID == "" || accepted.Project != mcpFixtureProjectID || accepted.Revision != mcpFixtureProjectRevision || accepted.Plate != 2 {
		t.Fatalf("slice_plate = %+v", accepted)
	}
	wantSource := jobSourceProject{ID: mcpFixtureProjectID, Revision: mcpFixtureProjectRevision, Plate: 2}
	type sliceJob struct {
		State       JobState          `json:"lifecycle_state"`
		Source      *jobSourceProject `json:"source_project"`
		DownloadURL string            `json:"download_url"`
		Error       string            `json:"error"`
	}
	readJob := func() sliceJob {
		var job sliceJob
		mcpTestCall(t, session, "get_slice_job", map[string]string{"job_id": accepted.ID}, &job)
		if job.Source == nil || *job.Source != wantSource {
			t.Fatalf("get_slice_job(%s) in state %q source = %+v, want %+v", accepted.ID, job.State, job.Source, wantSource)
		}
		return job
	}
	if job := readJob(); job.State != StateQueued {
		t.Fatalf("slice job state = %q, want queued", job.State)
	}
	jobs.mu.Lock()
	jobs.cfg.MaxJobs = workers
	jobs.dispatchLocked()
	jobs.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job := readJob()
		if job.State == StateDone {
			if job.DownloadURL != "/api/jobs/"+accepted.ID+"/download" {
				t.Fatalf("completed slice download URL = %q", job.DownloadURL)
			}
			break
		}
		if job.State.Finished() || time.Now().After(deadline) {
			t.Fatalf("slice job ended in %q: %s", job.State, job.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMCPUploadAndCatalogValidation(t *testing.T) {
	fixture := newMCPPrintFixture(t)
	session := fixture.connect(t, nil)
	limit := int(fixture.server.cfg.MaxUploadSize)
	model := bytes.Repeat([]byte("x"), limit+1)
	var upload struct {
		Size int `json:"size"`
	}
	mcpTestCall(t, session, "upload_model", map[string]string{
		"name": "fixture.stl", "data": base64.StdEncoding.EncodeToString(model[:limit]),
	}, &upload)
	if upload.Size != limit {
		t.Fatalf("upload_model size = %d, want %d", upload.Size, limit)
	}
	for _, tc := range []struct {
		name, data, want string
	}{
		{"../fixture.stl", "AA==", "file basename"},
		{"fixture.stl", "not base64!", "standard base64"},
		{"fixture.stl", base64.StdEncoding.EncodeToString(model), "upload limit"},
	} {
		mcpTestRefusal(t, mcpTestCall(t, session, "upload_model", map[string]string{"name": tc.name, "data": tc.data}, nil), tc.want)
	}
	type profile struct {
		ID string `json:"id"`
	}
	var profiles struct {
		Printers  []profile `json:"printers"`
		Processes []profile `json:"processes"`
		Filaments []profile `json:"filaments"`
	}
	mcpTestCall(t, session, "list_profiles", nil, &profiles)
	if !slices.Equal(profiles.Printers, []profile{{mcpFixturePrinter}}) || !slices.Equal(profiles.Processes, []profile{{"profile:process"}}) || !slices.Equal(profiles.Filaments, []profile{{"profile:filament"}}) {
		t.Fatalf("list_profiles = %+v", profiles)
	}
	var product struct {
		Version string  `json:"version"`
		Profile profile `json:"profile"`
	}
	mcpTestCall(t, session, "get_catalog_profile", map[string]string{"kind": "filaments", "id": "Filament"}, &product)
	if product.Version != "fixture" || product.Profile.ID != "Filament" {
		t.Fatalf("get_catalog_profile = %+v", product)
	}
	mcpTestRefusal(t, mcpTestCall(t, session, "get_catalog_profile", map[string]string{"kind": "unknown", "id": "Filament"}, nil), "not found")
	for _, tc := range []struct {
		query string
		want  []profile
	}{{" filament ", []profile{{"Filament"}}}, {"unknown", []profile{}}} {
		var catalog struct {
			Total     int       `json:"total"`
			Filaments []profile `json:"filaments"`
		}
		catalog.Total = -1
		mcpTestCall(t, session, "list_catalog_filaments", map[string]string{"query": tc.query}, &catalog)
		if catalog.Total != len(tc.want) || catalog.Filaments == nil || !slices.Equal(catalog.Filaments, tc.want) {
			t.Fatalf("list_catalog_filaments(%q) = %+v, want %+v", tc.query, catalog, tc.want)
		}
	}
}
