package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlowCalibrationRequestPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    json.RawMessage
		legacy bool
		want   flowCalibrationMode
	}{
		{"omitted", nil, false, flowCalibrationOff},
		{"legacy on", nil, true, flowCalibrationOn},
		{"explicit off overrides legacy on", json.RawMessage(`"off"`), true, flowCalibrationOff},
		{"explicit on overrides legacy off", json.RawMessage(`"on"`), false, flowCalibrationOn},
		{"explicit auto overrides legacy on", json.RawMessage(`"auto"`), true, flowCalibrationAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFlowCalibrationMode(tc.raw, tc.legacy)
			if err != nil || got != tc.want {
				t.Fatalf("mode = %v, error = %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestSendRejectsInvalidFlowCalibrationBeforeTransfer(t *testing.T) {
	job := &Job{ID: "fixture", Dir: t.TempDir(), state: StateDone, Request: JobRequest{Output: "gcode.3mf"}}
	sends := NewSendManager(nil)
	sends.store = &printerStore{}
	sends.conns = []*PrinterConfig{{Printer: "fixture-printer", Kind: "bambu-lan"}}
	server := &Server{jobs: &JobManager{jobs: map[string]*Job{job.ID: job}}, sends: sends}
	for _, invalid := range []string{`""`, `"AUTO"`, `"manual"`, `null`, `true`, `2`, `[]`, `{}`} {
		t.Run(invalid, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/jobs/fixture/send", strings.NewReader(`{"printer":"fixture-printer","flow_cali":true,"flow_cali_mode":`+invalid+`}`))
			r.SetPathValue("id", job.ID)
			w := httptest.NewRecorder()
			server.sendJob(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "flow_cali_mode") {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
			if job.send != nil || len(sends.buckets) != 0 {
				t.Fatal("invalid mode started transfer setup")
			}
		})
	}
}

func TestBambuAutoFlowConnectionCapability(t *testing.T) {
	for _, tc := range []struct {
		name, kind, model, serial string
		want                      bool
	}{
		{"display name", "bambu-lan", "  Bambu Lab h2d pro  ", "", true},
		{"model code", "bambu-lan", "o1c2", "", true},
		{"serial fallback", "bambu-lan", "", "094FIXTURE", true},
		{"project code is not a serial prefix", "bambu-lan", "", "O1EFIXTURE", false},
		{"unknown wins over serial", "bambu-lan", "Future printer", "094FIXTURE", false},
		{"legacy serial blocks stale model", "bambu-lan", "H2D", "01PFIXTURE", false},
		{"legacy model", "bambu-lan", "P1S", "", false},
		{"unknown", "bambu-lan", "", "", false},
		{"different connection kind", "bambu-connect", "H2D", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &PrinterConfig{Kind: tc.kind, model: tc.model, Serial: tc.serial}
			if got := p.view().AutoFlowCalibration; got != tc.want {
				t.Fatalf("auto_flow_calibration = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestBambuAutoFlowModelsMatchEngine(t *testing.T) {
	files, err := filepath.Glob("../engine/resources/printers/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("could not list native printer definitions: %v", err)
	}
	want := map[string]string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Baseline struct {
				DisplayName string `json:"display_name"`
				ModelID     string `json:"model_id"`
				Print       struct {
					AutoFlow bool `json:"support_auto_flow_calibration"`
				} `json:"print"`
			} `json:"00.00.00.00"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if model := config.Baseline; model.Print.AutoFlow {
			name := strings.TrimPrefix(strings.TrimSpace(model.DisplayName), "Bambu Lab ")
			want[strings.ToLower(name)] = name
			want[strings.ToLower(strings.TrimSpace(model.ModelID))] = name
			if got := bambuModel("", model.ModelID); got != name {
				t.Errorf("model code %q resolved to %q, want %q", model.ModelID, got, name)
			}
		}
	}
	if !maps.Equal(bambuAutoFlowModels, want) {
		t.Fatalf("Bambu Auto flow support changed. Run python3 generate_bambu_auto_flow.py.\nGot %v\nWant %v", bambuAutoFlowModels, want)
	}
}

func TestBambuProjectFileFlowCalibrationOnWire(t *testing.T) {
	for _, tc := range []struct {
		name, model, serial string
		mode                flowCalibrationMode
		legacy              string
		flag                string
	}{
		{"supported off", "H2D", "", flowCalibrationOff, "false", "0"},
		{"supported auto", "H2D", "", flowCalibrationAuto, "false", "2"},
		{"supported on", "H2D", "", flowCalibrationOn, "true", "1"},
		{"legacy off", "P1S", "", flowCalibrationOff, "false", "0"},
		{"legacy auto fallback", "P1S", "", flowCalibrationAuto, "true", "1"},
		{"legacy on", "P1S", "", flowCalibrationOn, "true", "1"},
		{"unknown auto fallback", "", "", flowCalibrationAuto, "true", "1"},
		{"stale model auto fallback", "H2D", "01PFIXTURE", flowCalibrationAuto, "true", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &PrinterConfig{Kind: "bambu-lan", model: tc.model, Serial: tc.serial}
			u := &upload{printer: p, options: bambuOptions{flowCali: tc.mode}, filename: "fixture.gcode.3mf"}
			message, err := u.bambuProjectFileMessage("1", "Metadata/plate_1.gcode", "", "file:///sdcard/fixture.gcode.3mf")
			if err != nil {
				t.Fatal(err)
			}
			var packet bytes.Buffer
			if err := mqttPublish(&packet, p, message); err != nil {
				t.Fatal(err)
			}
			kind, body, err := mqttRead(bufio.NewReader(&packet))
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Print map[string]json.RawMessage `json:"print"`
			}
			if err := json.Unmarshal(mqttPayload(kind, body), &payload); err != nil {
				t.Fatal(err)
			}
			if got := string(payload.Print["flow_cali"]); got != tc.legacy {
				t.Errorf("flow_cali = %s, want %s", got, tc.legacy)
			}
			if got := string(payload.Print["extrude_cali_flag"]); got != tc.flag {
				t.Errorf("extrude_cali_flag = %s, want %s", got, tc.flag)
			}
			if _, ok := payload.Print["auto_flow_cali"]; ok {
				t.Error("unexpected auto_flow_cali field")
			}
		})
	}
}
