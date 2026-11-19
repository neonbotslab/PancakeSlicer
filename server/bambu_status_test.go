package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func replayBambuStatus(t *testing.T, reports ...[]byte) PrinterStatus {
	t.Helper()
	state := map[string]any{}
	var status PrinterStatus
	for _, raw := range reports {
		report := decodeBambuReport(state, raw)
		if report == nil {
			t.Fatalf("could not decode report %s", raw)
		}
		status = report.status()
	}
	return status
}

func TestBambuReportFieldNamesAndNumericIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, delta string
		state       string
		temp        uint32
	}{
		{"case folded update", `{"print":{"GCODE_STATE":"RUNNING"}}`, "RUNNING", 1},
		{"exact name wins", `{"print":{"GCODE_STATE":"FAILED","gcode_state":"RUNNING"}}`, "RUNNING", 1},
		{"malformed unicode field", `{"print":{"gcode_ſtate":{},"mc_percent":42}}`, "IDLE", 1},
		{"nested folded names", `{"print":{"DEVICE":{"EXTRUDER":{"INFO":[{"ID":0,"TEMP":7}]}}}}`, "IDLE", 7},
		{"negative zero identity", `{"print":{"device":{"extruder":{"info":[{"id":-0,"temp":7}]}}}}`, "IDLE", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := map[string]any{}
			if decodeBambuReport(state, []byte(`{"print":{"gcode_state":"IDLE","device":{"extruder":{"info":[{"id":0,"temp":1}]}}}}`)) == nil {
				t.Fatal("could not decode initial state")
			}
			report := decodeBambuReport(state, []byte(tc.delta))
			if report == nil || report.State != tc.state || report.Device == nil || report.Device.Extruder == nil || len(report.Device.Extruder.Info) != 1 || report.Device.Extruder.Info[0].Temp != tc.temp {
				t.Fatalf("field normalization lost or duplicated state: %+v", report)
			}
			if tc.name == "malformed unicode field" && (report.Percent == nil || *report.Percent != 42) {
				t.Fatalf("malformed case variant discarded valid sibling: %+v", report)
			}
		})
	}
}

func TestBambuExternalTrayNozzle(t *testing.T) {
	for _, c := range []struct {
		name     string
		report   string
		id       int
		extruder *int
	}{
		{"legacy H2D", `{"print":{"vt_tray":{"id":"254","tray_type":"PLA"},"device":{"extruder":{"info":[{"id":0},{"id":1}]}}}}`, 255, ptr(1)},
		{"legacy P1S", `{"print":{"vt_tray":{"id":"254","tray_type":"PLA"},"nozzle_diameter":"0.4","ams":{"tray_now":"254"}}}`, 255, nil},
		{"modern right", `{"print":{"vir_slot":[{"id":"255","tray_type":"PLA"}],"device":{"extruder":{"info":[{"id":0},{"id":1}]}}}}`, 255, ptr(1)},
		{"modern left", `{"print":{"vir_slot":[{"id":"254","tray_type":"PLA"}],"device":{"extruder":{"info":[{"id":0},{"id":1}]}}}}`, 254, ptr(0)},
		{"dual nozzle existence flags", `{"print":{"vir_slot":[{"id":"255","tray_type":"PLA"}],"device":{"nozzle":{"exist":3}}}}`, 255, ptr(1)},
		{"left nozzle row", `{"print":{"vir_slot":[{"id":"255","tray_type":"PLA"}],"device":{"nozzle":{"info":[{"id":1,"diameter":0.4}]}}}}`, 255, ptr(1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			status := replayBambuStatus(t, []byte(c.report))
			if len(status.Trays) != 1 {
				t.Fatalf("got %d trays, want 1", len(status.Trays))
			}
			tray := status.Trays[0]
			if tray.ID != c.id || tray.AMS != c.id {
				t.Errorf("got tray ID %d and AMS ID %d, want both %d", tray.ID, tray.AMS, c.id)
			}
			if c.extruder == nil {
				if tray.Extruder != nil || !tray.Active {
					t.Errorf("got side set %t and active %t, want no side and active", tray.Extruder != nil, tray.Active)
				}
			} else if tray.Extruder == nil {
				t.Errorf("got no side, want %d", *c.extruder)
			} else if *tray.Extruder != *c.extruder {
				t.Errorf("got side %d, want %d", *tray.Extruder, *c.extruder)
			}
		})
	}
}

