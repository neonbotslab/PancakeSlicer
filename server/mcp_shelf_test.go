package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpShelfFixture struct {
	workspace http.Handler
	http      *httptest.Server
	now       atomic.Int64
}

func newMCPShelfFixture(t *testing.T) *mcpShelfFixture {
	t.Helper()
	s, _ := shelfFixture(t)
	f := &mcpShelfFixture{workspace: shelfWorkspaceFixture(t, s)}
	f.now.Store(time.Now().UnixNano())
	s.cfg.MCPEnabled, s.cfg.MCPConfirmationTTL = true, time.Minute
	service := newMCPService(s)
	service.now = func() time.Time { return time.Unix(0, f.now.Load()) }
	f.http = httptest.NewServer(service.handler())
	t.Cleanup(f.http.Close)
	return f
}

func (f *mcpShelfFixture) connect(t *testing.T, options *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	return mcpTestConnect(t, f.http, f.http.Client(), options)
}

type mcpShelfUploadReport struct {
	Revision uint64          `json:"revision"`
	Imported []shelfImported `json:"imported"`
	Folders  []shelfFolder   `json:"folders"`
	Skipped  []shelfSkipped  `json:"skipped"`
}

func mcpShelfFixtureUpload(t *testing.T, session *mcp.ClientSession, revision uint64, name string, model []byte) mcpShelfUploadReport {
	t.Helper()
	var report mcpShelfUploadReport
	mcpTestCall(t, session, "upload_shelf_model", mcpShelfUploadInput{ExpectedRevision: revision, Name: name, Data: base64.StdEncoding.EncodeToString(model)}, &report)
	return report
}

