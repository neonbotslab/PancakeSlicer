package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

type sendMaterial struct {
	Type  string `json:"type,omitempty"`
	Color string `json:"color,omitempty"`
}

type sendSlot struct {
	Filament   int           `json:"filament"`
	Slot       string        `json:"slot"`
	Tray       string        `json:"tray,omitempty"`
	Used       bool          `json:"used"`
	State      string        `json:"state,omitempty"`
	Expected   sendMaterial  `json:"expected"`
	Reported   *sendMaterial `json:"reported,omitempty"`
	Remembered *sendMaterial `json:"remembered,omitempty"`
	Spool      string        `json:"spool,omitempty"`
	Proof      string        `json:"proof"`
	tray       *AMSTray
	binding    sendSlotBinding
}

type sendPlan struct {
	Slots    []sendSlot `json:"slots"`
	targetID string
	dual     bool
}

type sendPlanInput struct {
	Source      *resolve.Request
	TargetID    string
	Sliced      *BambuJobMapping
	TargetLive  PrinterStatus
	filamentIDs sendFilamentAliases
}

type sendSlotBinding struct {
	sourceComponent string
	nozzle          int
	remembered      *savedTrayMaterial
}

type savedTrayMaterial struct {
	SlotID      string
	FilamentID  string
	Revision    uint64
	RevisionSet bool
	Nozzle      int
	Material    sendMaterial
}

type sendTargetTray struct {
	slot      *resolve.Slot
	nozzle    int
	routed    bool
	report    *AMSTray
	ambiguous bool
}

func (s *Server) printerSendPlan(lib *resolve.Library, in sendPlanInput) (sendPlan, error) {
	if len(in.TargetLive.Trays) > 0 && isAutomaticSendMode(in.Sliced.Mode) {
		in.filamentIDs = loadSendFilamentAliases(s.cfg.Resources)
	}
	return sendPlanOf(s.catalog, lib, in)
}

func sendPlanOf(catalog *resolve.Catalog, lib *resolve.Library, in sendPlanInput) (sendPlan, error) {
	req := in.Source
	if req == nil {
		return sendPlan{}, fmt.Errorf("This job names no library printer. Slice again.")
	}
	source, target := lib.Printers[req.Printer], lib.Printers[in.TargetID]
	if source == nil || len(req.Slots) != len(req.Filaments) {
		return sendPlan{}, fmt.Errorf("This job's printer or slots are no longer in your library. Slice again.")
	}
	if target == nil {
		return sendPlan{}, fmt.Errorf("The selected printer is no longer in your library. Choose a connected printer.")
	}
	plan := sendPlan{
		Slots:    make([]sendSlot, 0, len(req.Slots)),
		targetID: in.TargetID,
		dual:     len(source.Nozzles) > 1 || len(target.Nozzles) > 1 || reportedDualNozzles(in.TargetLive),
	}
	automatic := automaticSendMode(source, req, in.Sliced.Mode)
	materials := make(map[int]BambuSlicedMaterial, len(in.Sliced.Materials))
	for _, material := range in.Sliced.Materials {
		materials[material.Slot] = material
	}
	sourceTrays, bindings, usedTrays := map[string]string{}, map[string]string{}, map[string]bool{}
	for i, id := range req.Slots {
		at := source.SlotIndex(id)
		mix := false
		if m := source.MixByID(id); at < 0 && m != nil && len(m.Components) > 0 {
			at, mix = source.SlotIndex(m.Components[0]), true
		}
		if at < 0 {
			return sendPlan{}, fmt.Errorf("Filament %d's slot is no longer on %s. Slice again.", i+1, source.Name)
		}
		slot, material := source.Slots[at], materials[i+1]
		nozzle := -1
		if i < len(in.Sliced.Filaments) {
			nozzle = in.Sliced.Filaments[i] - 1
		}
		row := sendSlot{
			Filament: i + 1,
			Slot:     id,
			Tray:     slot.Tray,
			Used:     material.Object || material.Support,
			Expected: sendMaterial{material.Type, material.Color},
			binding:  sendSlotBinding{sourceComponent: slot.ID, nozzle: nozzle},
		}
		plan.Slots = append(plan.Slots, row)
		if !row.Used {
			continue
		}
		key := sendTrayKey(slot.Tray, plan.dual)
		if _, ok := resolve.ParseTray(key); ok {
			if previous, held := sourceTrays[key]; held && previous != slot.ID {
				return sendPlan{}, fmt.Errorf("Two used slots on %s identify the same physical tray %q. Fix their tray assignments and slice again.", source.Name, key)
			}
			sourceTrays[key] = slot.ID
		}
		if !automatic || mix {
			bindings[slot.ID], usedTrays[key] = key, true
		}
	}

	targetTrays := targetSendTrays(target, in.TargetLive.Trays, plan.dual)
	for i := range plan.Slots {
		row := &plan.Slots[i]
		if !row.Used {
			continue
		}
		profileID := sendProfileID(lib, req.Filaments[i])
		component := row.binding.sourceComponent
		key, bound := bindings[component]
		if !bound {
			key = sendTrayKey(row.Tray, plan.dual)
			if pick := automaticSendTray(lib, targetTrays, usedTrays, key, profileID, materials[row.Filament], row.binding.nozzle, in.filamentIDs); pick != "" {
				key = pick
			}
			bindings[component], usedTrays[key] = key, true
		}
		row.Tray = key
		if _, ok := resolve.ParseTray(key); !ok {
			row.State = "unassigned"
			continue
		}
		if in.TargetLive.Trays == nil {
			row.State = "assigned"
			continue
		}
		choice := targetTrays[key]
		if choice.ambiguous {
			row.State = "ambiguous"
			continue
		}
		row.State = "mismatch"
		if choice.report == nil {
			continue
		}
		report := *choice.report
		if report.Extruder != nil {
			report.Extruder = ptr(*report.Extruder)
		}
		row.tray = &report
		row.Spool = report.Spool
		row.Reported = &sendMaterial{report.Type, report.Color}
		if sameSendMaterialType(report.Type, row.Expected.Type) {
			row.State = "confirmed"
		} else if remembered := choice.rememberedMaterial(catalog, lib, target, &report, *row, plan.dual); remembered != nil && (in.TargetID != req.Printer || remembered.FilamentID == profileID) {
			row.binding.remembered = remembered
			row.Remembered = &remembered.Material
			row.State = "remembered"
		}
	}
	for i := range plan.Slots {
		plan.Slots[i].Proof = plan.slotProof(plan.Slots[i])
	}
	return plan, nil
}

