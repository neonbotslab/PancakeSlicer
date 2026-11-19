package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOfflinePrinterStatusKeepsLastOnlineReport(t *testing.T) {
	m := NewSendManager(nil)
	p := &PrinterConfig{Printer: "fixture"}
	mon := &monitor{cfg: p}
	m.hub.monitors[p.Printer] = mon

	offline := PrinterStatus{State: "offline", Detail: "fixture unavailable"}
	m.hub.set(mon, offline)
	firstOffline, _ := m.hub.current(p.Printer)
	if firstOffline.LastSeen != nil {
		t.Fatalf("never-online printer has last-seen time %v", firstOffline.LastSeen)
	}

	online := PrinterStatus{State: "idle", Online: true}
	m.hub.set(mon, online)
	firstOnline, _ := m.hub.current(p.Printer)
	repeatedAt := time.Now().UTC()
	m.hub.set(mon, online)
	repeated, _ := m.hub.current(p.Printer)
	if !repeated.At.Equal(firstOnline.At) {
		t.Fatalf("identical online report changed status timestamp from %v to %v", firstOnline.At, repeated.At)
	}

	m.hub.set(mon, offline)
	last, _ := m.hub.current(p.Printer)
	if last.LastSeen == nil || last.LastSeen.Before(repeatedAt) || last.LastSeen.After(last.At) {
		t.Fatalf("offline last seen %v, want time of the online report between %v and %v", last.LastSeen, repeatedAt, last.At)
	}
	offline.Detail = "fixture still unavailable"
	m.hub.set(mon, offline)
	stillOffline, _ := m.hub.current(p.Printer)
	encoded, err := json.Marshal(stillOffline)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		LastSeen *time.Time `json:"last_seen"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.LastSeen == nil || !wire.LastSeen.Equal(*last.LastSeen) {
		t.Fatalf("repeated offline report last seen %v, want %v", wire.LastSeen, last.LastSeen)
	}
}

func TestLastSeenSurvivesIdleMonitorRetirement(t *testing.T) {
	m := NewSendManager(nil)
	p := &PrinterConfig{Printer: "fixture"}
	m.conns = []*PrinterConfig{p}
	mon := &monitor{cfg: p, cancel: func() {}}
	m.hub.monitors[p.Printer] = mon
	m.hub.set(mon, PrinterStatus{State: "idle", Online: true})
	m.hub.set(mon, PrinterStatus{State: "offline"})
	before, _ := m.hub.current(p.Printer)
	m.hub.reconcile()
	if _, ok := m.hub.current(p.Printer); ok {
		t.Fatal("idle monitor status was not retired")
	}
	resumed := &monitor{cfg: p, cancel: func() {}}
	m.hub.monitors[p.Printer] = resumed
	m.hub.set(resumed, PrinterStatus{State: "offline"})
	after, _ := m.hub.current(p.Printer)
	if before.LastSeen == nil || after.LastSeen == nil || !before.LastSeen.Equal(*after.LastSeen) {
		t.Fatalf("last seen changed across idle retirement from %v to %v", before.LastSeen, after.LastSeen)
	}
	m.conns = nil
	m.hub.reconcile()
	m.conns = []*PrinterConfig{p}
	m.hub.monitors[p.Printer] = mon
	m.hub.set(mon, PrinterStatus{State: "offline"})
	readded, _ := m.hub.current(p.Printer)
	if readded.LastSeen != nil {
		t.Fatal("removed printer retained its last-seen timestamp")
	}
}
