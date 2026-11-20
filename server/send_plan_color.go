package main

import (
	"cmp"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

type sendFilamentAliases map[string]struct {
	BambuID string `json:"bambu_id"`
}

func loadSendFilamentAliases(resources string) sendFilamentAliases {
	b, err := os.ReadFile(filepath.Join(resources, "printers", "bambu_filament_ids.json"))
	if err != nil {
		return nil
	}
	var ids struct {
		Filaments sendFilamentAliases `json:"filaments"`
	}
	if json.Unmarshal(b, &ids) != nil {
		return nil
	}
	return ids.Filaments
}

func (aliases sendFilamentAliases) normalize(id string) string {
	return cmp.Or(aliases[id].BambuID, id)
}

func sendProfileID(lib *resolve.Library, id string) string {
	if f := lib.Filament(id); f != nil && f.Live() && f.Problem == nil {
		return f.ID
	}
	return ""
}

type sendTrayRank struct {
	identity int
	used     int
	distance float64
	moved    int
	key      string
}

func automaticSendTray(lib *resolve.Library, trays map[string]sendTargetTray, used map[string]bool, fixed, profileID string, sliced BambuSlicedMaterial, nozzle int, aliases sendFilamentAliases) string {
	var best sendTrayRank
	if nozzle < 0 {
		return ""
	}
	materialID := aliases.normalize(sliced.ID)
	color := sendColorLab(sliced.Color)
	for key, choice := range trays {
		tray := choice.report
		if tray == nil || choice.ambiguous || !sendTrayFeedsNozzle(tray, nozzle, true) || !sameSendMaterialType(tray.Type, sliced.Type) {
			continue
		}
		rank := sendTrayRank{identity: 2, distance: sendColorDistance(color, sendColorLab(tray.Color)), key: key}
		if choice.slot != nil && profileID != "" && sendProfileID(lib, choice.slot.Filament) == profileID {
			rank.identity = 0
		} else if materialID != "" && aliases.normalize(tray.FilamentID) == materialID {
			rank.identity = 1
		}
		if used[key] {
			rank.used = 1
		}
		if key != fixed {
			rank.moved = 1
		}
		if best.key == "" || cmp.Or(cmp.Compare(rank.identity, best.identity), cmp.Compare(rank.used, best.used),
			cmp.Compare(rank.distance, best.distance), cmp.Compare(rank.moved, best.moved), strings.Compare(rank.key, best.key)) < 0 {
			best = rank
		}
	}
	return best.key
}

func sendColorLab(hex string) [3]float64 {
	hex = strings.TrimPrefix(hex, "#")
	var rgb [3]float64
	for i := range rgb {
		var channel uint64
		if start := i * 2; len(hex) >= start+2 {
			channel, _ = strconv.ParseUint(hex[start:start+2], 16, 8)
		}
		value := float64(channel) / 255
		if value > 0.04045 {
			rgb[i] = math.Pow((value+0.055)/1.055, 2.4)
		} else {
			rgb[i] = value / 12.92
		}
	}
	r, g, b := rgb[0], rgb[1], rgb[2]
	f := func(value float64) float64 {
		if value > 0.008856 {
			return math.Cbrt(value)
		}
		return 7.787*value + 16.0/116
	}
	x := f((0.4124*r + 0.3576*g + 0.1805*b) / 0.9505)
	y := f(0.2126*r + 0.7152*g + 0.0722*b)
	z := f((0.0193*r + 0.1192*g + 0.9505*b) / 1.089)
	return [3]float64{116*y - 16, 500 * (x - y), 200 * (y - z)}
}

func sendColorDistance(a, b [3]float64) float64 {
	l, x, y := a[0]-b[0], a[1]-b[1], a[2]-b[2]
	return l*l + x*x + y*y
}
