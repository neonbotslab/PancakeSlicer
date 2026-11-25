package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpShelfListInput struct {
	FolderID *string `json:"folder_id,omitempty" jsonschema:"Return immediate children of this folder. Use an empty string for the root or omit to list the full tree."`
}

type mcpShelfUploadInput struct {
	FolderID         string `json:"folder_id,omitempty" jsonschema:"Destination folder ID. Omit for the root."`
	ExpectedRevision uint64 `json:"expected_revision" jsonschema:"Shelf revision returned by list_shelf"`
	Name             string `json:"name" jsonschema:"STL, OBJ, 3MF, STEP, STP, or ZIP file basename"`
	Data             string `json:"data" jsonschema:"File bytes in standard base64"`
}

type mcpShelfPlateInput struct {
	mcpPlateInput
	ModelID                 string   `json:"model_id" jsonschema:"Shelf model ID returned by list_shelf"`
	ExpectedProjectRevision uint64   `json:"expected_project_revision" jsonschema:"Saved project revision returned by get_project"`
	ExpectedRevision        uint64   `json:"expected_revision" jsonschema:"Shelf revision returned by list_shelf"`
	OperationID             string   `json:"operation_id" jsonschema:"Stable operation ID. Reuse this exact ID when retrying the same placement."`
	Session                 string   `json:"session,omitempty" jsonschema:"Existing workspace editing session ID if operating on behalf of the editor that holds its lease"`
	X                       *float64 `json:"x,omitempty" jsonschema:"X in millimeters on the target plate. Omit both x and y to find a free spot on the saved printer's bed."`
	Y                       *float64 `json:"y,omitempty" jsonschema:"Y in millimeters on the target plate. Omit both x and y to find a free spot on the saved printer's bed."`
}

type mcpShelfDeleteInput struct {
	ID               string `json:"id" jsonschema:"Exact Shelf item ID returned by list_shelf"`
	Kind             string `json:"kind" jsonschema:"model or folder"`
	ExpectedRevision uint64 `json:"expected_revision" jsonschema:"Shelf revision returned by list_shelf"`
	Recursive        bool   `json:"recursive,omitempty" jsonschema:"True to include all descendants of a nonempty folder"`
}

type mcpShelfDeleteConfirmation struct {
	Token         string `json:"token"`
	Digest        string `json:"digest" jsonschema:"Exact digest returned by prepare_shelf_delete"`
	HumanApproved bool   `json:"human_approved" jsonschema:"True only after the human explicitly approves the entire prepared summary"`
}

type mcpShelfDeleteSummary struct {
	ID        string        `json:"id"`
	Kind      string        `json:"kind"`
	Revision  uint64        `json:"revision"`
	Recursive bool          `json:"recursive"`
	Folders   []shelfFolder `json:"folders"`
	Models    []shelfModel  `json:"models"`
	ExpiresAt time.Time     `json:"expires_at"`
}

type mcpPreparedShelfDelete struct {
	session string
	digest  string
	summary mcpShelfDeleteSummary
}

func (m *mcpService) addShelfTools(protocol *mcp.Server) {
	mcpTool(protocol, "list_shelf", "List the saved model Shelf and its revision. Optionally list immediate children of one folder.", true, m.listShelf)
	mcpTool(protocol, "upload_shelf_model", "Save a base64-encoded model or ZIP archive to the Shelf. Returns imported items, skipped files and the new Shelf revision.", false, m.uploadShelfModel)
	mcpTool(protocol, "add_shelf_model_to_plate", "Add a Shelf model to an unlocked plate of a saved project. Requires the current Shelf and project revisions and a stable operation ID. Supply x and y or let the server find a free spot on the saved printer's bed. This only saves the project.", false, m.addShelfModelToPlate)
	mcpTool(protocol, "prepare_shelf_delete", "Prepare a Shelf deletion and return the exact items it would remove. Show the full summary to the human and obtain explicit approval before confirm_shelf_delete.", false, m.prepareShelfDelete)
	mcpTool(protocol, "confirm_shelf_delete", "Delete the exact prepared Shelf items after explicit human approval. Requires the session-bound single-use token, exact digest, and human_approved=true. Form elicitation must also be accepted when supported. Never infer approval.", false, m.confirmShelfDelete)
}

