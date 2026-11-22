package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type bambuReportField struct {
	typ    reflect.Type
	fields map[string]*bambuReportField
	item   *bambuReportField
}

var bambuRawNumberType = reflect.TypeFor[json.RawMessage]()

var bambuReportFields = func() *bambuReportField {
	report := newBambuReportField(reflect.TypeFor[bambuReport]())
	report.fields["vt_tray"] = newBambuReportField(reflect.TypeFor[bambuExternalTray]())
	return report
}()

func newBambuReportField(typ reflect.Type) *bambuReportField {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	field := &bambuReportField{typ: typ}
	switch typ.Kind() {
	case reflect.Struct:
		field.fields = make(map[string]*bambuReportField, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			member := typ.Field(i)
			field.fields[member.Tag.Get("json")] = newBambuReportField(member.Type)
		}
	case reflect.Slice:
		if typ != bambuRawNumberType {
			field.item = newBambuReportField(typ.Elem())
		}
	}
	return field
}

func decodeBambuReport(state map[string]any, payload []byte) *bambuReport {
	report, _, _ := decodeBambuReportUpdate(state, payload)
	return report
}

func decodeBambuReportUpdate(state map[string]any, payload []byte) (*bambuReport, bambuHeard, string) {
	if payload == nil {
		return nil, bambuHeard{}, ""
	}
	var message struct {
		Print json.RawMessage `json:"print"`
	}
	if json.Unmarshal(payload, &message) != nil {
		return nil, bambuHeard{}, "invalid_json"
	}
	if len(message.Print) == 0 || string(message.Print) == "null" {
		return nil, bambuHeard{}, ""
	}
	var delta map[string]any
	decoder := json.NewDecoder(bytes.NewReader(message.Print))
	decoder.UseNumber()
	if decoder.Decode(&delta) != nil {
		return nil, bambuHeard{}, "invalid_print"
	}

	var issue string
	filtered, keep := filterBambuReportValue(delta, bambuReportFields, "", &issue)
	if !keep {
		return nil, bambuHeard{}, issue
	}
	delta = filtered.(map[string]any)
	candidate := maps.Clone(state)
	mergeReport(candidate, delta)
	raw, err := json.Marshal(candidate)
	if err != nil {
		return nil, bambuHeard{}, "invalid_print"
	}
	var report bambuReport
	if json.Unmarshal(raw, &report) != nil {
		return nil, bambuHeard{}, "invalid_print"
	}

	pruneAbsentBambuInventory(candidate)
	clear(state)
	maps.Copy(state, candidate)
	heard := bambuHeard{full: delta["msg"] == json.Number("0")}
	for key := range delta {
		if key != "command" && key != "msg" {
			heard.telemetry = true
		}
	}
	if ams, ok := delta["ams"].(map[string]any); ok {
		_, heard.trays = ams["ams"].([]any)
	}
	if command, _ := delta["command"].(string); command != "" && command != "push_status" {
		echo := new(bambuEcho)
		if json.Unmarshal(message.Print, echo) == nil {
			heard.echo = echo
		}
	}
	return &report, heard, issue
}

func filterBambuReportValue(value any, field *bambuReportField, path string, issue *string) (any, bool) {
	if field == nil || value == nil {
		return value, true
	}
	if fields, ok := value.(map[string]any); ok && field.fields != nil {
		empty := len(fields) == 0
		cleaned := make(map[string]any, len(fields))
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			name, member := key, field.fields[key]
			if member == nil {
				for canonical, candidate := range field.fields {
					if strings.EqualFold(key, canonical) {
						name, member = canonical, candidate
						break
					}
				}
			}
			if member == nil {
				continue
			}
			if _, exact := fields[name]; exact && name != key {
				continue
			}
			updated, keep := filterBambuReportValue(fields[key], member, path+name+".", issue)
			if !keep {
				*issue = "invalid_field_type"
			} else {
				cleaned[name] = updated
			}
		}
		return cleaned, empty || len(cleaned) > 0
	}
	if items, ok := value.([]any); ok && field.item != nil {
		valid := items[:0]
		keyed := bambuKeyedReportRows(strings.TrimSuffix(path, "."))
		for _, item := range items {
			updated, keep := filterBambuReportValue(item, field.item, path, issue)
			if keyed {
				row, _ := updated.(map[string]any)
				keep = keep && row["id"] != nil
			}
			if keep {
				valid = append(valid, updated)
			} else {
				*issue = "invalid_field_type"
			}
		}
		return valid, len(items) == 0 || len(valid) > 0
	}
	if validBambuReportScalar(value, field.typ) {
		if number, ok := value.(json.Number); ok {
			switch field.typ.Kind() {
			case reflect.Int, reflect.Int64:
				integer, _ := strconv.ParseInt(string(number), 10, field.typ.Bits())
				return json.Number(strconv.FormatInt(integer, 10)), true
			case reflect.Uint32:
				integer, _ := strconv.ParseUint(string(number), 10, field.typ.Bits())
				return json.Number(strconv.FormatUint(integer, 10)), true
			}
		}
		return value, true
	}
	return nil, false
}

func validBambuReportScalar(value any, typ reflect.Type) bool {
	if typ == bambuRawNumberType {
		switch value.(type) {
		case string, json.Number:
			return true
		default:
			return false
		}
	}
	switch typ.Kind() {
	case reflect.String:
		_, ok := value.(string)
		return ok
	case reflect.Bool:
		_, ok := value.(bool)
		return ok
	}
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	var err error
	switch typ.Kind() {
	case reflect.Int, reflect.Int64:
		_, err = strconv.ParseInt(string(number), 10, typ.Bits())
	case reflect.Uint32:
		_, err = strconv.ParseUint(string(number), 10, typ.Bits())
	case reflect.Float64:
		_, err = strconv.ParseFloat(string(number), typ.Bits())
	default:
		return false
	}
	return err == nil
}
