package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"time"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

type settingsWriteError struct {
	Status   int
	Code     string
	Message  string
	Recovery string
	Revision uint64
}

func (e *settingsWriteError) Error() string {
	if e.Revision == 0 {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s Current revision is %d.", e.Code, e.Message, e.Revision)
}

type librarySettingsWrite struct {
	ExpectedRevision uint64                     `json:"expectedRevision"`
	ExpectedETag     string                     `json:"-"`
	OperationID      string                     `json:"operationId"`
	Document         map[string]json.RawMessage `json:"document"`
	DryRun           bool                       `json:"-"`
	ForceRevisionFor string                     `json:"-"`
}

type librarySettingsResult struct {
	Envelope *libraryEnvelope
	Before   map[string]json.RawMessage
	Revision uint64
}

func (l *libraryStore) readSettingsSnapshot() (*libraryEnvelope, error) {
	e, _, err := l.readEnvelope()
	if err != nil {
		return nil, err
	}
	var schema int
	if json.Unmarshal(e.Document["schema"], &schema) != nil || schema != librarySchema {
		return nil, &settingsWriteError{
			Status: http.StatusConflict, Code: "library_upgrade_needed",
			Message: fmt.Sprintf("Settings require profile library schema %d.", librarySchema), Recovery: "reload",
			Revision: resolve.LibraryRevision(e.Document),
		}
	}
	return e, nil
}

func (s *Server) writeLibrarySettings(in librarySettingsWrite) (map[string]any, error) {
	result, failure := s.applyLibrarySettings(in, nil)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{
		"document": result.Envelope.Document,
		"revision": result.Revision,
		"diff":     settingsDiff(result.Before, result.Envelope.Document),
		"dry_run":  in.DryRun,
	}, nil
}

func (s *Server) applyLibrarySettings(in librarySettingsWrite, onCommit func(map[string]json.RawMessage)) (librarySettingsResult, *settingsWriteError) {
	l := s.library
	l.mu.Lock()
	defer l.mu.Unlock()
	uiWrite := onCommit != nil && !in.DryRun
	read := l.readSettingsSnapshot
	if uiWrite {
		read = l.read
	}
	e, err := read()
	if err != nil {
		var failure *settingsWriteError
		if errors.As(err, &failure) {
			return librarySettingsResult{}, failure
		}
		return librarySettingsResult{}, &settingsWriteError{
			Status: http.StatusServiceUnavailable, Code: "library_unavailable",
			Message: "Oops! Couldn't read the stored library, so nothing was changed.", Recovery: "retry_storage",
		}
	}
	revision := resolve.LibraryRevision(e.Document)
	fail := func(status int, code, message, recovery string) (librarySettingsResult, *settingsWriteError) {
		return librarySettingsResult{}, &settingsWriteError{status, code, message, recovery, revision}
	}
	result := librarySettingsResult{Envelope: e, Before: e.Document, Revision: revision}
	var digest string
	if !in.DryRun && in.OperationID != "" {
		if uiWrite {
			digest = workspaceDigest([]any{in.ExpectedRevision, in.Document})
		} else {
			digest = workspaceDigest([]any{"settings", in.ExpectedRevision, in.ExpectedETag, in.Document, in.ForceRevisionFor})
		}
		if receipt, ok := e.Operations[in.OperationID]; ok {
			if receipt.Digest != digest {
				return fail(http.StatusConflict, "operation_conflict", "Heads up — this operation ID was already used for different changes.", "reload")
			}
			result.Revision = receipt.Revision
			return result, nil
		}
	}
	doc := maps.Clone(in.Document)
	if stringValue(doc["epoch"]) != stringValue(e.Document["epoch"]) {
		return fail(http.StatusConflict, "library_reset", "Profiles were reset! Give them a reload.", "reload")
	}
	currentETag := ""
	if in.ExpectedETag != "" {
		currentETag = mcpHash(e.Document)
	}
	if in.ExpectedRevision != revision || resolve.LibraryRevision(doc) != revision || in.ExpectedETag != currentETag {
		message := "The library changed elsewhere — reload it and reapply your change."
		if currentETag != "" {
			message += " Current revision token is " + currentETag + "."
		}
		return fail(http.StatusConflict, "revision_conflict", message, "reload")
	}
	previous := l.stored(e)
	if previous != nil {
		if dropped := droppedProfiles(previous, doc); len(dropped) > 0 {
			owner, profile := slotUser(doc, dropped)
			if profile.Name == "" {
				owner, profile, err = s.workspace.profileReference(dropped, !uiWrite)
				if err != nil {
					return fail(http.StatusServiceUnavailable, "workspace_unavailable", "Oops! Couldn't read your saved projects, so we didn't delete any profiles.", "retry_storage")
				}
			}
			if profile.Name != "" {
				message := owner + " uses " + profile.Name + ". Archive it instead."
				if profile.Archived {
					message = owner + " uses " + profile.Name + ", so it stays archived."
				}
				return fail(http.StatusConflict, "profile_referenced", message, "archive_profile")
			}
			dropCalibrations(doc, dropped)
		}
	}
	if err := l.deviceDefaults(doc); err != nil {
		return fail(http.StatusBadRequest, "library_invalid", err.Error(), "repair_profiles")
	}
	changes, err := stampLibraryRecords(doc, e, in.ForceRevisionFor)
	if err != nil {
		return fail(http.StatusBadRequest, "library_invalid", err.Error(), "repair_profiles")
	}
	next, err := l.catalog.ParseLibrary(doc, previous)
	if err != nil {
		return fail(http.StatusBadRequest, "library_invalid", err.Error(), "repair_profiles")
	}
	if previous != nil && livePrinters(previous) > 0 && livePrinters(next) == 0 {
		return fail(http.StatusConflict, "last_printer", "Keep at least one printer!", "reload")
	}
	if name := editedWhileRemoved(e.Document, doc); name != "" {
		return fail(http.StatusConflict, "removed", name+" was removed in another window, so this change wasn't saved.", "reload")
	}
	if kind, taken := takenName(previous, next); taken != "" {
		return fail(http.StatusConflict, "name_taken", "Oops! There's already a "+kind+" named "+taken+".", "rename")
	}
	if sameLibraryDocument(doc, e.Document) {
		return result, nil
	}
	if revision == math.MaxUint64 {
		return fail(http.StatusConflict, "revision_exhausted", "The library has reached its maximum revision.", "reload")
	}
	doc["revision"] = must(json.Marshal(revision + 1))
	e.Document = doc
	if in.DryRun {
		return result, nil
	}
	if len(changes) > 0 && e.History == nil {
		e.History = make(map[string][]settingsRevision, len(changes))
	}
	for key, change := range changes {
		history := e.History[key]
		if len(history) == 0 && change.Before != nil {
			history = appendSettingsRevision(history, change.BeforeRevision, change.Before)
		}
		e.History[key] = appendSettingsRevision(history, change.Revision, change.After)
	}
	if in.OperationID != "" {
		putReceipt(e.Operations, in.OperationID, libraryReceipt{Digest: digest, Revision: revision + 1}, func(r libraryReceipt) uint64 { return r.Seq },
			func(r libraryReceipt, seq uint64) libraryReceipt { r.Seq = seq; return r })
	}
	if err := l.write(e); err != nil {
		return fail(http.StatusInsufficientStorage, "library_write_failed", "Uh-oh, couldn't save profiles! Free up a little storage and try again.", "retry_storage")
	}
	if uiWrite {
		onCommit(doc)
	}
	result.Revision = revision + 1
	return result, nil
}

type libraryRecordChange struct {
	Before         json.RawMessage
	BeforeRevision uint64
	After          json.RawMessage
	Revision       uint64
}

func stampLibraryRecords(doc map[string]json.RawMessage, previous *libraryEnvelope, forceRevisionFor string) (map[string]libraryRecordChange, error) {
	now := must(json.Marshal(time.Now().UTC().Format(time.RFC3339)))
	changes := map[string]libraryRecordChange{}
	for _, list := range []struct{ Section, Kind string }{
		{"printers", "printer"}, {"processes", "process"}, {"filaments", "filament"}, {"calibrations", "calibration"},
	} {
		var after, before []json.RawMessage
		if json.Unmarshal(doc[list.Section], &after) != nil || after == nil {
			continue
		}
		json.Unmarshal(previous.Document[list.Section], &before)
		old := map[string]json.RawMessage{}
		for _, r := range before {
			var record map[string]json.RawMessage
			if json.Unmarshal(r, &record) == nil {
				if id := stringValue(record["id"]); id != "" {
					old[id] = r
				}
			}
		}
		for i, r := range after {
			var record, stored map[string]json.RawMessage
			if json.Unmarshal(r, &record) != nil {
				continue
			}
			var revision *uint64
			if json.Unmarshal(record["revision"], &revision) != nil || revision == nil {
				continue
			}
			id := stringValue(record["id"])
			oldRecord := old[id]
			key := list.Kind + "/" + id
			json.Unmarshal(oldRecord, &stored)
			delete(old, id)
			if oldRecord != nil && sameRecordJSON(oldRecord, r) && key != forceRevisionFor {
				record["revision"] = stored["revision"]
				if list.Kind != "calibration" {
					if stamp, ok := stored["edited"]; ok {
						record["edited"] = stamp
					} else {
						delete(record, "edited")
					}
				}
				after[i] = must(json.Marshal(record))
				continue
			}
			beforeRevision := resolve.LibraryRevision(stored)
			history := previous.History[key]
			if oldRecord != nil || len(history) > 0 {
				head := beforeRevision
				if len(history) > 0 {
					head = max(head, history[len(history)-1].Revision)
				}
				if head == math.MaxUint64 {
					return nil, fmt.Errorf("%s has reached its maximum revision", id)
				}
				*revision = head + 1
				record["revision"] = must(json.Marshal(*revision))
			}
			if list.Kind != "calibration" {
				record["edited"] = now
			}
			after[i] = must(json.Marshal(record))
			changes[key] = libraryRecordChange{oldRecord, beforeRevision, after[i], *revision}
		}
		for id, r := range old {
			var record map[string]json.RawMessage
			json.Unmarshal(r, &record)
			revision := resolve.LibraryRevision(record)
			if revision == math.MaxUint64 {
				return nil, fmt.Errorf("%s has reached its maximum revision", id)
			}
			changes[list.Kind+"/"+id] = libraryRecordChange{r, revision, json.RawMessage("null"), revision + 1}
		}
		doc[list.Section] = must(json.Marshal(after))
	}
	return changes, nil
}
