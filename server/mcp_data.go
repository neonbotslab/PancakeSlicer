package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpProjectInput struct {
	ProjectID string `json:"project_id" jsonschema:"Saved project ID returned by list_projects"`
}

type mcpPlateInput struct {
	mcpProjectInput
	PlateIndex int `json:"plate_index" jsonschema:"One-based plate index returned by list_plates"`
}

type mcpCatalogInput struct {
	Query  string `json:"query,omitempty" jsonschema:"Filter by product ID, brand, or material type"`
	Offset int    `json:"offset,omitempty" jsonschema:"Skip this many matching products"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum products to return. Defaults to 50. Maximum 200."`
}

type mcpCatalogProfileInput struct {
	Kind string `json:"kind" jsonschema:"One of printers, prints, or filaments"`
	ID   string `json:"id" jsonschema:"Exact catalog ID"`
}

type mcpSliceJobInput struct {
	JobID string `json:"job_id" jsonschema:"Slice job ID returned by slice_plate"`
}

type mcpBlobRef struct {
	Name   string `json:"name" jsonschema:"File basename referenced by the plate or project edits"`
	SHA256 string `json:"sha256" jsonschema:"Lowercase SHA-256 from upload_model or PUT /api/blobs/{sha256}"`
}

type mcpUploadModelInput struct {
	Name string `json:"name" jsonschema:"STL, OBJ, 3MF, STEP, STP, or DRC file basename"`
	Data string `json:"data" jsonschema:"Model file bytes in standard base64"`
}

type mcpSliceLibrary struct {
	Printer     string         `json:"printer" jsonschema:"Saved printer setup ID from list_profiles"`
	Print       string         `json:"print" jsonschema:"Saved print profile ID from list_profiles"`
	Filaments   []string       `json:"filaments" jsonschema:"Saved filament IDs in the same order as slots"`
	Slots       []string       `json:"slots" jsonschema:"Slot IDs from the selected printer setup"`
	LayerHeight float64        `json:"layerHeight,omitempty" jsonschema:"Layer height in millimeters. Omit to use the profile rule."`
	Project     map[string]any `json:"project,omitempty" jsonschema:"Project overrides keyed by machine, process, filaments, or slots"`
	UsedSlots   []int          `json:"usedSlots,omitempty" jsonschema:"One-based positions of the filament slots used on the plate"`
}

type mcpSliceInput struct {
	Library                mcpSliceLibrary   `json:"library"`
	Files                  []mcpBlobRef      `json:"files" jsonschema:"Prepared models. Put the original 3MF first for a native project."`
	ProjectID              string            `json:"project_id,omitempty" jsonschema:"Select a saved project's plate. Supply that plate's prepared slice inputs."`
	ProjectRevision        *uint64           `json:"project_revision,omitempty" jsonschema:"Reject a saved project whose revision differs from this value"`
	PlateIndex             int               `json:"plate_index,omitempty" jsonschema:"One-based saved plate index or native 3MF plate index"`
	Plate                  map[string]any    `json:"plate,omitempty" jsonschema:"Prepared plate JSON containing objects and their placements. Supply models baked for this layout."`
	ProjectInstances       []projectInstance `json:"project_instances,omitempty" jsonschema:"Printable flags and optional 12-number transform deltas in original 3MF build-item order"`
	ProjectEdits           []map[string]any  `json:"project_edits,omitempty" jsonschema:"Prepared native 3MF edits, including deletions, paint, and added geometry"`
	ProjectSettings        map[string]any    `json:"project_settings,omitempty" jsonschema:"Settings to patch into a native 3MF project"`
	Events                 []string          `json:"events,omitempty" jsonschema:"Layer events in engine format, beginning with the Z height in millimeters"`
	Output                 string            `json:"output,omitempty" jsonschema:"gcode or gcode.3mf. Default gcode."`
	Filename               string            `json:"filename,omitempty" jsonschema:"Optional download basename"`
	ForcePlanar            bool              `json:"force_planar,omitempty"`
	AllowMixedTemperatures bool              `json:"allow_mixed_temperatures,omitempty"`
	PrinterFilamentContext map[string]any    `json:"printer_filament_context,omitempty" jsonschema:"Optional loaded filament context for a supported engine"`
}