func TestMCPShelfUploadListAndPlacement(t *testing.T) {
	f := newMCPShelfFixture(t)
	session := f.connect(t, nil)
	var shelf shelfDocument
	mcpTestCall(t, session, "list_shelf", nil, &shelf)
	if shelf.Revision != 0 || len(shelf.Folders)+len(shelf.Models) != 0 {
		t.Fatalf("initial Shelf = %+v", shelf)
	}
	plain := mcpShelfFixtureUpload(t, session, 0, "plain.stl", []byte(shelfTestSTL))
	if plain.Revision != 1 || len(plain.Imported) != 1 || plain.Imported[0].Model.Name != "plain.stl" {
		t.Fatalf("plain upload = %+v", plain)
	}
	archive := shelfZIP(t, map[string][]byte{"parts/nested.stl": []byte(shelfTestSTL), "readme.txt": []byte("skip")})
	mcpTestRefusal(t, mcpTestCall(t, session, "upload_shelf_model", mcpShelfUploadInput{ExpectedRevision: 0, Name: "stale.zip", Data: base64.StdEncoding.EncodeToString(archive)}, nil), "revision_conflict")
	zip := mcpShelfFixtureUpload(t, session, 1, "batch.zip", archive)
	if zip.Revision != 2 || len(zip.Folders) != 1 || len(zip.Imported) != 1 || len(zip.Skipped) != 1 || zip.Imported[0].Model.SHA256 != plain.Imported[0].Model.SHA256 {
		t.Fatalf("ZIP upload = %+v", zip)
	}
	folder, model := zip.Folders[0], zip.Imported[0].Model
	mcpTestCall(t, session, "list_shelf", mcpShelfListInput{FolderID: &folder.ID}, &shelf)
	if shelf.Revision != 2 || len(shelf.Folders) != 0 || len(shelf.Models) != 1 || shelf.Models[model.ID] != model {
		t.Fatalf("folder listing = %+v", shelf)
	}

	const record = `{"meta":{"name":"Fixture","savedAt":1,"objects":0},"doc":{"printer":"Printer","plateIndex":1,"objects":[],"projectPlates":[]}}`
	shelfCreateProject(t, f.workspace, shelfTestProject, json.RawMessage(record))
	place := mcpShelfPlateInput{
		mcpPlateInput: mcpPlateInput{mcpProjectInput: mcpProjectInput{ProjectID: shelfTestProject}, PlateIndex: 1},
		ModelID:       model.ID, ExpectedProjectRevision: 1, ExpectedRevision: 2, OperationID: "mcp-add",
	}
	mcpTestRefusal(t, mcpTestCall(t, session, "add_shelf_model_to_plate", place, nil), "lease_held")
	leaseURL := "/api/project-leases/" + shelfTestProject
	shelfRequireStatus(t, shelfJSON(t, f.workspace, "POST", leaseURL+"/release", map[string]string{"session": shelfTestSession}, nil), http.StatusNoContent)
	var result shelfTestPlacement
	mcpTestCall(t, session, "add_shelf_model_to_plate", place, &result)
	if result.ProjectRevision != 2 || result.PlateIndex != 1 || result.ObjectID != 1 || result.InstanceID != 2 || result.Source != "sha256-"+result.SHA256 {
		t.Fatalf("MCP placement = %+v", result)
	}
	var retry shelfTestPlacement
	mcpTestCall(t, session, "add_shelf_model_to_plate", place, &retry)
	if result != retry {
		t.Fatalf("MCP retry = %+v, want %+v", retry, result)
	}
	w := shelfJSON(t, f.workspace, http.MethodGet, leaseURL, nil, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	var lease struct{ Held bool }
	if err := json.Unmarshal(w.Body.Bytes(), &lease); err != nil || lease.Held {
		t.Fatalf("MCP placement kept the editor lease: %s", w.Body)
	}
}

type mcpShelfPreparedResult struct {
	Token            string                `json:"token"`
	Digest           string                `json:"digest"`
	Summary          mcpShelfDeleteSummary `json:"summary"`
	ApprovalRequired bool                  `json:"approval_required"`
}

func (r mcpShelfPreparedResult) approve() mcpShelfDeleteConfirmation {
	return mcpShelfDeleteConfirmation{Token: r.Token, Digest: r.Digest, HumanApproved: true}
}

func mcpShelfPrepare(t *testing.T, session *mcp.ClientSession, in mcpShelfDeleteInput) mcpShelfPreparedResult {
	t.Helper()
	var prepared mcpShelfPreparedResult
	mcpTestCall(t, session, "prepare_shelf_delete", in, &prepared)
	if !prepared.ApprovalRequired || prepared.Token == "" || prepared.Digest == "" || prepared.Summary.ID != in.ID || prepared.Summary.Revision != in.ExpectedRevision {
		t.Fatalf("invalid preparation: %+v", prepared)
	}
	return prepared
}

func TestMCPShelfDeleteConfirmationGuards(t *testing.T) {
	f := newMCPShelfFixture(t)
	session := f.connect(t, nil)
	other := f.connect(t, nil)
	report := mcpShelfFixtureUpload(t, session, 0, "fixture.stl", []byte(shelfTestSTL))
	model := report.Imported[0].Model
	in := mcpShelfDeleteInput{ID: model.ID, Kind: "model", ExpectedRevision: report.Revision}
	assertPresent := func(t *testing.T, revision uint64) {
		t.Helper()
		var shelf shelfDocument
		mcpTestCall(t, session, "list_shelf", nil, &shelf)
		if shelf.Revision != revision || shelf.Models[model.ID].ID != model.ID {
			t.Fatalf("refused confirmation changed Shelf: %+v", shelf)
		}
	}
	for _, tc := range []struct {
		name, refusal string
		change        func(*mcpShelfDeleteConfirmation)
	}{
		{"mismatched digest", "does not match", func(in *mcpShelfDeleteConfirmation) { in.Digest = strings.Repeat("0", 64) }},
		{"approval omitted", "approval is required", func(in *mcpShelfDeleteConfirmation) { in.HumanApproved = false }},
		{"expired", "expired", func(*mcpShelfDeleteConfirmation) { f.now.Add(int64(2 * time.Minute)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mcpShelfPrepare(t, session, in)
			confirmation := p.approve()
			tc.change(&confirmation)
			mcpTestRefusal(t, mcpTestCall(t, session, "confirm_shelf_delete", confirmation, nil), tc.refusal)
			mcpTestRefusal(t, mcpTestCall(t, session, "confirm_shelf_delete", p.approve(), nil), "already used")
			assertPresent(t, 1)
		})
	}
	p := mcpShelfPrepare(t, session, in)
	mcpTestRefusal(t, mcpTestCall(t, other, "confirm_shelf_delete", p.approve(), nil), "unknown or already used")
	assertPresent(t, 1)
	w := shelfJSON(t, f.workspace, "PATCH", "/api/shelf/models/"+model.ID, map[string]any{"name": "changed.stl", "expectedRevision": 1}, nil)
	shelfRequireStatus(t, w, http.StatusOK)
	mcpTestRefusal(t, mcpTestCall(t, session, "confirm_shelf_delete", p.approve(), nil), "shelf changed")
	mcpTestRefusal(t, mcpTestCall(t, session, "confirm_shelf_delete", p.approve(), nil), "already used")
	assertPresent(t, 2)
}

func TestMCPShelfDeleteElicitsExactRecursiveTarget(t *testing.T) {
	f := newMCPShelfFixture(t)
	client := f.connect(t, nil)
	zip := shelfZIP(t, map[string][]byte{"parts/deep/one.stl": []byte(shelfTestSTL), "other.stl": []byte(shelfTestSTL)})
	report := mcpShelfFixtureUpload(t, client, 0, "fixture.zip", zip)
	if len(report.Folders) != 2 || len(report.Imported) != 2 {
		t.Fatalf("bad fixture: %+v", report)
	}
	var folder shelfFolder
	for _, each := range report.Folders {
		if each.Parent == "" {
			folder = each
		}
	}
	in := mcpShelfDeleteInput{ID: folder.ID, Kind: "folder", ExpectedRevision: report.Revision}
	mcpTestRefusal(t, mcpTestCall(t, client, "prepare_shelf_delete", in, nil), "recursive deletion is required")
	in.Recursive = true

	for _, tc := range []struct {
		name, action, refusal string
		approve, expire       bool
	}{
		{"decline", "decline", "elicitation", false, false},
		{"unchecked", "accept", "elicitation", false, false},
		{"expire during approval", "accept", "expired during approval", true, true},
		{"approve", "accept", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompts := make(chan string, 1)
			session := f.connect(t, &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				prompts <- req.Params.Message
				if tc.expire {
					f.now.Add(int64(2 * time.Minute))
				}
				return &mcp.ElicitResult{Action: tc.action, Content: map[string]any{"approve": tc.approve}}, nil
			}})
			prepared := mcpShelfPrepare(t, session, in)
			if len(prepared.Summary.Folders) != 2 || len(prepared.Summary.Models) != 1 || prepared.Summary.Models[0].Name != "one.stl" {
				t.Fatalf("recursive summary = %+v", prepared.Summary)
			}
			if tc.refusal != "" {
				mcpTestRefusal(t, mcpTestCall(t, session, "confirm_shelf_delete", prepared.approve(), nil), tc.refusal)
			} else {
				var result struct{ Revision uint64 }
				mcpTestCall(t, session, "confirm_shelf_delete", prepared.approve(), &result)
				if result.Revision != 2 {
					t.Fatalf("new Shelf revision = %d, want 2", result.Revision)
				}
			}
			select {
			case prompt := <-prompts:
				_, detail, ok := strings.Cut(prompt, "\n")
				var shown mcpShelfDeleteSummary
				if err := json.Unmarshal([]byte(detail), &shown); !ok || err != nil || !reflect.DeepEqual(shown, prepared.Summary) {
					t.Fatalf("approval prompt = %q, want %+v", prompt, prepared.Summary)
				}
			default:
				t.Fatal("client advertised elicitation and received no approval prompt")
			}
			mcpTestRefusal(t, mcpTestCall(t, session, "confirm_shelf_delete", prepared.approve(), nil), "already used")
			var shelf shelfDocument
			mcpTestCall(t, client, "list_shelf", nil, &shelf)
			if tc.refusal != "" && (shelf.Revision != 1 || len(shelf.Folders) != 2 || len(shelf.Models) != 2) {
				t.Fatalf("refused approval changed Shelf: %+v", shelf)
			}
			if tc.refusal == "" {
				if shelf.Revision != 2 || len(shelf.Folders) != 0 || len(shelf.Models) != 1 {
					t.Fatalf("approved removal left the wrong Shelf: %+v", shelf)
				}
				for _, remaining := range shelf.Models {
					if remaining.Name != "other.stl" {
						t.Fatalf("deleted an unapproved model: %+v", shelf)
					}
				}
			}
		})
	}
}
