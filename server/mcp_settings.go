package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const settingsWriteSafety = " Show the validated diff to the human and obtain approval before saving. Use dry_run=true to preview without saving. Requires the exact revision from the read. No print confirmation token is needed. Never infer approval."

var settingsSlotColour = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

type mcpProfileInput struct {
	Kind      string `json:"kind" jsonschema:"process, printer, or filament"`
	ProfileID string `json:"profile_id"`
}

type mcpProfileWriteInput struct {
	Kind             string         `json:"kind" jsonschema:"process, printer, or filament"`
	ProfileID        string         `json:"profile_id"`
	ExpectedRevision string         `json:"expected_revision" jsonschema:"Exact opaque revision returned by get_profile or get_printer_defaults"`
	Section          string         `json:"section,omitempty" jsonschema:"values for process, settings for filament, machine or printOverrides for printer; defaults to the primary section"`
	Changes          map[string]any `json:"changes,omitempty" jsonschema:"Individual engine option keys to set; null removes an override"`
	Name             string         `json:"name,omitempty" jsonschema:"New name for rename_profile or save_profile_as"`
	DryRun           bool           `json:"dry_run,omitempty"`
}

type mcpPreferencesWriteInput struct {
	ExpectedRevision string         `json:"expected_revision"`
	Changes          map[string]any `json:"changes" jsonschema:"Allowed keys: orca.printer, orca.web.afterSlice, orca.web.flowCalibration, orca.web.openDeviceAfterPrint, orca.web.openDeviceDelay"`
	DryRun           bool           `json:"dry_run,omitempty"`
}

type mcpPrinterDefaultsInput struct {
	PrinterID string `json:"printer_id"`
}

type mcpSlotAssignment struct {
	SlotID     string  `json:"slot_id"`
	FilamentID *string `json:"filament_id,omitempty" jsonschema:"Saved filament profile ID; empty clears the slot"`
	Colour     *string `json:"colour,omitempty"`
	Tray       *string `json:"tray,omitempty" jsonschema:"Existing saved feeder position such as ams0.0 or ext.L; empty clears the tray binding"`
}

type mcpPrinterDefaultsWriteInput struct {
	PrinterID        string              `json:"printer_id"`
	ExpectedRevision string              `json:"expected_revision"`
	ProcessID        *string             `json:"process_id,omitempty" jsonschema:"Saved default process ID; empty clears it"`
	Slots            []mcpSlotAssignment `json:"slots,omitempty" jsonschema:"Patches addressed by stable slot_id; never array indexes"`
	DryRun           bool                `json:"dry_run,omitempty"`
}

type mcpSettingsHistoryInput struct {
	Kind      string `json:"kind" jsonschema:"process, printer, filament, or preferences"`
	ProfileID string `json:"profile_id,omitempty"`
}

type mcpSettingsRevertInput struct {
	Kind             string `json:"kind" jsonschema:"process, printer, filament, or preferences"`
	ProfileID        string `json:"profile_id,omitempty"`
	RevisionID       string `json:"revision_id" jsonschema:"Retained revision id from list_settings_history"`
	ExpectedRevision string `json:"expected_revision"`
	DryRun           bool   `json:"dry_run,omitempty"`
}

