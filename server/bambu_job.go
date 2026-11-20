package main

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type BambuJobMapping struct {
	Filaments         []int                 `json:"filament_map"`
	Volumes           []int                 `json:"filament_volume_map"`
	Mode              string                `json:"filament_map_mode,omitempty"`
	HasFilamentSwitch *bool                 `json:"has_filament_switch,omitempty"`
	Materials         []BambuSlicedMaterial `json:"materials"`
}

type BambuSlicedMaterial struct {
	Slot     int     `json:"slot" xml:"id,attr"`
	Type     string  `json:"type" xml:"type,attr"`
	Color    string  `json:"color" xml:"color,attr"`
	ID       string  `json:"id" xml:"tray_info_idx,attr"`
	Flow     string  `json:"flow" xml:"volume_type,attr"`
	Diameter float64 `json:"diameter" xml:"nozzle_diameter,attr"`
	Object   bool    `json:"object" xml:"used_for_object,attr"`
	Support  bool    `json:"support" xml:"used_for_support,attr"`
}

func readBambuJobMapping(path string) (*BambuJobMapping, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	out := &BambuJobMapping{}
	for _, file := range z.File {
		if file.Name != "Metadata/model_settings.config" && file.Name != "Metadata/slice_info.config" {
			continue
		}
		r, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		var config struct {
			Plates []struct {
				Metadata []struct {
					Key   string `xml:"key,attr"`
					Value string `xml:"value,attr"`
				} `xml:"metadata"`
				Filaments []BambuSlicedMaterial `xml:"filament"`
			} `xml:"plate"`
		}
		if err := xml.NewDecoder(io.LimitReader(r, 4<<20)).Decode(&config); err != nil {
			return nil, err
		}
		if len(config.Plates) != 1 {
			return nil, errors.New("Please select a single sliced plate.")
		}
		if file.Name == "Metadata/slice_info.config" {
			out.Materials = config.Plates[0].Filaments
			for _, row := range config.Plates[0].Metadata {
				if row.Key == "has_filament_switcher" {
					value, err := strconv.ParseBool(row.Value)
					if err != nil {
						return nil, err
					}
					out.HasFilamentSwitch = &value
				}
			}
			continue
		}
		for _, row := range config.Plates[0].Metadata {
			var target *[]int
			switch row.Key {
			case "filament_map_mode":
				out.Mode = strings.TrimSpace(row.Value)
				continue
			case "filament_maps":
				target = &out.Filaments
			case "filament_volume_maps":
				target = &out.Volumes
			default:
				continue
			}
			for _, text := range strings.Fields(row.Value) {
				value, err := strconv.Atoi(text)
				if err != nil || value < 0 {
					return nil, errors.New("Oops! The filament mapping for this slice isn't valid.")
				}
				*target = append(*target, value)
			}
		}
	}
	if len(out.Filaments) == 0 {
		return nil, errors.New("This slice doesn't have filament slots assigned yet.")
	}
	return out, nil
}

func (s *Server) bambuJobMapping(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	if j.Request.Output != "gcode.3mf" || j.AwaitFinished(r.Context()) != StateDone {
		fail(w, 409, "no_finished_bambu_gcode")
		return
	}
	mapping, err := readBambuJobMapping(j.OutputPath("gcode.3mf"))
	if err != nil {
		fail(w, 422, "couldnt_read_nozzle_assignment", err.Error())
		return
	}
	writeJSON(w, 200, mapping)
}

func checkSendTopology(sliced *BambuJobMapping, submitted []int, status PrinterStatus, plan sendPlan, printer string) error {
	if submitted != nil && !slices.Equal(submitted, sliced.Filaments) {
		return fmt.Errorf("Oops! This sliced file puts the filaments on nozzles %v, not the %v this job asked for. Give it another slice!", sliced.Filaments, submitted)
	}
	dual := plan.dual || reportedDualNozzles(status)
	side := func(e int) string {
		if dual {
			return []string{"left", "right"}[min(e, 1)] + " nozzle"
		}
		return "nozzle"
	}
	for _, m := range sliced.Materials {
		if !m.Object && !m.Support {
			continue
		}
		if m.Slot < 1 || m.Slot > len(plan.Slots) || m.Slot > len(sliced.Filaments) || sliced.Filaments[m.Slot-1] < 1 {
			return fmt.Errorf("Filament %d in this plate has no valid nozzle assignment. Slice again.", m.Slot)
		}
		tray, nozzle := plan.Slots[m.Slot-1].tray, sliced.Filaments[m.Slot-1]-1
		if tray == nil {
			return fmt.Errorf("Hmm, filament %d is mapped to a tray that %s isn't reporting right now. Pick a loaded tray instead!", m.Slot, printer)
		}
		if dual && tray.Extruder == nil {
			return fmt.Errorf("%s hasn't reported which nozzle feeds filament %d's tray. Wait for its tray report, or pick another tray.", printer, m.Slot)
		}
		if dual && *tray.Extruder != nozzle {
			return fmt.Errorf("Filament %d is sliced for the %s, and its tray feeds the %s. Pick a tray on the %s!", m.Slot, side(nozzle), side(*tray.Extruder), side(nozzle))
		}
		installed := []NozzleStatus{}
		for _, n := range status.Nozzles {
			if n.Extruder == nozzle && !n.Rack {
				installed = append(installed, n)
			}
		}
		if len(installed) == 0 {
			if dual || nozzle > 0 {
				return fmt.Errorf("%s isn't reporting a %s yet. Wait for it to report its nozzles, or slice for the ones it has.", printer, side(nozzle))
			}
			continue
		}
		n := installed[0]
		if m.Diameter > 0 && math.Abs(n.Diameter-m.Diameter) > 1e-3 {
			return fmt.Errorf("Heads up! The %s on %s is %g mm, while this slice is for %g mm. Slice again for the installed nozzle.", side(nozzle), printer, n.Diameter, m.Diameter)
		}
		if m.Flow != "" && n.Flow != "" && m.Flow != n.Flow {
			return fmt.Errorf("Heads up! The %s on %s is %s, while this slice is for %s. Slice again for the installed nozzle.", side(nozzle), printer, n.Flow, m.Flow)
		}
	}
	return nil
}

func reportedDualNozzles(status PrinterStatus) bool {
	return slices.ContainsFunc(status.Nozzles, func(n NozzleStatus) bool { return n.Extruder > 0 }) ||
		slices.ContainsFunc(status.Trays, func(t AMSTray) bool { return t.Kind == "external" && t.Extruder != nil })
}

func (j *Job) submittedFilamentMap() []int {
	var mode, mapping string
	for _, kv := range j.Request.Overrides {
		switch kv[0] {
		case "filament_map_mode":
			mode = kv[1]
		case "filament_map":
			mapping = kv[1]
		}
	}
	if mode != "Manual" || mapping == "" {
		return nil
	}
	out := []int{}
	for _, t := range strings.Split(mapping, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}
