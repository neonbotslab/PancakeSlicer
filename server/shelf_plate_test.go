package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

const shelfTestProject = "project-fixture"
const shelfTestSession = "browser-test-session"

const shelfTestProjectRecord = `{
  "futureRecord":{"preserve":true},
  "meta":{"id":"project-fixture","name":"Native fixture","savedAt":200,"objects":2,"futureMeta":"kept"},
  "doc":{
    "project":"original-native", "files":{"original-native":"original.3mf"},
    "plateIndex":2, "events":[{"type":"pause","z":6}],
    "dirty":{"machine":{"printable_area":"-40x20,160x20,160x100,-40x100"}},
    "futureDoc":{"vendor":"kept"},
    "objects":[{"id":20,"source":"original-native","rotX":0,"rotY":0,"scale":[1,1,1],"instances":[{"id":21,"x":30,"y":60,"rotZ":0}],"project":{"buildIndex":3},"futureObject":[1,2]}],
    "projectPlates":[
      {"index":1,"objects":[{"id":95,"source":"original-native","instances":[{"id":96}],"volumes":[{"id":180}]}],"events":[{"type":"custom"}],"futurePlate":"kept"},
      {"index":2,"objects":[{"id":150,"instances":[{"id":151}]}],"origin":[130,250],"futurePlate":2},
      {"index":3,"locked":true,"objects":[],"futurePlate":3}
    ]
  }
}`

type shelfTestPlacement struct {
	ProjectID       string `json:"project_id"`
	ProjectRevision uint64 `json:"project_revision"`
	PlateIndex      int    `json:"plate_index"`
	ObjectID        uint64 `json:"object_id"`
	InstanceID      uint64 `json:"instance_id"`
	Source          string `json:"source"`
	SHA256          string `json:"sha256"`
}

func shelfWorkspaceFixture(t *testing.T, s *Server) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	s.workspace.register(mux)
	s.shelfRoutes(mux)
	return s.middleware(mux)
}

func shelfProjectJSON(t *testing.T, h http.Handler, method, path, session string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(sessionHeader, session)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func shelfCreateProject(t *testing.T, h http.Handler, id string, record json.RawMessage) {
	t.Helper()
	w := shelfProjectJSON(t, h, "PUT", "/api/projects/"+id, shelfTestSession,
		projectWriteInput{OperationID: "create-" + id, Record: record})
	shelfRequireStatus(t, w, http.StatusOK)
}

func shelfTestUploadOne(t *testing.T, h http.Handler, revision uint64, name string, model []byte) shelfModel {
	t.Helper()
	w := shelfUpload(t, h, revision, "", map[string][]byte{name: model})
	shelfRequireStatus(t, w, http.StatusCreated)
	var result struct{ Imported []shelfImported }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Imported) != 1 {
		t.Fatalf("invalid import result: %s, error %v", w.Body, err)
	}
	return result.Imported[0].Model
}