func (m *mcpService) addSettingsTools(protocol *mcp.Server) {
	mcpTool(protocol, "get_profile", "Read a saved process, printer, or filament profile and its opaque library revision for writes.", true, m.getProfile)
	mcpTool(protocol, "update_profile", "Validate and persist individual engine options on a saved profile; unknown and connection settings are rejected."+settingsWriteSafety, false, m.updateProfile)
	mcpTool(protocol, "save_profile_as", "Save a profile copy under a new name, optionally changing individual options. Does not select the copy or copy printer connections."+settingsWriteSafety, false, m.saveProfileAs)
	mcpTool(protocol, "rename_profile", "Rename a saved profile without changing its ID or references."+settingsWriteSafety, false, m.renameProfile)
	mcpTool(protocol, "get_preferences", "Read server-side Defaults preferences and the remembered startup printer, with an opaque revision. Does not read authentication or connection preferences.", true, m.getPreferences)
	mcpTool(protocol, "update_preferences", "Persist allowed Defaults preferences or the remembered startup printer. Its vendor is derived from the saved printer."+settingsWriteSafety, false, m.updatePreferences)
	mcpTool(protocol, "get_printer_defaults", "Read a saved printer's default process and stable slot filament, colour, and AMS tray assignments. Does not contact a printer.", true, m.getPrinterDefaults)
	mcpTool(protocol, "update_printer_defaults", "Persist a printer's default process and stable slot assignments using saved profile and slot IDs. Does not change feeder topology or contact hardware."+settingsWriteSafety, false, m.updatePrinterDefaults)
	mcpTool(protocol, "list_settings_history", "List retained saved profile or preferences revisions, their IDs, snapshots, and diffs. History includes UI and MCP writes; at most 64 snapshots per target are retained.", true, m.listSettingsHistory)
	mcpTool(protocol, "revert_settings", "Restore a target to a retained revision ID as a new revision. Validates restored settings and preserves unrelated profiles and non-exposed preferences. This replaces the target's current saved settings."+settingsWriteSafety, false, m.revertSettings)
}

func profileCollection(kind string) (string, error) {
	switch kind {
	case "process":
		return "processes", nil
	case "printer":
		return "printers", nil
	case "filament":
		return "filaments", nil
	default:
		return "", errors.New("kind must be process, printer, or filament")
	}
}

