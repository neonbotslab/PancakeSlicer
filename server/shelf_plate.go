package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type shelfPlateInput struct {
	ProjectID               string   `json:"projectId"`
	PlateIndex              int      `json:"plateIndex"`
	ExpectedProjectRevision *uint64  `json:"expectedProjectRevision"`
	ExpectedRevision        *uint64  `json:"expectedRevision"`
	OperationID             string   `json:"operationId"`
	X                       *float64 `json:"x,omitempty"`
	Y                       *float64 `json:"y,omitempty"`
}

type shelfPlateEdit struct {
	record, doc, meta map[string]json.RawMessage
	plates            []json.RawMessage
	objects           []json.RawMessage
	target, count     int
	active            bool
	lastID            uint64
}

func (s *Server) addShelfModelToPlate(w http.ResponseWriter, r *http.Request) {
	s.shelfModelToPlate(w, r, false)
}

func (s *Server) shelfCanonicalGeometry(ctx context.Context, id string, revision *uint64) (shelfModel, []shelfTriangle, []byte, error) {
	model, file, cleanup, err := s.shelfSource(id, revision)
	if err != nil {
		return model, nil, nil, err
	}
	defer cleanup()
	triangles, err := readShelfMesh(ctx, s.cfg, file, model.Format)
	if err != nil {
		return model, nil, nil, shelfProblem(422, "model_geometry_unavailable", "The server could not read geometry from this Shelf model.")
	}
	stl, _, err := shelfSTL(triangles)
	if err != nil {
		return model, nil, nil, shelfProblem(422, "model_geometry_unavailable", "The server could not convert this Shelf model to STL.")
	}
	return model, triangles, stl, nil
}

func (s *Server) shelfGeometry(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	var revision *uint64
	if values, hasRevision := query["revision"]; err != nil || hasRevision && len(values) != 1 {
		shelfFail(w, shelfProblem(400, "revision_invalid", "Supply a single numeric Shelf revision."))
		return
	} else if hasRevision {
		n, err := strconv.ParseUint(values[0], 10, 64)
		if err != nil {
			shelfFail(w, shelfProblem(400, "revision_invalid", "Supply a single numeric Shelf revision."))
			return
		}
		revision = &n
	}
	model, _, stl, err := s.shelfCanonicalGeometry(r.Context(), r.PathValue("id"), revision)
	if err != nil {
		shelfFail(w, err)
		return
	}
	name := shelfGeometryName(model.Name) + ".stl"
	w.Header().Set("Content-Type", "model/stl")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(stl))
}

func shelfGeometryName(name string) string {
	if stem := strings.TrimSuffix(name, filepath.Ext(name)); stem != "" {
		return stem
	}
	return name
}

func (s *Server) shelfModelToPlate(w http.ResponseWriter, r *http.Request, transientLease bool) {
	var in shelfPlateInput
	if !workspaceDecode(w, r, &in) {
		return
	}
	if !workspaceID(in.ProjectID) || in.PlateIndex < 1 || in.ExpectedProjectRevision == nil || in.ExpectedRevision == nil {
		shelfFail(w, shelfProblem(400, "placement_invalid", "projectId, plateIndex, expectedProjectRevision and expectedRevision are required."))
		return
	}
	if (in.X == nil) != (in.Y == nil) || in.X != nil && (math.Abs(*in.X) > math.MaxFloat32 || math.Abs(*in.Y) > math.MaxFloat32) {
		shelfFail(w, shelfProblem(400, "placement_invalid", "Supply both x and y coordinates, or omit both to find a free spot on the plate."))
		return
	}
	store := s.workspace
	modelID := r.PathValue("id")
	write := projectWriteInput{OperationID: in.OperationID, ExpectedRevision: *in.ExpectedProjectRevision}
	edit := projectSaveEdit{intent: []any{"add_shelf_model", modelID, in.PlateIndex, *in.ExpectedRevision, in.X, in.Y}, transientLease: transientLease}
	r = r.Clone(r.Context())
	r.SetPathValue("id", in.ProjectID)
	var projectID string
	var before json.RawMessage
	store.locked(w, func(data *workspaceData) {
		id, project, _, ok := store.projectWriteTarget(w, r, data, "save", write, &edit)
		if !ok {
			return
		}
		projectID, before = id, project.Record
	})
	if projectID == "" {
		return
	}
	plate, err := readShelfPlate(before, in.PlateIndex)
	if err != nil {
		shelfFail(w, err)
		return
	}
	model, triangles, stl, err := s.shelfCanonicalGeometry(r.Context(), modelID, in.ExpectedRevision)
	if err != nil {
		shelfFail(w, err)
		return
	}
	var point shelfPlanePoint
	if in.X != nil {
		point = shelfPlanePoint{*in.X, *in.Y}
	} else {
		point, err = s.placeShelfGeometry(r.Context(), plate, triangles)
		if err != nil {
			shelfFail(w, err)
			return
		}
	}
	sum := sha256.Sum256(stl)
	sha := hex.EncodeToString(sum[:])
	source := "sha256-" + sha
	name := shelfGeometryName(model.Name)
	objectID, instanceID := plate.lastID+1, plate.lastID+2
	record, err := plate.add(name, source, point, objectID, instanceID, len(triangles))
	if err != nil {
		shelfFail(w, err)
		return
	}
	asset := r.Clone(r.Context())
	asset.Method = http.MethodPut
	asset.Body = io.NopCloser(bytes.NewReader(stl))
	asset.SetPathValue("key", source)
	stored := &mcpResponse{}
	store.putAsset(stored, asset)
	if stored.status != http.StatusOK {
		writeJSON(w, stored.status, json.RawMessage(stored.Bytes()))
		return
	}
	edit.apply = func(project workspaceProject) (json.RawMessage, map[string]any, error) {
		if !bytes.Equal(project.Record, before) {
			return nil, nil, errors.New("The saved project changed. Reload it before adding the model.")
		}
		return record, map[string]any{
			"project_id": projectID, "project_revision": project.Revision + 1, "plate_index": in.PlateIndex,
			"object_id": objectID, "instance_id": instanceID, "source": source, "sha256": sha,
		}, nil
	}
	store.writeProject(w, r, "save", write, &edit)
}

