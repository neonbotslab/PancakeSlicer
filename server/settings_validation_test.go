package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

func TestValidateSettingChanges(t *testing.T) {
	const header = `{"schema":"pancake.catalog.v1","version":"settings-fixture"}` + "\n"
	catalog, err := resolve.LoadCatalog(
		strings.NewReader(header+`{"id":"TestPrinter","base":{"printable_height":"100","printable_area":"0x0,100x0,100x100,0x100","nozzle_diameter":"0.4","access_code":"","network_host":"","lan_mode":"0","printerIP":"","printer_connection":"","printhost_apikey":""}}`+"\n"+
			`{"id":"OtherPrinter","base":{"machine_max_speed_x":"200"}}`),
		strings.NewReader(header+`{"id":"TestFilament","base":{"nozzle_temperature":"210","filament_retraction_length":"nil","filament_type":"PLA","filament_colour":"#FFFFFF"}}`),
		strings.NewReader(header+`{"id":"TestProcess","base":{"sparse_infill_density":"15%","sparse_infill_pattern":"grid","wall_loops":"2","layer_height":"0.2","enable_support":"0","read_only":"0","catalog_only":"0","bearer_token":"","auth_mode":""}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	dictionary := []byte(`{
		"sparse_infill_density":{"type":"Percent","min":0,"max":100},
		"sparse_infill_pattern":{"type":"Enum","enum_values":["grid","gyroid"]},
		"wall_loops":{"type":"Int","min":0,"max":12,"enum_values":["0","1","2"]},
		"layer_height":{"type":"Float","min":0.01,"max":1},
		"enable_support":{"type":"Bool"},
		"read_only":{"type":"Bool","readonly":true},
		"metadata_only":{"type":"Int"},
		"printable_height":{"type":"Float","min":10,"max":1000},
		"printable_area":{"type":"Points"},
		"nozzle_diameter":{"type":"Floats","min":0.1,"max":2},
		"machine_max_speed_x":{"type":"Ints","min":1,"max":1000},
		"nozzle_temperature":{"type":"Ints","min":0,"max":350},
		"filament_retraction_length":{"type":"Floats","min":0,"max":10,"nullable":true},
		"filament_type":{"type":"Strings","enum_values":["PLA","PETG"]},
		"filament_colour":{"type":"Strings"},
		"bearer_token":{"type":"String"},
		"auth_mode":{"type":"String"},
		"access_code":{"type":"String"},
		"network_host":{"type":"String"},
		"lan_mode":{"type":"Bool"},
		"printerIP":{"type":"String"},
		"printer_connection":{"type":"String"},
		"printhost_apikey":{"type":"String"}
	}`)
	for _, tc := range []struct {
		name, kind, model, section, key, raw, wantErr string
	}{
		{"valid percent", "process", "", "values", "sparse_infill_density", `"25%"`, ""},
		{"valid integer", "process", "", "values", "wall_loops", `3`, ""},
		{"valid enum", "process", "", "values", "sparse_infill_pattern", `"gyroid"`, ""},
		{"valid native boolean", "process", "", "values", "enable_support", `true`, ""},
		{"valid serialized boolean", "process", "", "values", "enable_support", `"0"`, ""},
		{"valid printer setting", "printer", "TestPrinter", "machine", "printable_height", `120`, ""},
		{"valid print override", "printer", "TestPrinter", "printOverrides", "wall_loops", `"4"`, ""},
		{"valid numeric vector singleton", "filament", "", "settings", "nozzle_temperature", `210`, ""},
		{"valid numeric list", "filament", "", "settings", "nozzle_temperature", `[210, "215"]`, ""},
		{"valid nullable vector entry", "filament", "", "settings", "filament_retraction_length", `"nil,0.75"`, ""},
		{"valid string list", "filament", "", "settings", "filament_type", `["Custom PLA", "PETG"]`, ""},
		{"valid serialized string list", "filament", "", "settings", "filament_type", `"\"Custom; PLA\";PETG"`, ""},
		{"valid override removal", "process", "", "values", "wall_loops", `null`, ""},
		{"invalid range low", "process", "", "values", "layer_height", `0`, "at least"},
		{"invalid range high", "process", "", "values", "sparse_infill_density", `"105%"`, "at most"},
		{"invalid float suffix", "process", "", "values", "layer_height", `"0.2%"`, "expected a number"},
		{"invalid nondecimal float", "process", "", "values", "layer_height", `"0x1p-2"`, "expected a number"},
		{"invalid partial number", "process", "", "values", "layer_height", `"0.2mm"`, "expected a number"},
		{"invalid boolean", "process", "", "values", "enable_support", `"maybe"`, "expected a boolean"},
		{"invalid fractional integer", "process", "", "values", "wall_loops", `2.5`, "expected an integer"},
		{"invalid numeric enum", "process", "", "values", "sparse_infill_pattern", `1`, "enum values"},
		{"invalid enum label", "process", "", "values", "sparse_infill_pattern", `"Gyroid"`, "enum values"},
		{"invalid vector range", "filament", "", "settings", "nozzle_temperature", `"210,500"`, "list item 2"},
		{"invalid vector shape", "filament", "", "settings", "nozzle_temperature", `[[210]]`, "list item 1"},
		{"invalid object shape", "process", "", "values", "layer_height", `{"value":0.2}`, "expected a string"},
		{"invalid vector null marker", "filament", "", "settings", "nozzle_temperature", `"210,nil"`, "list item 2"},
		{"invalid serialized string list", "filament", "", "settings", "filament_type", `"\"PLA;PETG"`, "invalid serialized string list"},
		{"invalid trailing JSON", "process", "", "values", "layer_height", `0.2 true`, "expected a JSON setting value"},
		{"unknown key", "process", "", "values", "missing", `1`, "unknown setting"},
		{"unknown catalog key", "process", "", "values", "metadata_only", `1`, "unknown setting"},
		{"unknown metadata key", "process", "", "values", "catalog_only", `1`, "unknown setting"},
		{"unknown override removal", "process", "", "values", "missing", `null`, "unknown setting"},
		{"wrong printer model", "printer", "TestPrinter", "machine", "machine_max_speed_x", `[100]`, "unknown setting"},
		{"wrong record owner", "process", "", "values", "nozzle_temperature", `[180]`, "unknown setting"},
		{"wrong record section", "filament", "", "values", "filament_type", `["PLA"]`, "invalid settings section"},
		{"excluded hardware", "printer", "TestPrinter", "machine", "nozzle_diameter", `[0.6]`, "unknown setting"},
		{"excluded layer height override", "printer", "TestPrinter", "printOverrides", "layer_height", `0.3`, "unknown setting"},
		{"excluded filament colour", "filament", "", "settings", "filament_colour", `["#000000"]`, "unknown setting"},
		{"unsupported points type", "printer", "TestPrinter", "machine", "printable_area", `"0x0,10x10"`, "unsupported setting type"},
		{"read-only setting", "process", "", "values", "read_only", `false`, "read-only"},
		{"blocked bearer token", "process", "", "values", "bearer_token", `"fixture"`, "blocked"},
		{"blocked auth setting", "process", "", "values", "auth_mode", `null`, "blocked"},
		{"blocked access code", "printer", "TestPrinter", "machine", "access_code", `"fixture"`, "blocked"},
		{"blocked network host", "printer", "TestPrinter", "machine", "network_host", `"fixture"`, "blocked"},
		{"blocked LAN mode", "printer", "TestPrinter", "machine", "lan_mode", `false`, "blocked"},
		{"blocked printer IP", "printer", "TestPrinter", "machine", "printerIP", `"fixture"`, "blocked"},
		{"blocked connection", "printer", "TestPrinter", "machine", "printer_connection", `"fixture"`, "blocked"},
		{"blocked print host API key", "printer", "TestPrinter", "machine", "printhost_apikey", `"fixture"`, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSettingChanges(dictionary, catalog, tc.kind, tc.model, tc.section, map[string]json.RawMessage{tc.key: json.RawMessage(tc.raw)})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr))) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