func libraryProfile(doc map[string]json.RawMessage, kind, id string) (map[string]json.RawMessage, error) {
	collection, err := profileCollection(kind)
	if err != nil {
		return nil, err
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(doc[collection], &records); err != nil {
		return nil, errors.New("profile library is unreadable")
	}
	for _, record := range records {
		if stringValue(record["id"]) == id {
			if archived := record["archived"]; len(archived) > 0 && string(archived) != "null" {
				return nil, errors.New("profile is archived")
			}
			return record, nil
		}
	}
	return nil, errors.New("saved profile not found")
}

func replaceLibraryProfile(doc map[string]json.RawMessage, kind string, record map[string]json.RawMessage, create bool) error {
	collection, err := profileCollection(kind)
	if err != nil {
		return err
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(doc[collection], &records); err != nil {
		return err
	}
	for i, old := range records {
		if stringValue(old["id"]) == stringValue(record["id"]) {
			if create {
				return errors.New("new profile ID already exists")
			}
			records[i] = record
			doc[collection] = must(json.Marshal(records))
			return nil
		}
	}
	if !create {
		return errors.New("saved profile not found")
	}
	doc[collection] = must(json.Marshal(append(records, record)))
	return nil
}

func safeProfile(record map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(record)
	for _, section := range []string{"values", "settings", "machine", "printOverrides"} {
		if raw, ok := out[section]; ok {
			var values map[string]json.RawMessage
			json.Unmarshal(raw, &values)
			for key := range values {
				if blockedSettingChange(key) {
					delete(values, key)
					continue
				}
				values[key] = must(json.Marshal(safeSettingValue(settingsJSON(values[key]))))
			}
			out[section] = must(json.Marshal(values))
		}
	}
	return out
}

func safeSettingValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, child := range value {
			if !blockedSettingChange(key) {
				out[key] = safeSettingValue(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = safeSettingValue(child)
		}
		return out
	default:
		return value
	}
}

func safeLibrarySettings(doc map[string]json.RawMessage) map[string]any {
	out := map[string]any{}
	for kind, collection := range map[string]string{"printer": "printers", "process": "processes", "filament": "filaments", "calibration": "calibrations"} {
		var records []map[string]json.RawMessage
		json.Unmarshal(doc[collection], &records)
		group := map[string]any{}
		for _, record := range records {
			group[stringValue(record["id"])] = safeProfile(record)
		}
		out[kind] = group
	}
	return out
}

func (m *mcpService) settingsLibrary() (*libraryEnvelope, error) {
	l := m.server.library
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readSettingsSnapshot()
}

func requireSettingsRevision(expected, current string) error {
	if expected == "" {
		return errors.New("expected_revision from the read is required; current revision=" + current)
	}
	if expected != current {
		return errors.New("revision_conflict: read settings again; current revision=" + current)
	}
	return nil
}

func (m *mcpService) getProfile(_ context.Context, _ *mcp.CallToolRequest, in mcpProfileInput) (map[string]any, error) {
	e, err := m.settingsLibrary()
	if err != nil {
		return nil, err
	}
	r, err := libraryProfile(e.Document, in.Kind, in.ProfileID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"profile": safeProfile(r), "revision": mcpHash(e.Document), "library_revision": resolve.LibraryRevision(e.Document), "profile_revision": r["revision"]}
	if history := e.History[in.Kind+"/"+in.ProfileID]; len(history) > 0 {
		out["revision_id"] = history[len(history)-1].ID
	}
	return out, nil
}

func (m *mcpService) patchProfile(record map[string]json.RawMessage, in mcpProfileWriteInput) error {
	section := in.Section
	if section == "" {
		section = map[string]string{"process": "values", "printer": "machine", "filament": "settings"}[in.Kind]
	}
	changes := map[string]json.RawMessage{}
	for k, v := range in.Changes {
		changes[k] = must(json.Marshal(v))
	}
	if len(changes) > 0 {
		if m.server.cfg.Options == nil {
			return errors.New("engine option metadata is unavailable; no settings were changed")
		}
		if err := validateSettingChanges(m.server.cfg.Options.plain, m.server.catalog, in.Kind, stringValue(record["model"]), section, changes); err != nil {
			return err
		}
	}
	if len(changes) == 0 && in.Section != "" {
		return errors.New("section requires option changes")
	}
	if len(changes) > 0 {
		values := map[string]json.RawMessage{}
		if raw := record[section]; len(raw) > 0 && json.Unmarshal(raw, &values) != nil {
			return errors.New("saved settings are unreadable")
		}
		if values == nil {
			values = map[string]json.RawMessage{}
		}
		for k, v := range changes {
			if string(v) == "null" {
				delete(values, k)
			} else {
				values[k] = v
			}
		}
		record[section] = must(json.Marshal(values))
	}
	return nil
}

func (m *mcpService) persistProfile(e *libraryEnvelope, kind string, before, after map[string]json.RawMessage, dryRun bool, calibrations ...json.RawMessage) (map[string]any, error) {
	if archived := after["archived"]; len(archived) > 0 && string(archived) != "null" {
		return nil, errors.New("archived snapshots are read-only through MCP; use the UI to archive or restore a profile")
	}
	if kind == "printer" {
		var slots []resolve.Slot
		if err := json.Unmarshal(after["slots"], &slots); err != nil {
			return nil, errors.New("saved slots are unreadable")
		}
		var previous []resolve.Slot
		json.Unmarshal(before["slots"], &previous)
		for _, slot := range slots {
			unchanged := slices.ContainsFunc(previous, func(old resolve.Slot) bool { return old.ID == slot.ID && old.Colour == slot.Colour })
			if !unchanged && slot.Colour != "" && !settingsSlotColour.MatchString(slot.Colour) {
				return nil, errors.New("slot colour must be #RRGGBB or empty")
			}
		}
	}
	doc := maps.Clone(e.Document)
	if len(calibrations) > 0 {
		doc["calibrations"] = calibrations[0]
	}
	create := before == nil
	if err := replaceLibraryProfile(doc, kind, after, create); err != nil {
		return nil, err
	}
	out, err := m.server.writeLibrarySettings(librarySettingsWrite{ExpectedRevision: resolve.LibraryRevision(e.Document), ExpectedETag: mcpHash(e.Document), OperationID: newID("mcp"), Document: doc, DryRun: dryRun, ForceRevisionFor: kind + "/" + stringValue(after["id"])})
	if err != nil {
		return nil, err
	}
	next, ok := out["document"].(map[string]json.RawMessage)
	if !ok {
		return nil, errors.New("invalid library commit result")
	}
	saved, err := libraryProfile(next, kind, stringValue(after["id"]))
	if err != nil {
		return nil, err
	}
	revision, libraryRevision := mcpHash(next), resolve.LibraryRevision(next)
	if dryRun {
		revision, libraryRevision = mcpHash(e.Document), resolve.LibraryRevision(e.Document)
	}
	return map[string]any{"profile": safeProfile(saved), "profile_id": stringValue(saved["id"]), "revision": revision, "library_revision": libraryRevision, "diff": settingsDiff(safeLibrarySettings(e.Document), safeLibrarySettings(next)), "dry_run": dryRun}, nil
}

func (m *mcpService) profileWrite(in mcpProfileWriteInput, action string) (map[string]any, error) {
	e, err := m.settingsLibrary()
	if err != nil {
		return nil, err
	}
	if err := requireSettingsRevision(in.ExpectedRevision, mcpHash(e.Document)); err != nil {
		return nil, err
	}
	before, err := libraryProfile(e.Document, in.Kind, in.ProfileID)
	if err != nil {
		return nil, err
	}
	after := maps.Clone(before)
	var copiedCalibrations json.RawMessage
	if action == "rename" && (len(in.Changes) > 0 || in.Section != "") {
		return nil, errors.New("rename_profile only accepts a new name")
	}
	if action == "update" && in.Name != "" {
		return nil, errors.New("use rename_profile to change a name")
	}
	if action != "update" {
		if err := resolve.ValidProfileName(in.Name); err != nil {
			return nil, err
		}
		after["name"] = must(json.Marshal(in.Name))
	}
	if action == "copy" {
		after = safeProfile(after)
		id := newID("profile")
		after["id"], after["revision"] = must(json.Marshal(id)), json.RawMessage(`0`)
		delete(after, "edited")
		delete(after, "archived")
		delete(after, "aliases")
		if in.Kind == "printer" {
			var slots []resolve.Slot
			json.Unmarshal(after["slots"], &slots)
			ids := map[string]string{}
			for i := range slots {
				old := slots[i].ID
				slots[i].ID = newID("slot")
				ids[old] = slots[i].ID
			}
			after["slots"] = must(json.Marshal(slots))
			var mixes []resolve.Mix
			json.Unmarshal(after["mixes"], &mixes)
			for i := range mixes {
				mixes[i].ID = newID("mix")
				for j, k := range mixes[i].Components {
					mixes[i].Components[j] = ids[k]
				}
			}
			if len(mixes) > 0 {
				after["mixes"] = must(json.Marshal(mixes))
			}
		}
		var calibrations []map[string]json.RawMessage
		json.Unmarshal(e.Document["calibrations"], &calibrations)
		field := map[string]string{"printer": "printer", "filament": "filament"}[in.Kind]
		if field != "" {
			for _, cal := range slices.Clone(calibrations) {
				if stringValue(cal[field]) == in.ProfileID {
					copy := maps.Clone(cal)
					copy["id"], copy[field], copy["revision"] = must(json.Marshal(newID("calibration"))), must(json.Marshal(id)), json.RawMessage(`0`)
					calibrations = append(calibrations, copy)
				}
			}
			copiedCalibrations = must(json.Marshal(calibrations))
		}
	}
	if action != "rename" {
		if err := m.patchProfile(after, in); err != nil {
			return nil, err
		}
	}
	if action == "copy" {
		for _, section := range []string{"values", "settings", "machine", "printOverrides"} {
			var values map[string]any
			json.Unmarshal(after[section], &values)
			if len(values) > 0 {
				if err := m.patchProfile(after, mcpProfileWriteInput{Kind: in.Kind, Section: section, Changes: values}); err != nil {
					return nil, err
				}
			}
		}
		if copiedCalibrations != nil {
			return m.persistProfile(e, in.Kind, nil, after, in.DryRun, copiedCalibrations)
		}
		return m.persistProfile(e, in.Kind, nil, after, in.DryRun)
	}
	return m.persistProfile(e, in.Kind, before, after, in.DryRun)
}

func (m *mcpService) updateProfile(_ context.Context, _ *mcp.CallToolRequest, in mcpProfileWriteInput) (map[string]any, error) {
	return m.profileWrite(in, "update")
}
func (m *mcpService) saveProfileAs(_ context.Context, _ *mcp.CallToolRequest, in mcpProfileWriteInput) (map[string]any, error) {
	return m.profileWrite(in, "copy")
}
func (m *mcpService) renameProfile(_ context.Context, _ *mcp.CallToolRequest, in mcpProfileWriteInput) (map[string]any, error) {
	return m.profileWrite(in, "rename")
}

func printerDefaults(record map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range []string{"id", "process", "slots"} {
		if raw, ok := record[k]; ok {
			out[k] = raw
		}
	}
	return out
}

func (m *mcpService) getPrinterDefaults(ctx context.Context, call *mcp.CallToolRequest, in mcpPrinterDefaultsInput) (map[string]any, error) {
	out, err := m.getProfile(ctx, call, mcpProfileInput{Kind: "printer", ProfileID: in.PrinterID})
	if err != nil {
		return nil, err
	}
	profile := out["profile"].(map[string]json.RawMessage)
	delete(out, "profile")
	out["defaults"] = printerDefaults(profile)
	return out, nil
}

func (m *mcpService) updatePrinterDefaults(_ context.Context, _ *mcp.CallToolRequest, in mcpPrinterDefaultsWriteInput) (map[string]any, error) {
	e, err := m.settingsLibrary()
	if err != nil {
		return nil, err
	}
	if err := requireSettingsRevision(in.ExpectedRevision, mcpHash(e.Document)); err != nil {
		return nil, err
	}
	before, err := libraryProfile(e.Document, "printer", in.PrinterID)
	if err != nil {
		return nil, err
	}
	after := maps.Clone(before)
	if in.ProcessID != nil {
		if *in.ProcessID != "" {
			if _, err := libraryProfile(e.Document, "process", *in.ProcessID); err != nil {
				return nil, err
			}
		}
		after["process"] = must(json.Marshal(*in.ProcessID))
	}
	var slots []resolve.Slot
	if json.Unmarshal(after["slots"], &slots) != nil {
		return nil, errors.New("saved slots are unreadable")
	}
	seen := map[string]bool{}
	for _, patch := range in.Slots {
		if seen[patch.SlotID] {
			return nil, errors.New("duplicate slot_id")
		}
		seen[patch.SlotID] = true
		i := slices.IndexFunc(slots, func(slot resolve.Slot) bool { return slot.ID == patch.SlotID })
		if i < 0 {
			return nil, errors.New("unknown stable slot_id")
		}
		if patch.FilamentID != nil {
			if *patch.FilamentID != "" {
				if _, err := libraryProfile(e.Document, "filament", *patch.FilamentID); err != nil {
					return nil, err
				}
			}
			slots[i].Filament = *patch.FilamentID
		}
		if patch.Colour != nil {
			if *patch.Colour != "" && !settingsSlotColour.MatchString(*patch.Colour) {
				return nil, errors.New("slot colour must be #RRGGBB or empty")
			}
			slots[i].Colour = strings.ToUpper(*patch.Colour)
		}
		if patch.Tray != nil {
			slots[i].Tray = *patch.Tray
		}
	}
	after["slots"] = must(json.Marshal(slots))
	return m.persistProfile(e, "printer", before, after, in.DryRun)
}

func preferenceDefaults() map[string]json.RawMessage {
	return map[string]json.RawMessage{"orca.printer": json.RawMessage(`""`), "orca.vendor": json.RawMessage(`""`), "orca.web.afterSlice": json.RawMessage(`"export"`), "orca.web.flowCalibration": json.RawMessage(`"auto"`), openDeviceAfterPrintKey: json.RawMessage(`true`), openDeviceDelayKey: json.RawMessage(`5`)}
}

func safePreferences(raw json.RawMessage) map[string]json.RawMessage {
	out := preferenceDefaults()
	var saved map[string]json.RawMessage
	json.Unmarshal(raw, &saved)
	for k := range out {
		if v, ok := saved[k]; ok {
			out[k] = must(json.Marshal(safeSettingValue(settingsJSON(v))))
		}
	}
	return out
}

func (m *mcpService) settingsPreferences() (workspaceValue, error) {
	st := m.server.workspace
	st.mu.Lock()
	defer st.mu.Unlock()
	d, err := st.readSnapshot()
	if err != nil {
		return workspaceValue{}, errors.New("saved preferences are unavailable")
	}
	return d.Values["preferences"], nil
}

func (m *mcpService) getPreferences(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (map[string]any, error) {
	v, err := m.settingsPreferences()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"preferences": safePreferences(normalizePreferences(v.Document)), "revision": preferencesETag(v), "preferences_revision": v.Revision}
	if len(v.History) > 0 {
		out["revision_id"] = v.History[len(v.History)-1].ID
	}
	return out, nil
}

func (m *mcpService) validatePreferences(changes map[string]json.RawMessage, lib *libraryEnvelope) error {
	for key, raw := range changes {
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return errors.New("invalid preference value")
		}
		switch key {
		case "orca.web.afterSlice":
			if v != "export" && v != "send" {
				return errors.New(key + " must be export or send")
			}
		case "orca.web.flowCalibration":
			if v != "off" && v != "auto" && v != "on" {
				return errors.New(key + " must be off, auto, or on")
			}
		case openDeviceAfterPrintKey:
			if _, ok := v.(bool); !ok {
				return errors.New(key + " must be a boolean")
			}
		case openDeviceDelayKey:
			n, ok := v.(float64)
			if !ok || n < 0 || n > 60 || n != float64(int(n)) {
				return errors.New(key + " must be an integer between 0 and 60")
			}
		case "orca.printer":
			id, ok := v.(string)
			if !ok {
				return errors.New(key + " must be a saved printer ID")
			}
			if id != "" {
				if _, err := libraryProfile(lib.Document, "printer", id); err != nil {
					return err
				}
			}
		case "orca.vendor":
			return errors.New("orca.vendor is derived from orca.printer")
		default:
			return fmt.Errorf("unknown or blocked preference key %q", key)
		}
	}
	return nil
}

func (m *mcpService) persistPreferences(v workspaceValue, changes map[string]json.RawMessage, expected string, dryRun bool) (map[string]any, error) {
	if err := requireSettingsRevision(expected, preferencesETag(v)); err != nil {
		return nil, err
	}
	l := m.server.library
	l.mu.Lock()
	defer l.mu.Unlock()
	lib, err := l.readSettingsSnapshot()
	if err != nil {
		return nil, err
	}
	if err := m.validatePreferences(changes, lib); err != nil {
		return nil, err
	}
	doc := map[string]json.RawMessage{}
	if len(v.Document) > 0 {
		if err := json.Unmarshal(v.Document, &doc); err != nil {
			return nil, errors.New("saved preferences are unreadable")
		}
	}
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}
	for k, raw := range changes {
		doc[k] = raw
	}
	if raw, ok := changes["orca.printer"]; ok {
		vendor := ""
		if id := stringValue(raw); id != "" {
			p, _ := libraryProfile(lib.Document, "printer", id)
			vendor = stringValue(p["vendor"])
		}
		doc["orca.vendor"] = must(json.Marshal(vendor))
	}
	out, err := m.server.workspace.writePreferences(preferencesWrite{OperationID: newID("mcp"), ExpectedRevision: v.Revision, ExpectedETag: expected, Document: must(json.Marshal(doc)), DryRun: dryRun})
	if err != nil {
		return nil, err
	}
	raw, ok := out["document"].(json.RawMessage)
	if !ok {
		return nil, errors.New("invalid preference commit result")
	}
	revision, ok := out["revision"].(uint64)
	if !ok {
		return nil, errors.New("invalid preference revision")
	}
	after := safePreferences(raw)
	etag := preferencesETag(workspaceValue{Revision: revision, Document: raw})
	if dryRun {
		etag, revision = preferencesETag(v), v.Revision
	}
	return map[string]any{"preferences": after, "revision": etag, "preferences_revision": revision, "diff": settingsDiff(safePreferences(normalizePreferences(v.Document)), after), "dry_run": dryRun}, nil
}

