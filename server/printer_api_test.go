package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestPrinterAPIStatusProjection(t *testing.T) {
	report := decodeBambuReport(map[string]any{}, []byte(`{"print":{"gcode_state":"RUNNING","mc_percent":42,"mc_remaining_time":17,"layer_num":12,"total_layer_num":100,"bed_temper":60,"chamber_temper":31,"subtask_name":"example.3mf","device":{"extruder":{"info":[{"id":0,"temp":13762768},{"id":1,"temp":15073500}]}},"ams":{"ams":[{"id":"0","info":"3","humidity":"2","humidity_raw":"35","tray":[{"id":"0","tray_type":"PLA","tray_color":"FF8000FF","tray_uuid":"private-spool"}]}]},"hms":[{"attr":1,"code":2,"message":"Inspect filament"}]}}`))
	if report == nil {
		t.Fatal("report was not decoded")
	}
	now := time.Now().UTC()
	st := report.status()
	st.Link = LinkOnline
	st.Messages = append(st.Messages, PrinterMessage{Code: "upstream_error", Message: "private-access-code private-host private-path"})
	row := projectPrinterStatus(&PrinterConfig{Printer: "example", Name: "Example printer", Serial: "private-serial", Host: "private-host"}, st, &now, now)
	if row.SchemaVersion != 1 || row.State != "printing" || !row.Online || !row.Connected || row.Stale || *row.Progress != 0.42 || *row.RemainingSeconds != 1020 || *row.Layer.Current != 12 || *row.Layer.Total != 100 {
		t.Fatalf("incorrect normalized telemetry: %+v", row)
	}
	if len(row.Temps.Nozzles) != 2 || row.Temps.Nozzles[0].Side != "right" || row.Temps.Nozzles[1].Side != "left" || row.Temps.Nozzles[0].Actual != 208 || row.Temps.Nozzles[1].Actual != 220 {
		t.Fatalf("incorrect H2D temperatures: %+v", row.Temps.Nozzles)
	}
	if len(row.AMS.Units) != 1 || *row.AMS.Units[0].HumidityPercent != 35 || *row.AMS.Units[0].HumidityLevel != 2 || row.AMS.Units[0].Trays[0].Type != "PLA" || row.AMS.Units[0].Trays[0].Color != "#FF8000" {
		t.Fatalf("incorrect AMS: %+v", row.AMS)
	}
	if len(row.Errors) != 2 || row.Errors[0].Code != "00000001-00000002" || *row.Job.File != "example.3mf" {
		t.Fatalf("incorrect job/errors: %+v", row)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-")) || bytes.Contains(raw, []byte("serial")) || bytes.Contains(raw, []byte("spool")) {
		t.Fatalf("private connection or spool data in projection")
	}
}

func TestPrinterAPIStaleness(t *testing.T) {
	now := time.Now().UTC()
	for _, tt := range []struct {
		name  string
		seen  *time.Time
		stale bool
		age   *float64
	}{
		{"never_seen", nil, true, nil},
		{"fresh", ptr(now.Add(-29 * time.Second)), false, ptr(29.0)},
		{"threshold", ptr(now.Add(-30 * time.Second)), true, ptr(30.0)},
		{"future_clock", ptr(now.Add(time.Second)), false, ptr(0.0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := projectPrinterStatus(&PrinterConfig{}, PrinterStatus{}, tt.seen, now)
			if row.Stale != tt.stale || (row.AgeSeconds == nil) != (tt.age == nil) || tt.age != nil && *row.AgeSeconds != *tt.age {
				t.Fatalf("stale=%v age=%v", row.Stale, row.AgeSeconds)
			}
		})
	}
}

func TestPrinterAPIFreshnessUsesRepeatedReportReceipt(t *testing.T) {
	m := NewSendManager(nil)
	p := &PrinterConfig{Printer: "example"}
	mon := &monitor{cfg: p, link: LinkOnline}
	m.hub.monitors[p.Printer] = mon
	st := PrinterStatus{Online: true, State: "idle"}
	m.hub.set(mon, st)
	old := time.Now().Add(-time.Hour)
	cached, _ := m.hub.current(p.Printer)
	cached.At = old
	m.hub.status[p.Printer] = cached
	m.hub.set(mon, st)
	current, _ := m.hub.current(p.Printer)
	row := projectPrinterStatus(p, current, m.hub.lastSeen[p.Printer], time.Now())
	if row.Stale || *row.AgeSeconds > 1 || !current.At.Equal(old) {
		t.Fatalf("receipt age must be fresh even when unchanged status retains old At: %+v", row)
	}
}