func (m *mcpService) addDataTools(protocol *mcp.Server) {
	mcpTool(protocol, "list_projects", "List active saved projects and their revisions.", true, m.listProjects)
	mcpTool(protocol, "get_project", "Get a saved project's document, revision, active plate and source asset keys. Geometry edits require the prepared plate data described by slice_plate.", true, m.getProject)
	mcpTool(protocol, "list_plates", "List a saved project's plates and object counts.", true, m.listPlates)
	mcpTool(protocol, "get_plate", "Read a saved plate's objects, settings and events by project ID and one-based plate index.", true, m.getPlate)
	mcpTool(protocol, "list_printers", "Read connection state, progress, loaded AMS trays and nozzles for saved printer setups.", true, m.listPrinters)
	mcpTool(protocol, "list_profiles", "List saved printer setups, print profiles and filament profiles. Use their IDs and the printer setup's slot IDs in slice_plate.", true, m.listProfiles)
	mcpTool(protocol, "list_catalog_filaments", "Search the built-in filament catalog. Use the saved filament IDs from list_profiles for slicing.", true, m.listCatalogFilaments)
	mcpTool(protocol, "get_catalog_profile", "Read a built-in printer model, print profile or filament product by catalog ID.", true, m.getCatalogProfile)
	mcpTool(protocol, "upload_model", "Store a base64-encoded model in the upload cache and return its file reference for slice_plate.", false, m.uploadModel)
	mcpTool(protocol, "slice_plate", "Queue a slice from uploaded model hashes and explicit profile settings. For a saved project, supply its prepared layout and baked models, or its native 3MF with explicit instance transforms and project edits. Returns a job ID for get_slice_job. Creates only a slice job.", false, m.slicePlate)
	mcpTool(protocol, "get_slice_job", "Read slice job status, estimated time, filament weight, warnings, artifact location and print transfer status.", true, m.getSliceJob)
}

type mcpSavedProject struct {
	id       string
	record   workspaceProject
	document mcpProjectDocument
}

type mcpProjectDocument struct {
	ActivePlate int                 `json:"plateIndex"`
	Objects     []json.RawMessage   `json:"objects"`
	Events      []json.RawMessage   `json:"events"`
	Plates      []mcpWorkspacePlate `json:"projectPlates"`
	Native      string              `json:"project"`
}

type mcpWorkspacePlate struct {
	Index    int               `json:"index"`
	Name     string            `json:"name"`
	Origin   [2]float64        `json:"origin"`
	Locked   bool              `json:"locked,omitempty"`
	Settings json.RawMessage   `json:"settings,omitempty"`
	Events   []json.RawMessage `json:"events"`
	Objects  []json.RawMessage `json:"objects"`
}

