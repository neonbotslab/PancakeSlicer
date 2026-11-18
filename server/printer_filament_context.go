package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const printerFilamentContextFile = "printer-filament-context.json"

type printerFilament struct {
	Extruder int    `json:"extruder"`
	Type     string `json:"type"`
	Color    string `json:"color"`
	External bool   `json:"external"`
}

type filamentContext struct {
	AMSSlots        [2]int            `json:"ams_slots"`
	LoadedFilaments []printerFilament `json:"loaded_filaments"`
}

var filamentContextColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
var invalidFilamentContext = errors.New("printer_filament_context requires two AMS capacities of 0 to 64 and at most 128 valid nozzle-bound filament entries")

func parsePrinterFilamentContext(raw string) (*filamentContext, error) {
	if len(raw) > 32<<10 {
		return nil, invalidFilamentContext
	}
	var wire struct {
		AMSSlots        []*int `json:"ams_slots"`
		LoadedFilaments []struct {
			Extruder *int   `json:"extruder"`
			Type     string `json:"type"`
			Color    string `json:"color"`
			External *bool  `json:"external"`
		} `json:"loaded_filaments"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || len(wire.AMSSlots) != 2 || wire.LoadedFilaments == nil || len(wire.LoadedFilaments) > 128 {
		return nil, invalidFilamentContext
	}
	context := &filamentContext{LoadedFilaments: make([]printerFilament, 0, len(wire.LoadedFilaments))}
	for side, slots := range wire.AMSSlots {
		if slots == nil || *slots < 0 || *slots > 64 {
			return nil, invalidFilamentContext
		}
		context.AMSSlots[side] = *slots
	}
	var ams, external [2]int
	for _, row := range wire.LoadedFilaments {
		material := strings.TrimSpace(row.Type)
		if row.Extruder == nil || (*row.Extruder != 0 && *row.Extruder != 1) || row.External == nil || material == "" || len(material) > 64 ||
			strings.IndexFunc(material, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || !filamentContextColor.MatchString(row.Color) {
			return nil, invalidFilamentContext
		}
		side := *row.Extruder
		if *row.External {
			external[side]++
		} else {
			ams[side]++
		}
		if external[side] > 1 || ams[side] > context.AMSSlots[side] {
			return nil, invalidFilamentContext
		}
		context.LoadedFilaments = append(context.LoadedFilaments, printerFilament{Extruder: side, Type: material, Color: strings.ToUpper(row.Color), External: *row.External})
	}
	slices.SortFunc(context.LoadedFilaments, func(a, b printerFilament) int {
		if a.Extruder != b.Extruder {
			return cmp.Compare(a.Extruder, b.Extruder)
		}
		if a.External != b.External {
			if a.External {
				return 1
			}
			return -1
		}
		if a.Type != b.Type {
			return cmp.Compare(a.Type, b.Type)
		}
		return cmp.Compare(a.Color, b.Color)
	})
	return context, nil
}

func (context *filamentContext) write(input string) error {
	raw, err := json.Marshal(context)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(input, printerFilamentContextFile), raw, 0600)
}