func TestBambuStatusReports(t *testing.T) {
	fixture, err := os.ReadFile("testdata/bambu-status/synthetic-h2d.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	reports := bytes.Split(bytes.TrimSpace(fixture), []byte{'\n'})

	t.Run("one report combines modern left and legacy right slots", func(t *testing.T) {
		trays := replayBambuStatus(t, reports[6]).Trays
		if len(trays) != 2 || trays[0].ID != 254 || trays[0].Type != "ASA" || trays[1].ID != 255 || trays[1].Type != "ABS" {
			t.Errorf("got trays %v, want modern left ASA and legacy right ABS", trays)
		}
	})

	t.Run("sparse updates preserve trays and nozzles", func(t *testing.T) {
		status := replayBambuStatus(t, reports[:2]...)
		trays := map[int]AMSTray{}
		for _, tray := range status.Trays {
			trays[tray.ID] = tray
		}
		if tray := trays[0]; tray.Kind != "ams-2-pro" || tray.Extruder == nil || *tray.Extruder != 1 || tray.Type != "PLA" || tray.Color != "#E03030" || tray.Remaining == nil || *tray.Remaining != 59 || !tray.Active {
			t.Error("right AMS tray lost its binding, filament, or active state after a remaining-percentage update")
		}
		if trays[1].Type != "PETG" || trays[128].Type != "ABS" || trays[255].Type != "PLA" {
			t.Errorf("sparse update dropped a tray, AMS unit, or right external spool, retained trays %v", status.Trays)
		}
		if tray := trays[254]; tray.Extruder == nil || *tray.Extruder != 0 || tray.Type != "ASA" || tray.Color != "#AABBCC" || tray.FilamentID != "GFG99" {
			t.Errorf("left external spool did not retain its color and filament ID, got %+v", tray)
		}
		if len(status.NozzleTemps) != 2 || status.NozzleTemps[0] != (NozzleTemp{ID: 0, Actual: 208, Target: 210}) || status.NozzleTemps[1] != (NozzleTemp{ID: 1, Actual: 220, Target: 230}) {
			t.Errorf("got nozzle temperatures %v, want updated right and unchanged left", status.NozzleTemps)
		}
		right := NozzleStatus{ID: 0, Extruder: 1, Diameter: 0.4, Type: "hardened_steel", Flow: "Standard"}
		left := NozzleStatus{ID: 1, Extruder: 0, Diameter: 0.6, Type: "stainless_steel", Flow: "Standard", Abnormal: true}
		if len(status.Nozzles) != 2 || status.Nozzles[0] != right || status.Nozzles[1] != left {
			t.Errorf("got nozzles %v, want both nozzles with updated left condition", status.Nozzles)
		}
		if len(status.Messages) != 1 || status.Messages[0].Code != "00000003-00000003" {
			t.Errorf("got messages %v, want just the latest HMS message", status.Messages)
		}
	})

	t.Run("explicit empty arrays clear their entries", func(t *testing.T) {
		status := replayBambuStatus(t, reports[:3]...)
		if len(status.Trays) != 2 || status.Trays[0].ID != 0 || status.Trays[0].Type != "" || status.Trays[1].ID != 128 || len(status.NozzleTemps) != 0 || len(status.Messages) != 0 {
			t.Errorf("got trays %v, temperatures %v, messages %v, want empty connected AMS and untouched AMS HT", status.Trays, status.NozzleTemps, status.Messages)
		}
	})

	t.Run("full push replaces previously reported state", func(t *testing.T) {
		status := replayBambuStatus(t, reports[0], reports[3])
		if len(status.Trays) != 1 || status.Trays[0].ID != 255 || status.Trays[0].Type != "PETG" || status.Trays[0].Extruder != nil || len(status.NozzleTemps) != 0 || len(status.Messages) != 0 {
			t.Errorf("got trays %v, temperatures %v, messages %v, want just the latest main external spool", status.Trays, status.NozzleTemps, status.Messages)
		}
	})

	t.Run("push without msg preserves cached state", func(t *testing.T) {
		delta := []byte(`{"print":{"command":"push_status","gcode_state":"RUNNING"}}`)
		status := replayBambuStatus(t, reports[0], delta)
		if status.State != "printing" || len(status.Trays) != 5 || len(status.NozzleTemps) != 2 {
			t.Errorf("partial push did not retain inventory and apply state: %+v", status)
		}
	})

	t.Run("invalid modern external slots do not become main spools", func(t *testing.T) {
		status := replayBambuStatus(t, []byte(`{"print":{"vir_slot":[{"id":"unknown","tray_type":"PLA"},{"id":"253","tray_type":"ABS"},{"id":"255","tray_type":"PETG"}]}}`))
		if len(status.Trays) != 1 || status.Trays[0].ID != 255 || status.Trays[0].Type != "PETG" || status.Trays[0].Extruder != nil {
			t.Errorf("invalid slot fabricated a main spool: %+v", status.Trays)
		}
	})

	t.Run("unsupported external slots do not clear inventory", func(t *testing.T) {
		unknown := []byte(`{"print":{"command":"push_status","msg":1,"vir_slot":[{"id":"253"},{}]}}`)
		if status := replayBambuStatus(t, unknown); status.Trays != nil {
			t.Errorf("unsupported slots reported inventory: %v", status.Trays)
		}
		if status := replayBambuStatus(t, reports[0], unknown); len(status.Trays) != 5 {
			t.Errorf("unsupported rows changed cached inventory: %v", status.Trays)
		}
	})

	t.Run("malformed report does not poison the next update", func(t *testing.T) {
		state := map[string]any{}
		if decodeBambuReport(state, reports[0]) == nil {
			t.Fatal("could not decode the initial report")
		}
		partial := decodeBambuReport(state, []byte(`{"print":{"command":"push_status","msg":1,"gcode_state":"RUNNING","vir_slot":[{"id":255,"tray_type":"ABS"},{"id":"254","tray_type":"TPU","tray_color":987}]}}`))
		if partial == nil || partial.State != "RUNNING" {
			t.Fatal("malformed fields discarded a valid state update")
		}
		for _, tray := range partial.trays() {
			if tray.ID == 255 && tray.Type != "PLA" || tray.ID == 254 && (tray.Type != "TPU" || tray.Color != "#AABBCC") {
				t.Errorf("partial update did not isolate malformed external tray fields: %+v", tray)
			}
		}
		report := decodeBambuReport(state, []byte(`{"print":{"command":"push_status","msg":1,"vir_slot":[{"id":"255","tray_type":"ASA"}]}}`))
		if report == nil {
			t.Fatal("malformed report prevented the valid update from decoding")
		}
		materials := map[int]string{}
		for _, tray := range report.trays() {
			materials[tray.ID] = tray.Type
		}
		if materials[255] != "ASA" || materials[254] != "TPU" {
			t.Errorf("got external materials %q and %q, want right ASA and left TPU", materials[255], materials[254])
		}
	})

	t.Run("wrong nested types keep valid siblings and cached values", func(t *testing.T) {
		delta := []byte(`{"print":{"command":"push_status","msg":[],"gcode_state":"RUNNING","ams":{"ams":[{"id":7}]},"device":{"extruder":{"info":[{"id":"0","temp":1},{"id":1,"temp":15073502,"snow":[]}] },"nozzle":{"info":[{"id":0,"diameter":{},"stat":2},{"id":true,"diameter":0.8}]}},"hms":[false]}}`)
		status := replayBambuStatus(t, reports[0], delta)
		if status.State != "printing" || len(status.Trays) != 5 || len(status.Messages) != 2 {
			t.Errorf("invalid collections or message type changed cached state: %+v", status)
		}
		if len(status.NozzleTemps) != 2 || status.NozzleTemps[0].ID != 0 || status.NozzleTemps[1] != (NozzleTemp{ID: 1, Actual: 222, Target: 230}) {
			t.Errorf("malformed extruder fields prevented the valid temperature update: %v", status.NozzleTemps)
		}
		if len(status.Nozzles) != 2 || status.Nozzles[0].Diameter != 0.4 || !status.Nozzles[0].Abnormal {
			t.Errorf("malformed nozzle fields prevented the status update: %v", status.Nozzles)
		}
	})

	for _, c := range []struct {
		name                       string
		update                     []byte
		right, left, expectedColor string
	}{
		{"legacy update after modern report", reports[4], "ABS", "PETG", "#FF4400"},
		{"modern right slot takes precedence in same report", reports[5], "TPU", "ASA", "#EEEEEE"},
		{"legacy right slot accompanies modern left slot", reports[6], "ABS", "ASA", "#FF4400"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status := replayBambuStatus(t, reports[0], c.update)
			var right, left AMSTray
			for _, tray := range status.Trays {
				switch tray.ID {
				case 255:
					right = tray
				case 254:
					left = tray
				}
			}
			if right.Type != c.right || right.Color != c.expectedColor || right.Extruder == nil || *right.Extruder != 1 {
				t.Errorf("got right external spool %+v, want %s, color %s, and logical side 1", right, c.right, c.expectedColor)
			}
			if left.Type != c.left || left.Color != "#AABBCC" || left.Extruder == nil || *left.Extruder != 0 {
				t.Errorf("got left external spool %+v, want %s, unchanged color, and logical side 0", left, c.left)
			}
		})
	}

	t.Run("merge preserves its inputs", func(t *testing.T) {
		state := map[string]any{}
		for _, report := range [][]byte{reports[0], reports[5], reports[4]} {
			var message struct {
				Print map[string]any `json:"print"`
			}
			decoder := json.NewDecoder(bytes.NewReader(report))
			decoder.UseNumber()
			if err := decoder.Decode(&message); err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(message.Print)
			if err != nil {
				t.Fatal(err)
			}
			mergeReport(state, message.Print)
			t.Cleanup(func() {
				after, err := json.Marshal(message.Print)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Errorf("merge modified its input\nbefore: %s\n after: %s", before, after)
				}
			})
		}
	})
}