func (m *mcpService) updatePreferences(_ context.Context, _ *mcp.CallToolRequest, in mcpPreferencesWriteInput) (map[string]any, error) {
	v, err := m.settingsPreferences()
	if err != nil {
		return nil, err
	}
	changes := map[string]json.RawMessage{}
	for k, value := range in.Changes {
		changes[k] = must(json.Marshal(value))
	}
	return m.persistPreferences(v, changes, in.ExpectedRevision, in.DryRun)
}

func (m *mcpService) listSettingsHistory(_ context.Context, _ *mcp.CallToolRequest, in mcpSettingsHistoryInput) (map[string]any, error) {
	var history []settingsRevision
	var revision string
	if in.Kind == "preferences" {
		v, err := m.settingsPreferences()
		if err != nil {
			return nil, err
		}
		history, revision = v.History, preferencesETag(v)
	} else {
		e, err := m.settingsLibrary()
		if err != nil {
			return nil, err
		}
		if _, err := libraryProfile(e.Document, in.Kind, in.ProfileID); err != nil {
			return nil, err
		}
		history, revision = e.History[in.Kind+"/"+in.ProfileID], mcpHash(e.Document)
	}
	rows := []map[string]any{}
	var before any
	for _, entry := range history {
		var doc any
		if in.Kind == "preferences" {
			doc = safePreferences(entry.Document)
		} else {
			var record map[string]json.RawMessage
			json.Unmarshal(entry.Document, &record)
			if record != nil {
				doc = safeProfile(record)
			}
		}
		rows = append(rows, map[string]any{"id": entry.ID, "revision": entry.Revision, "at": entry.At, "snapshot": doc, "diff": settingsDiff(before, doc)})
		before = doc
	}
	return map[string]any{"revision": revision, "history": rows}, nil
}

