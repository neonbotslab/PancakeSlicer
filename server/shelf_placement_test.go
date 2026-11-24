package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func shelfTestFailure(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	shelfRequireStatus(t, w, status)
	var failure struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Code != code {
		t.Fatalf("error %s, want code %q", w.Body, code)
	}
}

func shelfTestPutAsset(t *testing.T, h http.Handler, key string, data []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/assets/"+key, bytes.NewReader(data)))
	shelfRequireStatus(t, w, http.StatusOK)
}

func shelfTestPlacementRequest(revision uint64, operation string) shelfPlateInput {
	return shelfPlateInput{ProjectID: shelfTestProject, PlateIndex: 1, ExpectedProjectRevision: &revision, ExpectedRevision: ptr(uint64(1)), OperationID: operation}
}

func shelfTestSavedPositions(t *testing.T, body []byte) []shelfPlanePoint {
	t.Helper()
	var state struct {
		Doc struct {
			Objects []struct{ Instances []struct{ X, Y float64 } }
		}
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	var points []shelfPlanePoint
	for _, o := range state.Doc.Objects {
		for _, i := range o.Instances {
			points = append(points, shelfPlanePoint{i.X, i.Y})
		}
	}
	return points
}

func TestShelfDefaultPlacementArrangesCopiesAndStopsOnFullPlate(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	record := json.RawMessage(`{"meta":{"name":"Fixture","savedAt":1,"objects":0},"doc":{"objects":[],"dirty":{"machine":{"printable_area":"0x0,33x0,33x15,0x15"}}}}`)
	shelfCreateProject(t, h, shelfTestProject, record)
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	projectPath := "/api/projects/" + shelfTestProject

	first := shelfTestPlacementRequest(1, "first-add")
	w := shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, first)
	shelfRequireStatus(t, w, http.StatusOK)
	before := shelfJSON(t, h, http.MethodGet, projectPath, nil, nil)
	points := shelfTestSavedPositions(t, before.Body.Bytes())
	if len(points) != 1 || points[0][0] < 7.5 || points[0][0] > 25.5 || points[0][1] != 7.5 {
		t.Fatalf("first placement outside bed: %v", points)
	}
	second := shelfTestPlacementRequest(2, "second-add")
	w = shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, second)
	shelfRequireStatus(t, w, http.StatusOK)
	after := shelfJSON(t, h, http.MethodGet, projectPath, nil, nil)
	withCopy := shelfTestSavedPositions(t, after.Body.Bytes())
	if len(withCopy) != 2 || withCopy[0] != points[0] || withCopy[1][0] < 7.5 || withCopy[1][0] > 25.5 ||
		withCopy[1][1] != 7.5 || math.Abs(withCopy[1][0]-withCopy[0][0]) < 18 {
		t.Fatalf("added copy overlaps or moves original: %v, from %v", withCopy, points)
	}
	replay := shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, second)
	shelfRequireStatus(t, replay, http.StatusOK)
	if !bytes.Equal(w.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("replay changed operation result: %s", replay.Body)
	}
	third := shelfTestPlacementRequest(3, "third-add")
	shelfTestFailure(t, shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, third), http.StatusConflict, "placement_no_room")
	stored := shelfJSON(t, h, http.MethodGet, projectPath, nil, nil)
	if !bytes.Equal(stored.Body.Bytes(), after.Body.Bytes()) {
		t.Fatalf("replay or refused placement changed the project: %s", stored.Body)
	}
}

func TestShelfDefaultPlacementAccountsForObjectAndVolumeTransforms(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	const old = "solid sample\nfacet normal 0 0 1\nouter loop\nvertex 0 0 0\nvertex 30 0 0\nvertex 0 12 10\nendloop\nendfacet\nendsolid sample\n"
	shelfTestPutAsset(t, h, "tracked-asset", []byte(old))
	vertices := []float32{0, 0, 0, 0, 0, 10, 40, 0, 0}
	data := make([]byte, 4*len(vertices))
	for i, v := range vertices {
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	volume := base64.StdEncoding.EncodeToString(data)
	record := json.RawMessage(`{"meta":{"name":"Fixture","savedAt":1,"objects":1},"doc":{"files":{"tracked-asset":"original.stl"},"dirty":{"machine":{"printable_area":"19x0,90x0,90x60,19x60"}},"objects":[{"id":1,"source":"tracked-asset","rotX":90,"rotY":0,"scale":[2,-1,3],"instances":[{"id":2,"x":40,"y":30,"rotZ":90}],"volumes":[{"id":3,"type":"part","matrix":[1,0,0,0,0,1,0,0,0,0,1,0,0,0,10,1],"geometry":{"position":{"$orcaBinary":"Float32Array","data":` + strconv.Quote(volume) + `}}}]}]}}`)
	shelfCreateProject(t, h, shelfTestProject, record)
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	w := shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, shelfTestPlacementRequest(1, "transformed-add"))
	shelfRequireStatus(t, w, http.StatusOK)
	saved := shelfJSON(t, h, http.MethodGet, "/api/projects/"+shelfTestProject, nil, nil)
	points := shelfTestSavedPositions(t, saved.Body.Bytes())
	if len(points) != 2 || points[0] != (shelfPlanePoint{40, 30}) ||
		(points[1][0]+7.5 > 25 && points[1][0]-7.5 < 70) {
		t.Fatalf("placement intersects transformed geometry: %v", points)
	}
}