func automaticSendMode(source *resolve.Printer, req *resolve.Request, slicedMode string) bool {
	if source.Vendor != "BBL" || source.Swap != "ams" || len(source.Nozzles) < 2 || !isAutomaticSendMode(slicedMode) {
		return false
	}
	intent, ok := req.Project.Process["filament_map_mode"]
	if !ok {
		intent = req.Project.Machine["filament_map_mode"]
	}
	return resolve.FirstConfigText(intent) == slicedMode
}

func isAutomaticSendMode(mode string) bool {
	return mode == "Auto For Match" || mode == "Auto For Flush"
}

func targetSendTrays(target *resolve.Printer, reports []AMSTray, dual bool) map[string]sendTargetTray {
	trays := make(map[string]sendTargetTray, len(reports))
	if target != nil {
		for i := range target.Slots {
			slot := &target.Slots[i]
			key := sendTrayKey(slot.Tray, dual)
			if _, ok := resolve.ParseTray(key); !ok {
				continue
			}
			choice := trays[key]
			choice.ambiguous = choice.slot != nil
			choice.slot = slot
			choice.nozzle, choice.routed = target.SlotExtruder(i)
			trays[key] = choice
		}
	}
	for i := range reports {
		report := &reports[i]
		key := sendTrayKey(slotID(*report), dual)
		if _, ok := resolve.ParseTray(key); !ok {
			continue
		}
		choice := trays[key]
		choice.ambiguous = choice.ambiguous || choice.report != nil && !sameSendTray(*choice.report, *report)
		choice.report = report
		trays[key] = choice
	}
	return trays
}

func (choice sendTargetTray) rememberedMaterial(catalog *resolve.Catalog, lib *resolve.Library, target *resolve.Printer, report *AMSTray, row sendSlot, dual bool) *savedTrayMaterial {
	if catalog == nil || target == nil || !target.Live() || choice.slot == nil || choice.ambiguous || !choice.routed ||
		report == nil || report.Kind != "external" || strings.TrimSpace(report.Type) != "" ||
		choice.nozzle != row.binding.nozzle || !sendTrayFeedsNozzle(report, choice.nozzle, dual) {
		return nil
	}
	filament := lib.Filament(choice.slot.Filament)
	if filament == nil || !filament.Live() {
		return nil
	}
	values, err := catalog.Profile(lib, target.ID, resolve.Ask{Type: "filament", ID: filament.ID, Extruder: &choice.nozzle})
	if err != nil || !sameSendMaterialType(resolve.FirstConfigText(values.Values["filament_type"]), row.Expected.Type) {
		return nil
	}
	remembered := &savedTrayMaterial{
		SlotID:     choice.slot.ID,
		FilamentID: filament.ID,
		Nozzle:     choice.nozzle,
		Material:   sendMaterial{row.Expected.Type, choice.slot.Colour},
	}
	if filament.Revision != nil {
		remembered.Revision, remembered.RevisionSet = *filament.Revision, true
	}
	return remembered
}