func TestBambuAMSInventory(t *testing.T) {
	full := []byte(`{"print":{"command":"push_status","msg":0,"ams":{"ams_exist_bits":"1011","tray_exist_bits":"2010003","tray_now":"0","ams":[{"id":"0","info":"2003","tray":[{"id":"0","tray_type":"PLA","tray_color":"112233FF","tray_sub_brands":"Basic","tray_info_idx":"GFL99","tray_uuid":"example-spool","remain":50},{"id":"1","tray_type":"PETG"}]},{"id":"128","info":"0104","tray":[{"id":"0","tray_type":"ABS"}]},{"id":"6","info":"0005","tray":[{"id":"1","tray_type":"ASA"}]}]}}}`)
	changed := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"ams_exist_bits":"1001","tray_exist_bits":"2"}}}`)

	t.Run("masks hide disconnected units and clear empty slots", func(t *testing.T) {
		before := replayBambuStatus(t, full)
		if len(before.Trays) != 4 || before.Trays[0].Type != "PLA" || before.Trays[2].ID != 128 || before.Trays[2].Type != "ABS" || before.Trays[3].ID != 25 || before.Trays[3].Type != "ASA" {
			t.Fatalf("full inventory missing loaded material: %v", before.Trays)
		}
		after := replayBambuStatus(t, full, changed)
		if len(after.Trays) != 3 || after.Trays[0].ID != 0 || after.Trays[1].ID != 1 || after.Trays[1].Type != "PETG" || after.Trays[2].ID != 25 || after.Trays[2].Type != "" {
			t.Fatalf("got trays %v, want empty slots 0 and 25, loaded slot 1, and no disconnected AMS HT", after.Trays)
		}
		empty := after.Trays[0]
		if empty.AMS != 0 || empty.Kind != "ams-2-pro" || empty.Extruder == nil || *empty.Extruder != 1 || empty.Type != "" || empty.Color != "" || empty.Name != "" || empty.FilamentID != "" || empty.Spool != "" || empty.Remaining != nil || empty.Active {
			t.Errorf("empty slot retained filament or lost AMS capacity: %+v", empty)
		}
	})

	t.Run("omitted or invalid masks preserve the last valid inventory", func(t *testing.T) {
		partial := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"ams":[{"id":"0","tray":[{"id":"1","tray_color":"998877FF"}]}]}}}`)
		invalid := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"ams_exist_bits":"invalid","tray_exist_bits":9}}}`)
		status := replayBambuStatus(t, full, changed, partial, invalid)
		if len(status.Trays) != 3 || status.Trays[0].Type != "" || status.Trays[1].Type != "PETG" || status.Trays[1].Color != "#998877" || status.Trays[2].Type != "" {
			t.Errorf("omitted or invalid masks changed inventory: %v", status.Trays)
		}
	})

	t.Run("connected unit still reports capacity with no tray rows", func(t *testing.T) {
		status := replayBambuStatus(t, []byte(`{"print":{"ams":{"ams_exist_bits":"1","tray_exist_bits":"0","ams":[{"id":"0","info":"2003","tray":[]}]}}}`))
		if len(status.Trays) == 0 || status.Trays[0].AMS != 0 || status.Trays[0].Kind != "ams-2-pro" || status.Trays[0].Extruder == nil || *status.Trays[0].Extruder != 1 || status.Trays[0].Type != "" {
			t.Errorf("empty AMS lost its capacity: %v", status.Trays)
		}
	})
}

func TestBambuSupportFilamentTypes(t *testing.T) {
	status := replayBambuStatus(t, []byte(`{"print":{"ams":{"ams":[{"id":"0","tray":[{"id":"0","tray_type":"PLA","tray_info_idx":"GFS00"},{"id":"1","tray_type":"PA","tray_info_idx":"GFS01"},{"id":"2","tray_info_idx":"GFS00"},{"id":"3","tray_type":"PLA","tray_info_idx":"GFS99"}]}]},"vir_slot":[{"id":"255","tray_type":"PLA","tray_info_idx":"GFS00"},{"id":"254","tray_type":"PA","tray_info_idx":"GFS01"}]}}`))
	want := map[int]string{0: "PLA-S", 1: "PA-S", 2: "", 3: "PLA", 254: "PA-S", 255: "PLA-S"}
	if len(status.Trays) != len(want) {
		t.Fatalf("got %d trays, want %d", len(status.Trays), len(want))
	}
	for _, tray := range status.Trays {
		if expected, ok := want[tray.ID]; !ok || tray.Type != expected {
			t.Errorf("tray %d has material %q, want %q", tray.ID, tray.Type, expected)
		}
	}
}

func TestBambuAMSInventoryReconnect(t *testing.T) {
	loaded := []byte(`{"print":{"command":"push_status","msg":0,"ams":{"ams_exist_bits":"1","tray_exist_bits":"1","ams":[{"id":"0","info":"2003","tray":[{"id":"0","tray_type":"PLA","tray_color":"112233FF","tray_info_idx":"GFS00","tray_uuid":"example-old-spool","remain":70}]}]}}}`)
	present := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"ams_exist_bits":"1","tray_exist_bits":"1"}}}`)
	fresh := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"ams":[{"id":"0","info":"0103","tray":[{"id":"0","tray_type":"PA","tray_color":"445566FF","tray_info_idx":"GFS01"}]}]}}}`)

	t.Run("presence bits do not restore emptied filament", func(t *testing.T) {
		empty := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"tray_exist_bits":"0"}}}`)
		status := replayBambuStatus(t, loaded, empty, present)
		if len(status.Trays) != 1 || status.Trays[0].ID != 0 || status.Trays[0].Type != "" || status.Trays[0].Color != "" || status.Trays[0].FilamentID != "" || status.Trays[0].Spool != "" || status.Trays[0].Remaining != nil {
			t.Errorf("slot presence restored old filament: %+v", status.Trays)
		}
		updated := replayBambuStatus(t, loaded, empty, present, fresh)
		if len(updated.Trays) != 1 || updated.Trays[0].Type != "PA-S" || updated.Trays[0].FilamentID != "GFS01" || updated.Trays[0].Spool != "" || updated.Trays[0].Remaining != nil {
			t.Errorf("fresh filament retained old spool data: %+v", updated.Trays)
		}
	})

	t.Run("reattaching waits for fresh AMS routing and filament", func(t *testing.T) {
		disconnected := []byte(`{"print":{"command":"push_status","msg":1,"ams":{"ams_exist_bits":"0"}}}`)
		status := replayBambuStatus(t, loaded, disconnected, present)
		if len(status.Trays) != 0 {
			t.Errorf("AMS presence restored old routing or filament: %+v", status.Trays)
		}
		updated := replayBambuStatus(t, loaded, disconnected, present, fresh)
		if len(updated.Trays) != 1 || updated.Trays[0].Extruder == nil || *updated.Trays[0].Extruder != 0 || updated.Trays[0].Type != "PA-S" || updated.Trays[0].Color != "#445566" || updated.Trays[0].Spool != "" || updated.Trays[0].Remaining != nil {
			t.Errorf("fresh AMS metadata did not establish the new left binding and filament: %+v", updated.Trays)
		}
	})
}
