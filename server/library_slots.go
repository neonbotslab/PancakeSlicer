package main

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

func layoutTraylessSlots(doc map[string]json.RawMessage, catalog *resolve.Catalog, readVotes func() (map[string][]map[int]int, error)) ([]string, error) {
	var votes map[string][]map[int]int
	var printers []map[string]json.RawMessage
	if err := json.Unmarshal(doc["printers"], &printers); err != nil {
		return nil, fmt.Errorf("printers: %w", err)
	}
	var records []string
	note := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		records = append(records, line)
		log.Printf("profile library: slot layout: %s", line)
	}
	for _, p := range printers {
		var nozzles []resolve.Nozzle
		var slots []map[string]json.RawMessage
		json.Unmarshal(p["nozzles"], &nozzles)
		json.Unmarshal(p["slots"], &slots)
		feed, ok := resolve.PrinterFeed(stringValue(p["model"]))
		if len(nozzles) < 2 || stringValue(p["swap"]) == "toolheads" || !ok || feed.External != "perNozzle" || len(slots) == 0 {
			continue
		}
		var units0 []resolve.Unit
		json.Unmarshal(p["units"], &units0)
		trayNozzle := func(units []resolve.Unit, tray string) (int, bool) {
			if tray == "ext" {
				return 0, false
			}
			nozzle, ok := resolve.FeedTrayNozzle(feed, units, tray)
			return nozzle, ok && nozzle < len(nozzles)
		}
		unrouted := false
		for _, s := range slots {
			_, ok := trayNozzle(units0, stringValue(s["tray"]))
			unrouted = unrouted || !ok
		}
		if !unrouted {
			continue
		}
		if votes == nil {
			v, err := readVotes()
			if err != nil {
				return nil, err
			}
			votes = v
			if votes == nil {
				votes = map[string][]map[int]int{}
			}
		}
		name := fmt.Sprintf("%s (%s)", stringValue(p["name"]), stringValue(p["id"]))
		master := 0
		if raw, ok := catalogMachineValue(catalog, p, "master_extruder_id"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(resolve.FirstConfigText(raw))); err == nil && n >= 1 && n <= len(nozzles) {
				master = n - 1
			}
		}
		byDefault := fmt.Sprintf("nozzle %d, the model's master extruder", master+1)
		slotNozzle := func(i int) (int, string) {
			id := stringValue(slots[i]["id"])
			if at := strings.LastIndex(id, ":"); at > len("slot:") {
				if e, ok := trayNozzle(units0, id[at+1:]); ok {
					return e, "the tray its id names, " + id[at+1:]
				}
			}
			tally := map[int]int{}
			for _, project := range votes[stringValue(p["id"])] {
				if e, ok := project[i]; ok && e < len(nozzles) {
					tally[e]++
				}
			}
			e := master
			for n, c := range tally {
				if c > tally[e] || c == tally[e] && n < e {
					e = n
				}
			}
			if len(tally) > 1 {
				note("%s slot %d: projects disagree on its nozzle (%v by nozzle); nozzle %d, the most chosen", name, i+1, tally, e+1)
			}
			if len(tally) > 0 {
				return e, "its projects' nozzle"
			}
			return e, byDefault
		}
		units := append([]resolve.Unit{}, units0...)
		unitIDs := map[int]bool{}
		standardCount := 0
		for i, unit := range units {
			unitIDs[unit.EffectiveID(i)] = true
			if unit.Kind == "" {
				standardCount++
			}
		}
		onUnit := map[int][]int{}
		for i, s := range slots {
			if ref, ok := resolve.ParseTray(stringValue(s["tray"])); ok && !ref.External {
				unitIDs[ref.Unit] = true
				onUnit[ref.Unit] = append(onUnit[ref.Unit], i)
			}
		}
		legacyUnits := !slices.ContainsFunc(units0, func(u resolve.Unit) bool { return u.ID != nil || u.Kind != "" })
		for u := len(units); legacyUnits && u < feed.MaxUnits && len(onUnit[u]) > 0; u++ {
			tally, from := map[int]int{}, byDefault
			for _, i := range onUnit[u] {
				if e, why := slotNozzle(i); why != byDefault {
					tally[e]++
					from = why
				}
			}
			e := master
			for n, c := range tally {
				if c > tally[e] || c == tally[e] && n < e {
					e = n
				}
			}
			units = append(units, resolve.Unit{Nozzle: e})
			standardCount++
			note("%s: AMS unit %d added on nozzle %d (%s), for the trays its slots hold", name, u+1, e+1, from)
		}
		kept := len(onUnit) > 0 || !legacyUnits
		for _, s := range slots {
			_, ok := trayNozzle(units, stringValue(s["tray"]))
			kept = kept || ok
		}
		if !kept {
			want, source := make([]int, len(slots)), make([]string, len(slots))
			for i := range slots {
				want[i], source[i] = slotNozzle(i)
			}
			laid, trays, ok := slotLayout(feed, want)
			if !ok {
				for i, s := range slots {
					note("%s slot %d (%s, filament %s): left on no nozzle; it belongs on nozzle %d (%s), and the slots' nozzles %v can't take the model's positions in order (or need more than %d AMS units) without renumbering the slots",
						name, i+1, stringValue(s["id"]), stringValue(s["filament"]), want[i]+1, source[i], oneBased(want), feed.MaxUnits)
				}
				continue
			}
			units = laid
			for i, s := range slots {
				s["tray"] = must(json.Marshal(trays[i]))
				note("%s slot %d (%s, filament %s): tray %s on nozzle %d (%s)", name, i+1, stringValue(s["id"]), stringValue(s["filament"]), trays[i], want[i]+1, source[i])
			}
		} else {
			taken := map[string]bool{}
			for i, s := range slots {
				tray := stringValue(s["tray"])
				if e, ok := trayNozzle(units, tray); ok {
					taken[tray] = true
					note("%s slot %d (%s, filament %s): keeps its tray %s on nozzle %d", name, i+1, stringValue(s["id"]), stringValue(s["filament"]), tray, e+1)
				}
			}
			last := -1
			for i, s := range slots {
				tray := stringValue(s["tray"])
				if _, ok := trayNozzle(units, tray); ok {
					for at, pos := range resolve.FeedPositions(feed, units) {
						if pos.Tray == tray {
							last = at
						}
					}
					continue
				}
				if ref, ok := resolve.ParseTray(tray); ok && !ref.External {
					continue
				}
				e, why := slotNozzle(i)
				free := func() (string, int) {
					for at, pos := range resolve.FeedPositions(feed, units) {
						if at > last && pos.Nozzle == e && !taken[pos.Tray] {
							return pos.Tray, at
						}
					}
					return "", -1
				}
				tray, at := free()
				if tray == "" && standardCount < feed.MaxUnits {
					for id := range feed.MaxUnits {
						if unitIDs[id] {
							continue
						}
						unit := resolve.Unit{Nozzle: e}
						if id != len(units) {
							unit.ID = &id
						}
						units = append(units, unit)
						unitIDs[id] = true
						standardCount++
						tray, at = free()
						break
					}
				}
				if tray == "" {
					note("%s slot %d (%s, filament %s): left on no nozzle; it belongs on nozzle %d (%s) and no free tray follows the slot before it", name, i+1, stringValue(s["id"]), stringValue(s["filament"]), e+1, why)
					continue
				}
				taken[tray], last = true, at
				s["tray"] = must(json.Marshal(tray))
				note("%s slot %d (%s, filament %s): tray %s on nozzle %d (%s)", name, i+1, stringValue(s["id"]), stringValue(s["filament"]), tray, e+1, why)
			}
		}
		p["slots"], p["units"] = must(json.Marshal(slots)), must(json.Marshal(units))
	}
	if len(records) > 0 {
		doc["printers"] = must(json.Marshal(printers))
	}
	return records, nil
}