func sendTrayFeedsNozzle(tray *AMSTray, nozzle int, dual bool) bool {
	if tray.Extruder == nil {
		return !dual && nozzle == 0
	}
	return *tray.Extruder == nozzle
}

func sameSendMaterialType(reported, expected string) bool {
	reported = strings.TrimSpace(reported)
	return reported != "" && strings.EqualFold(reported, strings.TrimSpace(expected))
}

func (plan sendPlan) mapping(override []string) ([]int, []bambuTrayAddress, *sendSlot) {
	v0, v1 := make([]int, len(plan.Slots)), make([]bambuTrayAddress, len(plan.Slots))
	for i, row := range plan.Slots {
		if !row.Used {
			v0[i], v1[i] = -1, bambuTrayAddress{255, 255}
			continue
		}
		overridden := row.State == "mismatch" && row.tray != nil && strings.TrimSpace(row.tray.Type) != "" && containsString(override, row.Slot)
		if row.tray == nil || row.State != "confirmed" && row.State != "remembered" && !overridden {
			return nil, nil, &plan.Slots[i]
		}
		t := row.tray
		switch {
		case t.Kind == "external":
			v0[i], v1[i] = -1, bambuTrayAddress{t.ID, 0}
		case t.AMS >= 128:
			v0[i], v1[i] = t.ID, bambuTrayAddress{t.AMS, 0}
		default:
			v0[i], v1[i] = t.ID, bambuTrayAddress{t.AMS, t.Slot}
		}
	}
	return v0, v1, nil
}

func (plan sendPlan) checkLiveTrays(trays []AMSTray) error {
	current := targetSendTrays(nil, trays, plan.dual)
	for _, row := range plan.Slots {
		if !row.Used {
			continue
		}
		want := row.tray
		choice := current[row.Tray]
		if want == nil || choice.report == nil || choice.ambiguous || !sameSendTray(*choice.report, *want) {
			return fmt.Errorf("Filament %d's tray changed during the upload. Check the tray and send again.", row.Filament)
		}
	}
	return nil
}

func sendTrayKey(key string, dual bool) string {
	if dual && key == "ext" {
		return "ext.L"
	}
	return key
}

func (plan sendPlan) checkRemembered(catalog *resolve.Catalog, lib *resolve.Library) error {
	target := lib.Printers[plan.targetID]
	current := targetSendTrays(target, nil, plan.dual)
	for _, row := range plan.Slots {
		want := row.binding.remembered
		if want == nil {
			continue
		}
		material := current[row.Tray].rememberedMaterial(catalog, lib, target, row.tray, row, plan.dual)
		if material == nil || *material != *want {
			return fmt.Errorf("Filament %d's saved external filament changed during the upload. Check the slot and send again.", row.Filament)
		}
	}
	return nil
}

type sendTrayIdentity struct {
	ID, AMS, Slot, Extruder              int
	Kind, Type, Color, FilamentID, Spool string
}

func (tray AMSTray) sendIdentity() sendTrayIdentity {
	return sendTrayIdentity{
		ID: tray.ID, AMS: tray.AMS, Slot: tray.Slot, Extruder: trayExtruder(tray),
		Kind: tray.Kind, Type: strings.ToUpper(strings.TrimSpace(tray.Type)),
		Color: strings.ToUpper(tray.Color), FilamentID: tray.FilamentID, Spool: tray.Spool,
	}
}

func sameSendTray(a, b AMSTray) bool {
	return a.sendIdentity() == b.sendIdentity()
}

func (plan sendPlan) slotProof(row sendSlot) string {
	proof := struct {
		TargetID, Slot, Tray, State, SourceComponent string
		Dual, Used                                   bool
		Filament, Nozzle                             int
		Expected                                     sendMaterial
		Reported                                     *sendTrayIdentity
		Remembered                                   *savedTrayMaterial
	}{
		TargetID: plan.targetID, Dual: plan.dual, Slot: row.Slot, Tray: row.Tray, State: row.State, Used: row.Used,
		Filament: row.Filament, Nozzle: row.binding.nozzle, SourceComponent: row.binding.sourceComponent,
		Expected: row.Expected, Remembered: row.binding.remembered,
	}
	if row.tray != nil {
		identity := row.tray.sendIdentity()
		proof.Reported = &identity
	}
	data, _ := json.Marshal(proof)
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func (plan sendPlan) matchesProofs(expected []string) bool {
	if expected == nil || len(expected) != len(plan.Slots) {
		return false
	}
	for i, row := range plan.Slots {
		if row.Proof != expected[i] {
			return false
		}
	}
	return true
}

func trayExtruder(tray AMSTray) int {
	if tray.Extruder == nil {
		return -1
	}
	return *tray.Extruder
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