func (m *mcpService) listShelf(ctx context.Context, call *mcp.CallToolRequest, in mcpShelfListInput) (map[string]any, error) {
	query := url.Values{"tree": {"1"}}
	if in.FolderID != nil {
		query = url.Values{"folder": {*in.FolderID}}
	}
	r := mcpHTTPRequest(ctx, call, http.MethodGet, "/api/shelf?"+query.Encode(), nil)
	w := &mcpResponse{}
	m.server.listShelf(w, r)
	return w.result()
}

func (m *mcpService) uploadShelfModel(ctx context.Context, call *mcp.CallToolRequest, in mcpShelfUploadInput) (map[string]any, error) {
	if !shelfName(in.Name) {
		return nil, errors.New("name must be a safe file basename")
	}
	decode := func() io.Reader {
		return mcpContextReader{ctx, base64.NewDecoder(base64.StdEncoding, strings.NewReader(in.Data))}
	}
	limit := shelfUploadLimits(m.server.cfg).FileBytes
	size, err := io.Copy(io.Discard, io.LimitReader(decode(), limit+1))
	if err != nil || size == 0 {
		return nil, errors.New("data must contain a file encoded in standard base64")
	}
	if size > limit {
		return nil, errors.New("file exceeds the Shelf upload limit")
	}
	var framing bytes.Buffer
	multipartBody := multipart.NewWriter(&framing)
	if _, err := multipartBody.CreateFormFile("file", in.Name); err != nil {
		return nil, err
	}
	offset := framing.Len()
	if err := multipartBody.Close(); err != nil {
		return nil, err
	}
	head, tail := framing.Bytes()[:offset], framing.Bytes()[offset:]
	body := io.MultiReader(bytes.NewReader(head), decode(), bytes.NewReader(tail))
	query := url.Values{"folder": {in.FolderID}}
	r := mcpHTTPRequest(ctx, call, http.MethodPost, "/api/shelf/uploads?"+query.Encode(), body)
	r.Header.Set("If-Match", strconv.FormatUint(in.ExpectedRevision, 10))
	r.Header.Set("Content-Type", multipartBody.FormDataContentType())
	w := &mcpResponse{}
	m.server.uploadShelf(w, r)
	return w.result()
}

func (m *mcpService) addShelfModelToPlate(ctx context.Context, call *mcp.CallToolRequest, in mcpShelfPlateInput) (map[string]any, error) {
	body, err := json.Marshal(shelfPlateInput{
		ProjectID: in.ProjectID, PlateIndex: in.PlateIndex, ExpectedProjectRevision: &in.ExpectedProjectRevision,
		ExpectedRevision: &in.ExpectedRevision, OperationID: in.OperationID, X: in.X, Y: in.Y,
	})
	if err != nil {
		return nil, err
	}
	r := mcpHTTPRequest(ctx, call, http.MethodPost, "/api/shelf/models/"+url.PathEscape(in.ModelID)+"/add-to-plate", bytes.NewReader(body))
	r.SetPathValue("id", in.ModelID)
	session := in.Session
	if session == "" {
		session = "mcp-" + mcpHash(call.Session.ID())
	}
	r.Header.Set(sessionHeader, session)
	w := &mcpResponse{}
	m.server.shelfModelToPlate(w, r, true)
	return w.result()
}