func TestPassiveBambuMonitorNeverPublishes(t *testing.T) {
	client, broker := net.Pipe()
	defer broker.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan PrinterStatus, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- monitorBambuConn(ctx, client, bufio.NewReader(client), &PrinterConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(st PrinterStatus) { published <- st }, nil, true)
	}()
	wire := append(mqttString("device/fixture/report"), []byte(`{"print":{"gcode_state":"IDLE"}}`)...)
	if err := mqttWrite(broker, 0x30, wire); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-published:
		if st.State != "idle" {
			t.Fatal(st.State)
		}
	case <-time.After(time.Second):
		t.Fatal("passive monitor blocked before reading a report, possibly publishing")
	}
	broker.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	b := make([]byte, 1)
	if n, _ := broker.Read(b); n != 0 {
		t.Fatal("passive monitor wrote to printer")
	}
	cancel()
	broker.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("passive monitor did not stop")
	}
}

func TestPrinterAPIAuthAndRoutes(t *testing.T) {
	m := NewSendManager(nil)
	m.conns = []*PrinterConfig{{Printer: "example", Kind: "bambu-connect", Name: "Example printer"}}
	s := &Server{cfg: Config{BearerToken: "fixture-token"}, sends: m}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/printers/status", s.printerStatuses)
	mux.HandleFunc("GET /api/printers/{printer}/status", s.printerStatuses)
	handler := s.middleware(mux)
	for _, path := range []string{"/api/printers/status", "/api/printers/example/status", "/api/printers/example/camera/stream.mjpeg", "/api/printers/example/camera/snapshot.jpg"} {
		for _, token := range []string{"", "wrong"} {
			r := httptest.NewRequest("GET", path+"?token=fixture-token", nil)
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("unauthorized %s got %d", path, w.Code)
			}
		}
	}
	for _, path := range []string{"/api/printers/status", "/api/printers/example/status"} {
		for _, cookie := range []bool{false, true} {
			r := httptest.NewRequest("GET", path, nil)
			if cookie {
				r.AddCookie(&http.Cookie{Name: accessCookie, Value: s.accessProof()})
			} else {
				r.Header.Set("Authorization", "Bearer fixture-token")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"schema_version":1`) || !strings.Contains(w.Body.String(), `"stale":true`) {
				t.Fatalf("authorized %s got %d %s", path, w.Code, w.Body.String())
			}
		}
	}
	r := httptest.NewRequest("GET", "/api/printers/missing/status", nil)
	r.Header.Set("Authorization", "Bearer fixture-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestPrinterStatusMCP(t *testing.T) {
	m := NewSendManager(nil)
	m.conns = []*PrinterConfig{{Printer: "example", Name: "Example printer", Kind: "bambu-connect"}}
	s := &Server{cfg: Config{MCPEnabled: true}, sends: m}
	httpServer := httptest.NewServer(s.mcpHandler())
	defer httpServer.Close()
	session := mcpTestConnect(t, httpServer, httpServer.Client(), nil)
	defer session.Close()
	var result struct {
		SchemaVersion int                `json:"schema_version"`
		Printers      []apiPrinterStatus `json:"printers"`
	}
	mcpTestCall(t, session, "get_printer_status", map[string]any{"printer": "example"}, &result)
	if result.SchemaVersion != 1 || len(result.Printers) != 1 || result.Printers[0].ID != "example" || !result.Printers[0].Stale {
		t.Fatalf("MCP status differs from HTTP contract: %+v", result)
	}
}

func TestPrinterAPIDoesNotInventNozzleTemperature(t *testing.T) {
	report := decodeBambuReport(map[string]any{}, []byte(`{"print":{"gcode_state":"IDLE","device":{"extruder":{"info":[{"id":0,"snow":1}]}}}}`))
	if report == nil || len(report.status().NozzleTemps) != 0 {
		t.Fatal("an absent packed temperature became a zero-degree measurement")
	}
}

func TestPrinterAPIPassiveDemandDoesNotExtendLegacyGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &PrinterConfig{Printer: "fixture", Kind: "bambu-lan"}
		m := NewSendManager(nil)
		m.conns = []*PrinterConfig{p}
		h := m.hub
		h.observeReadOnly(p.Printer)
		h.mu.Lock()
		passive := h.monitors[p.Printer]
		h.mu.Unlock()
		if passive == nil || !passive.passive {
			t.Fatal("API-only demand did not create a passive monitor")
		}

		unsubscribe := h.subscribe()
		h.mu.Lock()
		active := h.monitors[p.Printer]
		h.mu.Unlock()
		if active == nil || active.passive || active == passive {
			t.Fatal("legacy demand did not replace the passive monitor")
		}
		unsubscribe()

		time.Sleep(monitorLinger - time.Second)
		h.observeReadOnly(p.Printer)
		h.mu.Lock()
		duringGrace := h.monitors[p.Printer]
		h.mu.Unlock()
		if duringGrace != active {
			t.Fatal("API observation interrupted legacy grace")
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		h.mu.Lock()
		downgraded := h.monitors[p.Printer]
		h.passive[p.Printer].Stop()
		delete(h.passive, p.Printer)
		h.mu.Unlock()
		h.reconcile()
		if downgraded == nil || !downgraded.passive || downgraded == active {
			t.Fatal("API-only demand carried the active monitor past legacy grace")
		}
	})
}
