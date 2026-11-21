package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
)

type preferencesWrite struct {
	OperationID      string          `json:"operationId"`
	ExpectedRevision uint64          `json:"expectedRevision"`
	Document         json.RawMessage `json:"document"`
	ExpectedETag     string          `json:"-"`
	DryRun           bool            `json:"-"`
}

func preferencesETag(v workspaceValue) string {
	return mcpHash([]any{v.Revision, normalizePreferences(v.Document)})
}

func (st *workspaceStore) preferencesWait() <-chan struct{} {
	st.watch.Lock()
	defer st.watch.Unlock()
	if st.preferencesChanged == nil {
		st.preferencesChanged = make(chan struct{})
	}
	return st.preferencesChanged
}

func (st *workspaceStore) preferencesWritten() {
	st.watch.Lock()
	defer st.watch.Unlock()
	if st.preferencesChanged != nil {
		close(st.preferencesChanged)
	}
	st.preferencesChanged = make(chan struct{})
}

func (st *workspaceStore) preferencesRevision() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	d, err := st.readSnapshot()
	if err != nil {
		return 0
	}
	return d.Values["preferences"].Revision
}

func (st *workspaceStore) writePreferences(in preferencesWrite) (map[string]any, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	d, err := st.readSnapshot()
	if err != nil {
		return nil, errors.New("saved preferences are unavailable")
	}
	v := d.Values["preferences"]
	fail := func(status int, code, message string) (map[string]any, error) {
		return nil, &settingsWriteError{Status: status, Code: code, Message: message, Recovery: "reload_and_reapply", Revision: v.Revision}
	}
	if in.OperationID == "" || len(in.OperationID) > 200 {
		return fail(400, "operation_required", "operationId is required")
	}
	digest := workspaceDigest([]any{"preferences", struct {
		OperationID      string          `json:"operationId"`
		ExpectedRevision uint64          `json:"expectedRevision"`
		Document         json.RawMessage `json:"document"`
	}{in.OperationID, in.ExpectedRevision, in.Document}})
	if receipt, ok := d.Operations[in.OperationID]; ok && !in.DryRun {
		if receipt.Digest != digest {
			return fail(409, "operation_conflict", "operationId was already used for different data")
		}
		var out map[string]any
		err := json.Unmarshal(receipt.Response, &out)
		return out, err
	}
	if v.Revision != in.ExpectedRevision || in.ExpectedETag != "" && in.ExpectedETag != preferencesETag(v) {
		return fail(http.StatusConflict, "revision_conflict", "preferences changed; read them again; current etag="+preferencesETag(v))
	}
	if v.Revision == math.MaxUint64 {
		return fail(http.StatusConflict, "revision_exhausted", "preferences reached their maximum revision")
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(in.Document, &document) != nil || document == nil {
		return fail(400, "invalid_document", "preferences must be an object")
	}
	before := normalizePreferences(v.Document)
	v.Document = normalizePreferences(in.Document)
	v.History = appendSettingsRevision(v.History, v.Revision, before)
	v.Revision++
	v.History = appendSettingsRevision(v.History, v.Revision, v.Document)
	out := map[string]any{"revision": v.Revision, "document": v.Document, "operationId": in.OperationID, "diff": settingsDiff(before, v.Document), "dry_run": in.DryRun}
	if in.DryRun {
		return out, nil
	}
	d.Values["preferences"] = v
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	putReceipt(d.Operations, in.OperationID, workspaceReceipt{Digest: digest, Response: raw}, func(r workspaceReceipt) uint64 { return r.Seq }, func(r workspaceReceipt, seq uint64) workspaceReceipt { r.Seq = seq; return r })
	if err := st.save(d); err != nil {
		return fail(507, "workspace_write_failed", "could not save preferences")
	}
	st.preferencesWritten()
	return out, nil
}