func readShelfPlate(raw json.RawMessage, index int) (shelfPlateEdit, error) {
	var p shelfPlateEdit
	invalid := shelfProblem(400, "project_invalid", "The saved project document is unreadable.")
	missing := shelfProblem(404, "plate_missing", "The saved project does not have that plate.")
	if json.Unmarshal(raw, &p.record) != nil || json.Unmarshal(p.record["doc"], &p.doc) != nil || p.doc == nil ||
		json.Unmarshal(p.record["meta"], &p.meta) != nil || p.meta == nil {
		return p, invalid
	}
	var activeIndex int
	if value := p.doc["plateIndex"]; len(value) > 0 && json.Unmarshal(value, &activeIndex) != nil {
		return p, invalid
	}
	activeIndex = max(1, activeIndex)
	var activeObjects []json.RawMessage
	if value := p.doc["objects"]; len(value) > 0 && json.Unmarshal(value, &activeObjects) != nil {
		return p, invalid
	}
	if value := p.doc["projectPlates"]; len(value) > 0 && json.Unmarshal(value, &p.plates) != nil {
		return p, invalid
	}
	p.count, p.target, p.active = len(activeObjects), -1, index == activeIndex
	if !p.observeIDs(activeObjects) {
		return p, invalid
	}
	seen := map[int]bool{}
	for i, raw := range p.plates {
		var plate struct {
			Index   int               `json:"index"`
			Locked  bool              `json:"locked"`
			Objects []json.RawMessage `json:"objects"`
		}
		if json.Unmarshal(raw, &plate) != nil || plate.Index < 1 || seen[plate.Index] {
			return p, invalid
		}
		seen[plate.Index] = true
		if !p.observeIDs(plate.Objects) {
			return p, invalid
		}
		if plate.Index != activeIndex {
			p.count += len(plate.Objects)
		}
		if plate.Index == index {
			if plate.Locked {
				return p, shelfProblem(409, "plate_locked", "Unlock the target plate before adding a model.")
			}
			p.target, p.objects = i, plate.Objects
		}
	}
	if len(p.plates) == 0 && index != 1 || len(p.plates) > 0 && p.target == -1 {
		return p, missing
	}
	if len(p.plates) == 0 || p.active {
		p.active, p.objects = true, activeObjects
	}
	return p, nil
}

func (p *shelfPlateEdit) observeIDs(objects []json.RawMessage) bool {
	for _, raw := range objects {
		type identified struct{ ID json.RawMessage }
		var object struct {
			identified
			Instances, Volumes, Ranges []identified
		}
		if json.Unmarshal(raw, &object) != nil {
			return false
		}
		for _, row := range [][]identified{{object.identified}, object.Instances, object.Volumes, object.Ranges} {
			for _, item := range row {
				var id float64
				if json.Unmarshal(item.ID, &id) != nil {
					var legacy string
					if json.Unmarshal(item.ID, &legacy) == nil {
						id, _ = strconv.ParseFloat(legacy, 64)
					}
				}
				if id > (1<<53)-3 {
					return false
				}
				if id > 0 && math.Trunc(id) == id {
					p.lastID = max(p.lastID, uint64(id))
				}
			}
		}
	}
	return true
}

func (p shelfPlateEdit) add(name, source string, point [2]float64, objectID, instanceID uint64, triangles int) (json.RawMessage, error) {
	var files map[string]json.RawMessage
	if raw := p.doc["files"]; len(raw) > 0 && json.Unmarshal(raw, &files) != nil {
		return nil, shelfProblem(400, "project_invalid", "The saved project asset list is unreadable.")
	}
	if files == nil {
		files = map[string]json.RawMessage{}
	}
	if _, ok := files[source]; !ok {
		files[source] = must(json.Marshal(name + ".stl"))
	}
	object := map[string]any{
		"id": objectID, "name": name, "source": source, "rotX": 0, "rotY": 0, "scale": [3]int{1, 1, 1},
		"extruder": 1, "settings": map[string]any{},
		"simplification": map[string]any{"trianglesBefore": triangles, "trianglesAfter": triangles, "maxErrorMm": 0, "kept": "original"},
		"instances":      []map[string]any{{"id": instanceID, "x": point[0], "y": point[1], "rotZ": 0}},
	}
	objects := must(json.Marshal(append(p.objects, json.RawMessage(must(json.Marshal(object))))))
	if p.target >= 0 {
		var fields map[string]json.RawMessage
		json.Unmarshal(p.plates[p.target], &fields)
		fields["objects"] = objects
		p.plates[p.target] = must(json.Marshal(fields))
		p.doc["projectPlates"] = must(json.Marshal(p.plates))
	}
	if p.active {
		p.doc["objects"] = objects
	}
	p.doc["files"] = must(json.Marshal(files))
	p.meta["objects"] = must(json.Marshal(p.count + 1))
	p.meta["savedAt"] = must(json.Marshal(time.Now().UnixMilli()))
	p.meta["change"] = must(json.Marshal([]versionChange{{Label: "Added " + name + " from Shelf"}}))
	p.record["meta"], p.record["doc"] = must(json.Marshal(p.meta)), must(json.Marshal(p.doc))
	return json.Marshal(p.record)
}