func (m *mcpService) revertSettings(_ context.Context, _ *mcp.CallToolRequest, in mcpSettingsRevertInput) (map[string]any, error) {
	if in.Kind == "preferences" {
		v, err := m.settingsPreferences()
		if err != nil {
			return nil, err
		}
		for _, entry := range v.History {
			if entry.ID == in.RevisionID {
				changes := safePreferences(entry.Document)
				delete(changes, "orca.vendor")
				return m.persistPreferences(v, changes, in.ExpectedRevision, in.DryRun)
			}
		}
		return nil, errors.New("retained revision_id not found")
	}
	e, err := m.settingsLibrary()
	if err != nil {
		return nil, err
	}
	if err := requireSettingsRevision(in.ExpectedRevision, mcpHash(e.Document)); err != nil {
		return nil, err
	}
	before, err := libraryProfile(e.Document, in.Kind, in.ProfileID)
	if err != nil {
		return nil, err
	}
	for _, entry := range e.History[in.Kind+"/"+in.ProfileID] {
		if entry.ID == in.RevisionID {
			var after map[string]json.RawMessage
			if json.Unmarshal(entry.Document, &after) != nil || after == nil {
				return nil, errors.New("cannot restore an absent profile; saved copies retain their original revision for later restoration")
			}
			if stringValue(after["id"]) != in.ProfileID {
				return nil, errors.New("history target mismatch")
			}
			for _, section := range []string{"values", "settings", "machine", "printOverrides"} {
				var old, next map[string]json.RawMessage
				json.Unmarshal(before[section], &old)
				json.Unmarshal(after[section], &next)
				changes, removals := map[string]any{}, map[string]any{}
				for k, raw := range next {
					if string(old[k]) != string(raw) {
						changes[k] = settingsJSON(raw)
					}
				}
				for k := range old {
					if _, ok := next[k]; !ok {
						removals[k] = nil
					}
				}
				if len(changes) > 0 {
					probe := maps.Clone(after)
					if err := m.patchProfile(probe, mcpProfileWriteInput{Kind: in.Kind, Section: section, Changes: changes}); err != nil {
						return nil, err
					}
				}
				if len(removals) > 0 {
					probe := maps.Clone(before)
					if err := m.patchProfile(probe, mcpProfileWriteInput{Kind: in.Kind, Section: section, Changes: removals}); err != nil {
						return nil, err
					}
				}
			}
			return m.persistProfile(e, in.Kind, before, after, in.DryRun)
		}
	}
	return nil, errors.New("retained revision_id not found")
}