func slotLayout(feed resolve.Feed, want []int) ([]resolve.Unit, []string, bool) {
	type run struct{ nozzle, n int }
	var runs []run
	for _, e := range want {
		if len(runs) > 0 && runs[len(runs)-1].nozzle == e {
			runs[len(runs)-1].n++
			continue
		}
		for _, r := range runs {
			if r.nozzle == e {
				return nil, nil, false
			}
		}
		runs = append(runs, run{e, 1})
	}
	units := []resolve.Unit{}
	for i, r := range runs {
		k := (max(0, r.n-1) + feed.Slots - 1) / feed.Slots
		if i < len(runs)-1 && (r.n-1)%feed.Slots != 0 {
			return nil, nil, false
		}
		if len(runs) == 1 {
			k = max(1, k)
		}
		for range k {
			units = append(units, resolve.Unit{Nozzle: r.nozzle})
		}
	}
	positions := resolve.FeedPositions(feed, units)
	trays := make([]string, len(want))
	for i, e := range want {
		if i >= len(positions) || positions[i].Nozzle != e {
			return nil, nil, false
		}
		trays[i] = positions[i].Tray
	}
	return units, trays, len(units) <= feed.MaxUnits
}

func slotTrays(slots []map[string]json.RawMessage) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = stringValue(s["tray"])
	}
	return out
}

func oneBased(want []int) []int {
	out := make([]int, len(want))
	for i, e := range want {
		out[i] = e + 1
	}
	return out
}

func projectNozzleVotes(raw []byte) (map[string][]map[int]int, error) {
	if raw == nil {
		return nil, nil
	}
	w, err := decodeWorkspace(raw)
	if err != nil {
		return nil, fmt.Errorf("workspace.json: %w", err)
	}
	type projectDoc struct {
		Printer        string `json:"printer"`
		ProjectPresets struct {
			Machine map[string]json.RawMessage `json:"machine"`
		} `json:"projectPresets"`
		Dirty struct {
			Process map[string]json.RawMessage `json:"process"`
		} `json:"dirty"`
		Plates []struct {
			Settings map[string]json.RawMessage `json:"settings"`
		} `json:"projectPlates"`
	}
	out := map[string][]map[int]int{}
	for _, id := range sortedKeys(w.Projects) {
		var record struct {
			Doc projectDoc `json:"doc"`
		}
		if w.Projects[id].Record == nil || json.Unmarshal(w.Projects[id].Record, &record) != nil || record.Doc.Printer == "" {
			continue
		}
		doc := record.Doc
		layers := []map[string]json.RawMessage{}
		for _, plate := range doc.Plates {
			layers = append(layers, plate.Settings)
		}
		layers = append(layers, doc.ProjectPresets.Machine, doc.Dirty.Process)
		mode, mapping := "", json.RawMessage(nil)
		for _, l := range layers {
			if v, ok := l["filament_map_mode"]; ok && mode == "" {
				mode = resolve.FirstConfigText(v)
			}
			if v, ok := l["filament_map"]; ok && mapping == nil {
				mapping = v
			}
		}
		if !strings.Contains(mode, "Manual") || mapping == nil {
			continue
		}
		vote := map[int]int{}
		for i, v := range resolve.ConfigList(mapping) {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 {
				vote[i] = n - 1
			}
		}
		out[doc.Printer] = append(out[doc.Printer], vote)
	}
	return out, nil
}