func TestShelfDefaultPlacementFitsConcaveBed(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	record := json.RawMessage(`{"meta":{"name":"Fixture","savedAt":1,"objects":0},"doc":{"objects":[],"dirty":{"machine":{"printable_area":"0x0,60x0,60x60,33x60,33x20,27x20,27x60,0x60"}}}}`)
	shelfCreateProject(t, h, shelfTestProject, record)
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	w := shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, shelfTestPlacementRequest(1, "concave-bed"))
	shelfRequireStatus(t, w, http.StatusOK)
	saved := shelfJSON(t, h, http.MethodGet, "/api/projects/"+shelfTestProject, nil, nil)
	points := shelfTestSavedPositions(t, saved.Body.Bytes())
	if len(points) != 1 {
		t.Fatalf("placement missing from project: %s", saved.Body)
	}
	x, y := points[0][0], points[0][1]
	if y+7.5 > 20 && x+7.5 > 27 && x-7.5 < 33 {
		t.Fatalf("placement crosses the bed notch: %v", points[0])
	}
}

func TestShelfDefaultPlacementRespectsPrinterLimits(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	const prefix = `{"meta":{"name":"Fixture","savedAt":1,"objects":0},"doc":{"objects":[],"dirty":{"machine":{"printable_area":"0x0,40x0,40x15,0x15",`
	const suffix = `}}}}`
	shelfCreateProject(t, h, shelfTestProject, json.RawMessage(prefix+`"bed_exclude_area":["0x0","20x0","20x15","0x15"]`+suffix))
	shelfCreateProject(t, h, "short-printer", json.RawMessage(prefix+`"printable_height":"9"`+suffix))
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	plc := shelfTestPlacementRequest(1, "height-limit")
	plc.ProjectID = "short-printer"
	before := shelfJSON(t, h, http.MethodGet, "/api/projects/short-printer", nil, nil)
	shelfTestFailure(t, shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, plc), http.StatusConflict, "placement_no_room")
	after := shelfJSON(t, h, http.MethodGet, "/api/projects/short-printer", nil, nil)
	if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatalf("height limit changed the recorded project: %s", after.Body)
	}
	geometry := shelfJSON(t, h, http.MethodGet, "/api/shelf/models/"+model.ID+"/geometry.stl?revision=1", nil, nil)
	shelfRequireStatus(t, geometry, http.StatusOK)
	source := fmt.Sprintf("/api/assets/sha256-%x", sha256.Sum256(geometry.Body.Bytes()))
	shelfRequireStatus(t, shelfJSON(t, h, http.MethodGet, source, nil, nil), http.StatusNotFound)
	w := shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, shelfTestPlacementRequest(1, "exclude-area"))
	shelfRequireStatus(t, w, http.StatusOK)
	saved := shelfJSON(t, h, http.MethodGet, "/api/projects/"+shelfTestProject, nil, nil)
	positions := shelfTestSavedPositions(t, saved.Body.Bytes())
	if len(positions) != 1 || positions[0][0]-7.5 <= 20 || positions[0][0]+7.5 > 40 || positions[0][1] != 7.5 {
		t.Fatalf("placement intersects the excluded area: %v", positions)
	}
}