func (m *mcpService) listProjects(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (map[string]any, error) {
	type projectRow struct {
		Meta    json.RawMessage
		ID      string  `json:"id"`
		SavedAt float64 `json:"savedAt"`
	}
	st := m.server.workspace
	st.mu.Lock()
	defer st.mu.Unlock()
	data, err := st.readSnapshot()
	if err != nil {
		return nil, errors.New("saved projects are unavailable")
	}
	rows := make([]projectRow, 0, len(data.Projects))
	for id, project := range data.Projects {
		if project.Archived || project.Record == nil {
			continue
		}
		row := projectRow{ID: id, Meta: projectMeta(project)}
		if json.Unmarshal(row.Meta, &row) == nil {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b projectRow) int {
		if order := cmp.Compare(b.SavedAt, a.SavedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	projects := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, row.Meta)
	}
	return map[string]any{"projects": projects}, nil
}

func (m *mcpService) savedProject(id string) (mcpSavedProject, error) {
	st := m.server.workspace
	st.mu.Lock()
	defer st.mu.Unlock()
	data, err := st.readSnapshot()
	if err != nil {
		return mcpSavedProject{}, errors.New("saved projects are unavailable")
	}
	canonical, err := canonicalWorkspaceProject(data, id)
	if err != nil {
		return mcpSavedProject{}, err
	}
	project := data.Projects[canonical]
	if project.Archived || project.Record == nil {
		return mcpSavedProject{}, errors.New("active saved project not found")
	}
	var record struct {
		Doc mcpProjectDocument `json:"doc"`
	}
	if err := json.Unmarshal(project.Record, &record); err != nil {
		return mcpSavedProject{}, errors.New("saved project document is unreadable")
	}
	return mcpSavedProject{id: canonical, record: project, document: record.Doc}, nil
}

func (d mcpProjectDocument) plates() []mcpWorkspacePlate {
	if len(d.Plates) == 0 {
		return []mcpWorkspacePlate{{Index: 1, Objects: d.Objects, Events: d.Events}}
	}
	for i := range d.Plates {
		if d.Plates[i].Index == d.ActivePlate {
			d.Plates[i].Objects = d.Objects
			d.Plates[i].Events = d.Events
			break
		}
	}
	return d.Plates
}

func (p mcpSavedProject) plate(index int) (mcpWorkspacePlate, error) {
	for _, plate := range p.document.plates() {
		if plate.Index == index {
			return plate, nil
		}
	}
	return mcpWorkspacePlate{}, fmt.Errorf("saved project has no plate %d", index)
}

func (m *mcpService) getProject(_ context.Context, _ *mcp.CallToolRequest, in mcpProjectInput) (map[string]any, error) {
	project, err := m.savedProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(servedRecord(project.record), &out); err != nil {
		return nil, err
	}
	out["project_id"] = project.id
	out["project_revision"] = project.record.Revision
	return out, nil
}

func (m *mcpService) listPlates(_ context.Context, _ *mcp.CallToolRequest, in mcpProjectInput) (map[string]any, error) {
	project, err := m.savedProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	plates := project.document.plates()
	out := make([]map[string]any, 0, len(plates))
	for _, plate := range plates {
		out = append(out, map[string]any{"plate_index": plate.Index, "name": plate.Name, "object_count": len(plate.Objects), "locked": plate.Locked})
	}
	return map[string]any{"project_id": project.id, "project_revision": project.record.Revision, "active_plate_index": max(1, project.document.ActivePlate), "plates": out}, nil
}

func (m *mcpService) getPlate(_ context.Context, _ *mcp.CallToolRequest, in mcpPlateInput) (map[string]any, error) {
	project, err := m.savedProject(in.ProjectID)
	if err != nil {
		return nil, err
	}
	plate, err := project.plate(in.PlateIndex)
	if err != nil {
		return nil, err
	}
	return map[string]any{"project_id": project.id, "project_revision": project.record.Revision, "plate_index": plate.Index, "plate": plate}, nil
}

type mcpPrinter struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Kind                string         `json:"kind"`
	Model               string         `json:"model,omitempty"`
	AutoFlowCalibration bool           `json:"auto_flow_calibration"`
	State               string         `json:"state"`
	Online              bool           `json:"online"`
	Link                LinkState      `json:"link,omitempty"`
	Progress            *float64       `json:"progress,omitempty"`
	Remaining           *int           `json:"remaining_s,omitempty"`
	At                  *time.Time     `json:"at,omitempty"`
	LastSeen            *time.Time     `json:"last_seen,omitempty"`
	Trays               []AMSTray      `json:"trays,omitempty"`
	Nozzles             []NozzleStatus `json:"nozzles,omitempty"`
}

func (m *mcpService) listPrinters(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (map[string]any, error) {
	sends := m.server.sends
	hub := sends.hub
	defer hub.subscribe()()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	connections := sends.connections()
	for {
		rows, wake, waiting := mcpPrinterStatuses(hub, connections)
		if !waiting {
			return map[string]any{"printers": rows}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return map[string]any{"printers": rows}, nil
		case <-wake:
		}
	}
}

func mcpPrinterStatuses(hub *statusHub, connections []*PrinterConfig) ([]mcpPrinter, <-chan struct{}, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	rows := make([]mcpPrinter, 0, len(connections))
	waiting := false
	for _, p := range connections {
		if p.Printer == "" {
			continue
		}
		row := mcpPrinter{ID: p.Printer, Name: p.Name, Kind: p.Kind, Model: p.model, AutoFlowCalibration: p.supportsAutoFlowCalibration(),
			State: "unavailable", LastSeen: hub.lastSeen[p.Printer]}
		if st, found := hub.status[p.Printer]; found {
			row.State, row.Online, row.Link = st.State, st.Online, st.Link
			row.Progress, row.Remaining, row.At = st.Progress, st.Remaining, &st.At
			row.Trays, row.Nozzles = st.Trays, st.Nozzles
			waiting = waiting || st.Link == LinkConnecting
		}
		rows = append(rows, row)
	}
	return rows, hub.wake, waiting
}

func (m *mcpService) listProfiles(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (map[string]any, error) {
	lib, err := m.server.libraryDocument()
	if err != nil {
		return nil, errors.New("the profile library is unavailable")
	}
	printers := make([]map[string]any, 0, len(lib.Printers))
	for _, id := range sortedKeys(lib.Printers) {
		p := lib.Printers[id]
		if p.Live() {
			printers = append(printers, map[string]any{"id": p.ID, "name": p.Name, "vendor": p.Vendor, "model": p.Model, "nozzles": p.Nozzles,
				"process": p.Process, "slots": p.Slots, "mixes": p.Mixes, "problem": p.Problem})
		}
	}
	prints := make([]map[string]any, 0, len(lib.Processes))
	for _, id := range sortedKeys(lib.Processes) {
		p := lib.Processes[id]
		if p.Live() {
			prints = append(prints, map[string]any{"id": p.ID, "name": p.Name, "product": p.Product, "preferred_height": p.PreferredHeight, "problem": p.Problem})
		}
	}
	filaments := make([]map[string]any, 0, len(lib.Filaments))
	for _, id := range sortedKeys(lib.Filaments) {
		f := lib.Filaments[id]
		if f.Live() {
			filaments = append(filaments, map[string]any{"id": f.ID, "name": f.Name, "product": f.Product, "problem": f.Problem})
		}
	}
	return map[string]any{"revision": lib.Revision, "printers": printers, "processes": prints, "filaments": filaments}, nil
}

func (m *mcpService) listCatalogFilaments(_ context.Context, _ *mcp.CallToolRequest, in mcpCatalogInput) (map[string]any, error) {
	if in.Offset < 0 || in.Limit < 0 || in.Limit > 200 {
		return nil, errors.New("offset must be non-negative and limit must be between 1 and 200")
	}
	query := strings.ToLower(strings.TrimSpace(in.Query))
	catalog := m.server.catalog
	match := make([]resolve.CatalogEntry, 0)
	for _, row := range catalog.List("filaments") {
		if query == "" || strings.Contains(strings.ToLower(row.ID+" "+row.Brand+" "+row.Type), query) {
			match = append(match, row)
		}
	}
	start := min(in.Offset, len(match))
	end := min(start+cmp.Or(in.Limit, 50), len(match))
	out := map[string]any{"version": catalog.Version, "filaments": match[start:end], "total": len(match)}
	if end < len(match) {
		out["next_offset"] = end
	}
	return out, nil
}

func (m *mcpService) getCatalogProfile(_ context.Context, _ *mcp.CallToolRequest, in mcpCatalogProfileInput) (map[string]any, error) {
	catalog := m.server.catalog
	raw := catalog.Line(in.Kind, in.ID)
	if raw == nil {
		return nil, errors.New("catalog profile not found")
	}
	return map[string]any{"version": catalog.Version, "kind": in.Kind, "id": in.ID, "profile": json.RawMessage(raw)}, nil
}

func (m *mcpService) uploadModel(ctx context.Context, call *mcp.CallToolRequest, in mcpUploadModelInput) (map[string]any, error) {
	if in.Name == "" || len(in.Name) > 240 || strings.ContainsAny(in.Name, "/\\") || strings.IndexFunc(in.Name, unicode.IsControl) >= 0 {
		return nil, errors.New("name must be a model file basename")
	}
	switch strings.ToLower(filepath.Ext(in.Name)) {
	case ".stl", ".obj", ".3mf", ".step", ".stp", ".drc":
	default:
		return nil, errors.New("unsupported model file extension")
	}
	limit := m.server.cfg.MaxUploadSize
	decode := func() io.Reader { return base64.NewDecoder(base64.StdEncoding, strings.NewReader(in.Data)) }
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(decode(), limit+1))
	if err != nil || size == 0 {
		return nil, errors.New("data must contain a model encoded in standard base64")
	}
	if size > limit {
		return nil, errors.New("model exceeds the server upload limit")
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	r := mcpHTTPRequest(ctx, call, http.MethodPut, "/api/blobs/"+sum, decode())
	r.SetPathValue("sha256", sum)
	w := &mcpResponse{}
	m.server.blobs.put(w, r, limit)
	out, err := w.result()
	if err != nil {
		return nil, err
	}
	out["name"] = in.Name
	out["size"] = size
	return out, nil
}

func (m *mcpService) getSliceJob(ctx context.Context, call *mcp.CallToolRequest, in mcpSliceJobInput) (map[string]any, error) {
	j := m.server.jobs.Get(in.JobID)
	if j == nil {
		return nil, errors.New("slice job not found")
	}
	view := j.View()
	out := map[string]any{"id": view.ID, "state": view.State, "lifecycle_state": view.LifecycleState, "stage": view.Stage, "progress": view.Progress,
		"queue_position": view.QueuePosition, "eta_seconds": view.ETA, "created": view.Created, "started": view.Started, "finished": view.Finished,
		"error": view.Error, "error_code": view.ErrorCode, "result_ready": view.ResultReady, "output": view.Request.Output,
		"plate_index": view.Request.PlateIndex, "download_url": view.DownloadURL, "filename": view.Filename}
	if view.Request.SourceProject != nil {
		out["source_project"] = view.Request.SourceProject
	}
	if len(view.Slice) != 0 {
		var numbers versionSlice
		sliceNumbers(view.Slice, &numbers)
		var record struct {
			Warnings json.RawMessage `json:"warnings"`
		}
		if err := json.Unmarshal(view.Slice, &record); err == nil {
			out["estimated_time_s"] = numbers.TimeS
			out["filament_grams"] = numbers.Grams
			out["warnings"] = record.Warnings
		}
	}
	r := mcpHTTPRequest(ctx, call, http.MethodGet, "/api/jobs/send", nil)
	r.SetPathValue("id", j.ID)
	w := &mcpResponse{}
	m.server.getSend(w, r)
	if transfer, err := w.result(); err == nil {
		delete(transfer, "error")
		delete(transfer, "message")
		delete(transfer, "job_errors")
		out["transfer"] = transfer
	}
	return out, nil
}

func (m *mcpService) slicePlate(ctx context.Context, call *mcp.CallToolRequest, in mcpSliceInput) (map[string]any, error) {
	if in.Output != "" && in.Output != "gcode" && in.Output != "gcode.3mf" {
		return nil, errors.New("output must be gcode or gcode.3mf")
	}
	if in.Plate != nil && (in.ProjectInstances != nil || in.ProjectEdits != nil || in.ProjectSettings != nil || in.ProjectID == "" && in.PlateIndex != 0) {
		return nil, errors.New("use either a prepared plate layout or native 3MF slice fields")
	}

	var project mcpSavedProject
	if in.ProjectID != "" {
		var err error
		project, err = m.savedProject(in.ProjectID)
		if err != nil {
			return nil, err
		}
		if in.ProjectRevision != nil && *in.ProjectRevision != project.record.Revision {
			return nil, errors.New("the saved project revision changed. Read the project again")
		}
		if in.PlateIndex == 0 {
			in.PlateIndex = max(1, project.document.ActivePlate)
		}
		plate, err := project.plate(in.PlateIndex)
		if err != nil {
			return nil, err
		}
		if len(plate.Objects) == 0 {
			return nil, errors.New("the saved plate has no objects")
		}
		if project.document.Native == "" && in.Plate == nil || project.document.Native != "" && len(in.ProjectInstances) == 0 {
			return nil, errors.New("supply the saved plate's prepared layout and baked files or its original 3MF and explicit project_instances with any edits")
		}
	} else if in.ProjectRevision != nil {
		return nil, errors.New("project_revision requires project_id")
	}

	fields := map[string]any{"library": in.Library, "files": in.Files}
	if in.Plate != nil {
		fields["plate"] = in.Plate
	}
	if in.ProjectInstances != nil {
		fields["project_instances"] = in.ProjectInstances
	}
	if in.ProjectEdits != nil {
		fields["project_edits"] = in.ProjectEdits
	}
	if in.ProjectSettings != nil {
		fields["project_settings"] = in.ProjectSettings
	}
	if in.Events != nil {
		fields["events"] = in.Events
	}
	if in.PrinterFilamentContext != nil {
		fields["printer_filament_context"] = in.PrinterFilamentContext
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range fields {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if err := form.WriteField(key, string(raw)); err != nil {
			return nil, err
		}
	}
	if in.Output != "" {
		form.WriteField("output", in.Output)
	}
	if in.Filename != "" {
		form.WriteField("filename", in.Filename)
	}
	if in.PlateIndex != 0 && in.Plate == nil && (in.ProjectID == "" || len(project.document.Plates) > 1) {
		form.WriteField("plate_index", strconv.Itoa(in.PlateIndex))
	}
	if in.ForcePlanar {
		form.WriteField("force_planar", "1")
	}
	if in.AllowMixedTemperatures {
		form.WriteField("allow_mixed_temperatures", "1")
	}
	if err := form.Close(); err != nil {
		return nil, err
	}
	if in.ProjectID != "" {
		ctx = context.WithValue(ctx, sourceProjectKey{}, &jobSourceProject{ID: project.id, Revision: project.record.Revision, Plate: in.PlateIndex})
	}
	r := mcpHTTPRequest(ctx, call, http.MethodPost, "/api/jobs", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := &mcpResponse{}
	m.server.createJob(w, r)
	out, err := w.result()
	if err != nil {
		return nil, err
	}
	result := map[string]any{"id": out["id"]}
	if in.ProjectID != "" {
		result["project_id"] = project.id
		result["project_revision"] = project.record.Revision
		result["plate_index"] = in.PlateIndex
	}
	return result, nil
}