func (m *mcpService) prepareShelfDelete(_ context.Context, call *mcp.CallToolRequest, in mcpShelfDeleteInput) (map[string]any, error) {
	shelf, err := m.server.shelfSnapshot()
	if err != nil {
		return nil, errors.New("Shelf storage is unavailable")
	}
	if err := shelf.checkRevision(in.ExpectedRevision); err != nil {
		return nil, err
	}
	if in.Kind != "model" && in.Kind != "folder" {
		return nil, errors.New("kind must be model or folder")
	}
	targets, err := shelf.deletionTargets(in.ID, in.Kind == "folder", in.Recursive)
	if err != nil {
		return nil, err
	}
	summary := mcpShelfDeleteSummary{ID: in.ID, Kind: in.Kind, Revision: targets.Revision, Recursive: in.Recursive,
		Folders: make([]shelfFolder, 0, len(targets.Folders)), Models: make([]shelfModel, 0, len(targets.Models)), ExpiresAt: m.now().Add(m.ttl)}
	for _, id := range sortedKeys(targets.Folders) {
		summary.Folders = append(summary.Folders, targets.Folders[id])
	}
	for _, id := range sortedKeys(targets.Models) {
		summary.Models = append(summary.Models, targets.Models[id])
	}
	token := rand.Text()
	prepared := mcpPreparedShelfDelete{session: call.Session.ID(), digest: mcpHash(summary), summary: summary}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupShelfDeletesLocked()
	if len(m.preparedShelfDeletes) >= 128 {
		return nil, errors.New("Too many prepared Shelf deletions. Wait for confirmation tokens to expire")
	}
	m.preparedShelfDeletes[token] = prepared
	time.AfterFunc(m.ttl, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cleanupShelfDeletesLocked()
	})
	return map[string]any{"token": token, "digest": prepared.digest, "summary": summary, "approval_required": true}, nil
}

func (m *mcpService) cleanupShelfDeletesLocked() {
	for token, prepared := range m.preparedShelfDeletes {
		if !m.now().Before(prepared.summary.ExpiresAt) {
			delete(m.preparedShelfDeletes, token)
		}
	}
}

func (m *mcpService) confirmShelfDelete(ctx context.Context, call *mcp.CallToolRequest, in mcpShelfDeleteConfirmation) (map[string]any, error) {
	m.mu.Lock()
	prepared, found := m.preparedShelfDeletes[in.Token]
	sameSession := found && prepared.session == call.Session.ID()
	if sameSession {
		delete(m.preparedShelfDeletes, in.Token)
	}
	m.cleanupShelfDeletesLocked()
	m.mu.Unlock()
	if !sameSession {
		return nil, errors.New("Unknown or already used confirmation token")
	}
	expires := prepared.summary.ExpiresAt
	if !m.now().Before(expires) {
		return nil, errors.New("Confirmation token expired. Prepare again and obtain fresh approval")
	}
	if in.Digest != prepared.digest {
		return nil, errors.New("Confirmation does not match the prepared Shelf deletion")
	}
	if !in.HumanApproved {
		return nil, errors.New("Explicit human approval is required")
	}
	if params := call.Session.InitializeParams(); params != nil && params.Capabilities != nil && params.Capabilities.Elicitation != nil {
		caps := params.Capabilities.Elicitation
		if caps.Form == nil && caps.URL != nil {
			return nil, errors.New("Client only supports URL elicitation. Shelf deletion approval requires form elicitation")
		}
		summary := must(json.MarshalIndent(prepared.summary, "", "  "))
		approvalCtx, cancel := context.WithTimeout(ctx, expires.Sub(m.now()))
		defer cancel()
		answer, err := call.Session.Elicit(approvalCtx, &mcp.ElicitParams{
			Message: "Delete these exact Shelf items?\n" + string(summary),
			RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{
				"approve": map[string]any{"type": "boolean", "title": "Delete these Shelf items"},
			}, "required": []string{"approve"}},
		})
		if err != nil || answer == nil || answer.Action != "accept" || answer.Content["approve"] != true {
			return nil, errors.New("Human elicitation was declined, cancelled, or failed. Shelf deletion refused")
		}
	}
	if !m.now().Before(expires) {
		return nil, errors.New("Confirmation token expired during approval")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	summary := prepared.summary
	shelf, err := m.server.shelfDelete(summary.ID, summary.Kind == "folder", summary.Revision, summary.Recursive)
	if err != nil {
		return nil, err
	}
	return map[string]any{"revision": shelf.Revision, "deleted": summary}, nil
}
