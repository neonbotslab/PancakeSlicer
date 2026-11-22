package main

import (
	"context"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const printerStatusSchemaVersion = 1
const printerStatusStaleAfter = 30 * time.Second

var errPrinterNotFound = errors.New("printer not found")

type apiPrinterStatus struct {
	SchemaVersion    int               `json:"schema_version"`
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	State            string            `json:"state"`
	Online           bool              `json:"online"`
	Connected        bool              `json:"connected"`
	Progress         *float64          `json:"progress"`
	Layer            apiPrinterLayer   `json:"layer"`
	Temps            apiPrinterTemps   `json:"temps"`
	RemainingSeconds *int              `json:"remaining_seconds"`
	Job              apiPrinterJob     `json:"job"`
	AMS              apiPrinterAMS     `json:"ams"`
	Errors           []apiPrinterError `json:"errors"`
	UpdatedAt        *time.Time        `json:"updated_at"`
	AgeSeconds       *float64          `json:"age_seconds"`
	Stale            bool              `json:"stale"`
}

type apiPrinterError struct {
	Code string `json:"code"`
}

type apiPrinterLayer struct {
	Current *int `json:"current"`
	Total   *int `json:"total"`
}

type apiPrinterTemps struct {
	Nozzles []apiNozzleTemp `json:"nozzles"`
	Bed     *Temp           `json:"bed"`
	Chamber *Temp           `json:"chamber"`
}

type apiNozzleTemp struct {
	NozzleTemp
	Side string `json:"side"`
}

type apiPrinterJob struct {
	File      *string    `json:"file"`
	StartedAt *time.Time `json:"started_at"`
}

type apiPrinterAMS struct {
	Units         []apiAMSUnit `json:"units"`
	ExternalTrays []apiAMSTray `json:"external_trays"`
}

type apiAMSUnit struct {
	AMSUnitStatus
	Trays []apiAMSTray `json:"trays"`
}

type apiAMSTray struct {
	ID               int    `json:"id"`
	Slot             int    `json:"slot"`
	Extruder         *int   `json:"extruder"`
	Type             string `json:"type"`
	Color            string `json:"color"`
	RemainingPercent *int   `json:"remaining_percent"`
	Active           bool   `json:"active"`
}

func projectPrinterStatus(p *PrinterConfig, st PrinterStatus, seen *time.Time, now time.Time) apiPrinterStatus {
	out := apiPrinterStatus{SchemaVersion: printerStatusSchemaVersion, ID: p.Printer, Name: p.Name,
		State: st.State, Online: st.Online, Connected: st.Link == LinkOnline,
		Progress: st.Progress, Layer: apiPrinterLayer{st.Layer, st.Layers},
		Temps:            apiPrinterTemps{Nozzles: []apiNozzleTemp{}, Bed: st.Bed, Chamber: st.Chamber},
		RemainingSeconds: st.Remaining, Job: apiPrinterJob{StartedAt: st.StartedAt},
		AMS:    apiPrinterAMS{Units: []apiAMSUnit{}, ExternalTrays: []apiAMSTray{}},
		Errors: []apiPrinterError{}, UpdatedAt: seen, Stale: true}
	for _, message := range st.Messages {
		out.Errors = append(out.Errors, apiPrinterError{Code: message.Code})
	}
	if out.State == "" {
		out.State = "unavailable"
	}
	if seen != nil {
		age := max(0, now.Sub(*seen).Seconds())
		out.AgeSeconds = &age
		out.Stale = now.Sub(*seen) >= printerStatusStaleAfter
	}
	if st.File != "" {
		out.Job.File = ptr(path.Base(strings.ReplaceAll(st.File, "\\", "/")))
	}
	for _, nozzle := range st.NozzleTemps {
		side := ""
		if len(st.NozzleTemps) == 2 {
			side = map[int]string{0: "right", 1: "left"}[nozzle.ID]
		}
		out.Temps.Nozzles = append(out.Temps.Nozzles, apiNozzleTemp{nozzle, side})
	}
	if len(out.Temps.Nozzles) == 0 && st.Nozzle != nil {
		out.Temps.Nozzles = append(out.Temps.Nozzles, apiNozzleTemp{NozzleTemp: NozzleTemp{ID: 0, Actual: st.Nozzle.Actual, Target: st.Nozzle.Target}})
	}
	sort.Slice(out.Temps.Nozzles, func(i, j int) bool { return out.Temps.Nozzles[i].ID < out.Temps.Nozzles[j].ID })
	units := map[int]*apiAMSUnit{}
	for _, unit := range st.AMSUnits {
		units[unit.ID] = &apiAMSUnit{AMSUnitStatus: unit, Trays: []apiAMSTray{}}
	}
	for _, tray := range st.Trays {
		row := apiAMSTray{ID: tray.ID, Slot: tray.Slot, Extruder: tray.Extruder, Type: tray.Type, Color: tray.Color, RemainingPercent: tray.Remaining, Active: tray.Active}
		if tray.Kind == "external" || isExternalTray(tray.ID) {
			out.AMS.ExternalTrays = append(out.AMS.ExternalTrays, row)
			continue
		}
		if units[tray.AMS] == nil {
			units[tray.AMS] = &apiAMSUnit{AMSUnitStatus: AMSUnitStatus{ID: tray.AMS, Kind: tray.Kind, Extruder: tray.Extruder}, Trays: []apiAMSTray{}}
		}
		units[tray.AMS].Trays = append(units[tray.AMS].Trays, row)
	}
	for _, unit := range units {
		sort.Slice(unit.Trays, func(i, j int) bool { return unit.Trays[i].Slot < unit.Trays[j].Slot })
		out.AMS.Units = append(out.AMS.Units, *unit)
	}
	sort.Slice(out.AMS.Units, func(i, j int) bool { return out.AMS.Units[i].ID < out.AMS.Units[j].ID })
	return out
}

func (s *Server) readPrinterStatuses(ctx context.Context, id string) ([]apiPrinterStatus, error) {
	connections := s.sends.connections()
	if id != "" {
		p := s.sends.printer(id)
		if p == nil {
			return nil, errPrinterNotFound
		}
		connections = []*PrinterConfig{p}
	}
	h := s.sends.hub
	h.observeReadOnly(id)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		h.mu.Lock()
		out := make([]apiPrinterStatus, 0, len(connections))
		waiting := false
		now := time.Now().UTC()
		for _, p := range connections {
			if p.Printer == "" {
				continue
			}
			st := h.status[p.Printer]
			out = append(out, projectPrinterStatus(p, st, h.lastSeen[p.Printer], now))
			waiting = waiting || st.Link == LinkConnecting
		}
		wake := h.wake
		h.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		if !waiting {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return out, nil
		case <-wake:
		}
	}
}

func (s *Server) printerStatuses(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("printer")
	rows, err := s.readPrinterStatuses(r.Context(), id)
	if err != nil {
		if errors.Is(err, errPrinterNotFound) {
			fail(w, 404, "printer_doesnt_have_network")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if id != "" {
		writeJSON(w, 200, rows[0])
		return
	}
	writeJSON(w, 200, map[string]any{"schema_version": printerStatusSchemaVersion, "printers": rows})
}

type mcpPrinterStatusInput struct {
	Printer string `json:"printer,omitempty" jsonschema:"Saved printer ID. Omit to read all configured printers."`
}

func (m *mcpService) getPrinterStatus(ctx context.Context, _ *mcp.CallToolRequest, in mcpPrinterStatusInput) (map[string]any, error) {
	rows, err := m.server.readPrinterStatuses(ctx, in.Printer)
	if err != nil {
		return nil, err
	}
	return map[string]any{"schema_version": printerStatusSchemaVersion, "printers": rows}, nil
}