func TestShelfPlacementPreservesProjectAndReplays(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	native := shelfZIP(t, map[string][]byte{"3D/3dmodel.model": []byte(`<model><resources>
	  <object id="1"><mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="15" y="0" z="0"/><vertex x="0" y="15" z="10"/></vertices>
	  <triangles><triangle v1="0" v2="1" v3="2"/></triangles></mesh></object>
	  <object id="2"><mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="150" y="0" z="0"/><vertex x="0" y="100" z="10"/></vertices>
	  <triangles><triangle v1="0" v2="1" v3="2"/></triangles></mesh></object></resources>
	  <build><item objectid="2"/><item objectid="2"/><item objectid="2"/><item objectid="1"/></build></model>`)})
	asset := httptest.NewRequest("PUT", "/api/assets/original-native", bytes.NewReader(native))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, asset)
	shelfRequireStatus(t, w, http.StatusOK)
	shelfCreateProject(t, h, shelfTestProject, json.RawMessage(shelfTestProjectRecord))
	before := shelfJSON(t, h, "GET", "/api/projects/"+shelfTestProject, nil, nil)
	shelfRequireStatus(t, before, http.StatusOK)
	model := shelfTestUploadOne(t, h, 0, "new-part.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	place := shelfPlateInput{ProjectID: shelfTestProject, PlateIndex: 1, ExpectedProjectRevision: ptr(uint64(1)), ExpectedRevision: ptr(uint64(1)), OperationID: "add-inactive", X: ptr(22.0), Y: ptr(29.0)}
	w = shelfProjectJSON(t, h, "POST", path, shelfTestSession, place)
	shelfRequireStatus(t, w, http.StatusOK)
	var result shelfTestPlacement
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ProjectID != shelfTestProject || result.ProjectRevision != 2 || result.PlateIndex != 1 || result.ObjectID != 181 || result.InstanceID != 182 {
		t.Fatalf("inactive placement = %+v", result)
	}
	assetBody := shelfJSON(t, h, "GET", "/api/assets/"+result.Source, nil, nil)
	shelfRequireStatus(t, assetBody, http.StatusOK)
	geometry := shelfJSON(t, h, "GET", "/api/shelf/models/"+model.ID+"/geometry.stl?revision=1", nil, nil)
	shelfRequireStatus(t, geometry, http.StatusOK)
	if !bytes.Equal(geometry.Body.Bytes(), assetBody.Body.Bytes()) {
		t.Fatal("geometry download and saved workspace asset use different bytes")
	}
	sum := sha256.Sum256(assetBody.Body.Bytes())
	if result.SHA256 != hex.EncodeToString(sum[:]) || result.Source != "sha256-"+result.SHA256 || assetBody.Header().Get("X-Content-SHA256") != result.SHA256 {
		t.Fatalf("returned asset %q has a different digest: %+v", result.Source, sum)
	}
	binarySTL := assetBody.Body.Bytes()
	if len(binarySTL) != 134 || binary.LittleEndian.Uint32(binarySTL[80:84]) != 1 {
		t.Fatalf("asset is not a single-triangle binary STL: %d bytes", len(binarySTL))
	}
	vertices := [9]float32{}
	for i := range vertices {
		vertices[i] = math.Float32frombits(binary.LittleEndian.Uint32(binarySTL[96+i*4:]))
	}
	if vertices != [9]float32{-7.5, -7.5, 0, 7.5, -7.5, 0, -7.5, 7.5, 10} {
		t.Fatalf("canonical STL vertices = %v", vertices)
	}

	active := place
	active.PlateIndex, active.OperationID, active.ExpectedProjectRevision = 2, "add-active", ptr(uint64(2))
	active.X, active.Y = nil, nil
	activeWrite := shelfProjectJSON(t, h, "POST", path, shelfTestSession, active)
	shelfRequireStatus(t, activeWrite, http.StatusOK)
	var activeResult shelfTestPlacement
	if err := json.Unmarshal(activeWrite.Body.Bytes(), &activeResult); err != nil {
		t.Fatal(err)
	}
	if activeResult.ProjectRevision != 3 || activeResult.ObjectID != 183 || activeResult.InstanceID != 184 || activeResult.Source != result.Source {
		t.Fatalf("active placement = %+v", activeResult)
	}

	current := shelfJSON(t, h, "GET", "/api/projects/"+shelfTestProject, nil, nil)
	shelfRequireStatus(t, current, http.StatusOK)
	var saved struct {
		FutureRecord map[string]bool
		Meta         struct {
			Revision, Objects int
			FutureMeta        string
		}
		Doc struct {
			Project, FutureDoc json.RawMessage
			Events             json.RawMessage
			PlateIndex         int
			Files              map[string]string
			Objects            []map[string]any
			ProjectPlates      []struct {
				Objects     []map[string]any
				Events      json.RawMessage
				FuturePlate any
				Origin      []float64
				Locked      bool
			}
		}
	}
	if err := json.Unmarshal(current.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.FutureRecord["preserve"] || saved.Meta.FutureMeta != "kept" || saved.Meta.Revision != 3 || saved.Meta.Objects != 4 ||
		string(saved.Doc.Project) != `"original-native"` || string(saved.Doc.FutureDoc) != `{"vendor":"kept"}` || string(saved.Doc.Events) != `[{"type":"pause","z":6}]` ||
		saved.Doc.PlateIndex != 2 || saved.Doc.Files[result.Source] != "new-part.stl" || saved.Doc.Files["original-native"] != "original.3mf" {
		t.Fatalf("lost project metadata: %s", current.Body)
	}
	plates := saved.Doc.ProjectPlates
	if len(plates) != 3 || len(plates[0].Objects) != 2 || len(saved.Doc.Objects) != 2 || !reflect.DeepEqual(plates[1].Objects, saved.Doc.Objects) ||
		plates[0].FuturePlate != "kept" || string(plates[0].Events) != `[{"type":"custom"}]` || !reflect.DeepEqual(plates[1].Origin, []float64{130, 250}) || !plates[2].Locked || len(plates[2].Objects) != 0 {
		t.Fatalf("lost plate representation: %s", current.Body)
	}
	if !reflect.DeepEqual(saved.Doc.Objects[0]["project"], map[string]any{"buildIndex": 3.0}) || !reflect.DeepEqual(saved.Doc.Objects[0]["futureObject"], []any{1.0, 2.0}) {
		t.Fatalf("native object was modified: %+v", saved.Doc.Objects[0])
	}
	for _, tc := range []struct {
		object   map[string]any
		explicit bool
	}{{plates[0].Objects[1], true}, {saved.Doc.Objects[1], false}} {
		if tc.object["source"] != result.Source || tc.object["extruder"] != 1.0 || !reflect.DeepEqual(tc.object["scale"], []any{1.0, 1.0, 1.0}) {
			t.Fatalf("new object is not normal workspace geometry: %+v", tc.object)
		}
		instance := tc.object["instances"].([]any)[0].(map[string]any)
		x, y := instance["x"].(float64), instance["y"].(float64)
		if instance["rotZ"] != 0.0 || tc.explicit && (x != 22 || y != 29) ||
			!tc.explicit && (x < -32.5 || x > 152.5 || y < 27.5 || y > 92.5 || math.Abs(x-30) < 15 && math.Abs(y-60) < 15) {
			t.Fatalf("wrong placement: %+v", instance)
		}
	}

	s.workspace.mu.Lock()
	workspace, err := s.workspace.readSnapshot()
	if err != nil {
		s.workspace.mu.Unlock()
		t.Fatal(err)
	}
	versions := workspace.Projects[shelfTestProject].history().Versions
	s.workspace.mu.Unlock()
	if len(versions) == 0 || versions[len(versions)-1].Revision != 3 || versions[len(versions)-1].Objects != 4 {
		t.Fatalf("normal project version missing: %+v", versions)
	}
	shelfRequireStatus(t, shelfJSON(t, h, "DELETE", "/api/shelf/models/"+model.ID, nil, ptr(uint64(1))), http.StatusOK)
	replayed := shelfProjectJSON(t, h, "POST", path, shelfTestSession, place)
	shelfRequireStatus(t, replayed, http.StatusOK)
	if !bytes.Equal(w.Body.Bytes(), replayed.Body.Bytes()) {
		t.Fatalf("operation retry returned %s, want %s", replayed.Body, w.Body)
	}
	place.X = ptr(99.0)
	conflict := shelfProjectJSON(t, h, "POST", path, shelfTestSession, place)
	shelfRequireStatus(t, conflict, http.StatusConflict)
	stillCurrent := shelfJSON(t, h, "GET", "/api/projects/"+shelfTestProject, nil, nil)
	if !bytes.Equal(current.Body.Bytes(), stillCurrent.Body.Bytes()) {
		t.Fatalf("retry changed project: %s", stillCurrent.Body)
	}
	afterDelete := shelfJSON(t, h, "GET", "/api/assets/"+result.Source, nil, nil)
	shelfRequireStatus(t, afterDelete, http.StatusOK)
	if !bytes.Equal(afterDelete.Body.Bytes(), binarySTL) {
		t.Fatal("Shelf deletion changed the saved project's asset")
	}
}

func TestShelfPlacementRejectsWritesWithoutChangingProject(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	const record = `{"meta":{"name":"Fixture","savedAt":1,"objects":0},"doc":{"plateIndex":1,"objects":[],"projectPlates":[{"index":1,"objects":[]},{"index":2,"locked":true,"objects":[]}]}}`
	shelfCreateProject(t, h, shelfTestProject, json.RawMessage(record))
	shelfCreateProject(t, h, "archived-project", json.RawMessage(record))
	shelfRequireStatus(t, shelfProjectJSON(t, h, "POST", "/api/projects/archived-project/archive", shelfTestSession,
		projectWriteInput{OperationID: "archive", ExpectedRevision: 1}), http.StatusOK)
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	before := shelfJSON(t, h, "GET", "/api/projects/"+shelfTestProject, nil, nil)
	for _, tc := range []struct {
		name, code, session string
		status              int
		change              func(*shelfPlateInput)
	}{
		{"foreign lease", "lease_held", "other-valid-session", 409, func(*shelfPlateInput) {}},
		{"no session", "session_required", "", 400, func(*shelfPlateInput) {}},
		{"missing project", "project_missing", shelfTestSession, 404, func(in *shelfPlateInput) { in.ProjectID = "missing-project" }},
		{"archived project", "project_archived", shelfTestSession, 410, func(in *shelfPlateInput) { in.ProjectID = "archived-project" }},
		{"locked plate", "plate_locked", shelfTestSession, 409, func(in *shelfPlateInput) { in.PlateIndex = 2 }},
		{"missing plate", "plate_missing", shelfTestSession, 404, func(in *shelfPlateInput) { in.PlateIndex = 4 }},
		{"stale shelf", "revision_conflict", shelfTestSession, 409, func(in *shelfPlateInput) { in.ExpectedRevision = ptr(uint64(0)) }},
		{"stale project", "revision_conflict", shelfTestSession, 409, func(in *shelfPlateInput) { in.ExpectedProjectRevision = ptr(uint64(0)) }},
		{"unknown bed", "placement_required", shelfTestSession, 400, func(in *shelfPlateInput) { in.X, in.Y = nil, nil }},
		{"partial placement", "placement_invalid", shelfTestSession, 400, func(in *shelfPlateInput) { in.Y = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := shelfPlateInput{ProjectID: shelfTestProject, PlateIndex: 1, ExpectedProjectRevision: ptr(uint64(1)), ExpectedRevision: ptr(uint64(1)), OperationID: "rejected", X: ptr(1.0), Y: ptr(1.0)}
			tc.change(&in)
			w := shelfProjectJSON(t, h, "POST", path, tc.session, in)
			shelfRequireStatus(t, w, tc.status)
			var failure struct{ Code string }
			if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Code != tc.code {
				t.Fatalf("error = %s, want %q", w.Body, tc.code)
			}
		})
	}
	step := shelfTestUploadOne(t, h, 1, "fixture.step", []byte("ISO-10303-21;\nEND-ISO-10303-21;\n"))
	withoutGeometry := shelfPlateInput{ProjectID: shelfTestProject, PlateIndex: 1, ExpectedProjectRevision: ptr(uint64(1)), ExpectedRevision: ptr(uint64(2)), OperationID: "step-model", X: ptr(1.0), Y: ptr(1.0)}
	shelfRequireStatus(t, shelfProjectJSON(t, h, "POST", "/api/shelf/models/"+step.ID+"/add-to-plate", shelfTestSession, withoutGeometry), http.StatusUnprocessableEntity)

	after := shelfJSON(t, h, "GET", "/api/projects/"+shelfTestProject, nil, nil)
	if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatalf("refused placement changed project: %s", after.Body)
	}
}