func TestShelfDefaultPlacementRefusesUnresolvedNativePosition(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	asset := shelfZIP(t, map[string][]byte{"3D/3dmodel.model": []byte(`<model/>`)})
	shelfTestPutAsset(t, h, "original", asset)
	record := json.RawMessage(`{"meta":{"name":"Fixture","savedAt":1,"objects":1},"doc":{"project":"original","files":{"original":"source.3mf"},"dirty":{"machine":{"printable_area":"0x0,60x0,60x60,0x60"}},"objects":[{"id":1,"source":"original","scale":[1,1,1],"instances":[{"id":2,"x":0,"y":0}],"project":{"buildIndex":0,"placeholder":true}}]}}`)
	shelfCreateProject(t, h, shelfTestProject, record)
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/add-to-plate"
	projectPath := "/api/projects/" + shelfTestProject
	before := shelfJSON(t, h, http.MethodGet, projectPath, nil, nil)
	place := shelfTestPlacementRequest(1, "native-unknown")
	shelfTestFailure(t, shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, place), http.StatusUnprocessableEntity, "placement_geometry_unavailable")
	after := shelfJSON(t, h, http.MethodGet, projectPath, nil, nil)
	if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatalf("refused placement changed project: %s", after.Body)
	}
	place.X, place.Y = ptr(40.0), ptr(45.0)
	w := shelfProjectJSON(t, h, http.MethodPost, path, shelfTestSession, place)
	shelfRequireStatus(t, w, http.StatusOK)
	saved := shelfJSON(t, h, http.MethodGet, projectPath, nil, nil)
	points := shelfTestSavedPositions(t, saved.Body.Bytes())
	if len(points) != 2 || points[1] != (shelfPlanePoint{40, 45}) {
		t.Fatalf("explicit add did not preserve requested coordinates: %v", points)
	}
}

func TestShelfGeometryHonorsAccessAndRevision(t *testing.T) {
	s, h := shelfFixture(t)
	model := shelfTestUploadOne(t, h, 0, "café.stl", []byte(shelfTestSTL))
	path := "/api/shelf/models/" + model.ID + "/geometry.stl"
	s.cfg.BearerToken = "shelf-test-credential"
	get := func(endpoint string, authorized bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, endpoint, nil)
		if authorized {
			r.Header.Set("Authorization", "Bearer "+s.cfg.BearerToken)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	shelfRequireStatus(t, get(path+"?revision=1", false), http.StatusUnauthorized)
	for _, query := range []string{"", "-1", "1.0", "18446744073709551616", "1&revision=1"} {
		shelfTestFailure(t, get(path+"?revision="+query, true), http.StatusBadRequest, "revision_invalid")
	}
	shelfTestFailure(t, get(path+"?revision=0", true), http.StatusConflict, "revision_conflict")
	shelfTestFailure(t, get("/api/shelf/models/missing/geometry.stl?revision=1", true), http.StatusNotFound, "model_missing")
	w := get(path+"?revision=1", true)
	shelfRequireStatus(t, w, http.StatusOK)
	disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if err != nil || disposition != "inline" || params["filename"] != "café.stl" ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "model/stl" || w.Body.Len() != 134 {
		t.Fatalf("invalid canonical geometry response: headers %v, size %d", w.Header(), w.Body.Len())
	}
}

func TestShelfPlacementUsesImportedMachineOverrides(t *testing.T) {
	s, _ := shelfFixture(t)
	h := shelfWorkspaceFixture(t, s)
	model := shelfTestUploadOne(t, h, 0, "fixture.stl", []byte(shelfTestSTL))
	for _, tc := range []struct {
		id, imported, dirty string
		status              int
	}{
		{"imported-area", `{"printable_area":"0x0,14x0,14x14,0x14"}`, `{}`, http.StatusConflict},
		{"imported-height", `{"printable_area":"0x0,40x0,40x40,0x40","printable_height":9}`, `{}`, http.StatusConflict},
		{"dirty-overrides-import", `{"printable_area":"0x0,14x0,14x14,0x14"}`, `{"printable_area":"0x0,40x0,40x40,0x40"}`, http.StatusOK},
	} {
		t.Run(tc.id, func(t *testing.T) {
			record := json.RawMessage(`{"meta":{"name":"Imported fixture","savedAt":1,"objects":0},"doc":{"objects":[],"projectPresets":{"machine":` + tc.imported + `},"dirty":{"machine":` + tc.dirty + `}}}`)
			shelfCreateProject(t, h, tc.id, record)
			request := shelfTestPlacementRequest(1, tc.id)
			request.ProjectID = tc.id
			w := shelfProjectJSON(t, h, http.MethodPost, "/api/shelf/models/"+model.ID+"/add-to-plate", shelfTestSession, request)
			shelfRequireStatus(t, w, tc.status)
			if tc.status == http.StatusOK {
				saved := shelfJSON(t, h, http.MethodGet, "/api/projects/"+tc.id, nil, nil)
				var result struct {
					Doc struct {
						Objects []struct {
							Simplification struct{ Kept string }
						}
					}
				}
				if json.Unmarshal(saved.Body.Bytes(), &result) != nil || len(result.Doc.Objects) != 1 || result.Doc.Objects[0].Simplification.Kept != "original" {
					t.Fatalf("canonical geometry intent was not saved: %s", saved.Body)
				}
			}
		})
	}
}
