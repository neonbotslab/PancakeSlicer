package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

type settingOption struct {
	Type       string   `json:"type"`
	Min        *float64 `json:"min"`
	Max        *float64 `json:"max"`
	EnumValues []string `json:"enum_values"`
	Nullable   bool     `json:"nullable"`
	Readonly   bool     `json:"readonly"`
}

var settingVectorTypes = map[string]string{
	"bools": "bool", "enums": "enum", "strings": "string", "ints": "int",
	"floats": "float", "percents": "percent", "floatsorpercents": "floatorpercent",
}

var settingDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func validateSettingChanges(dictionary []byte, catalog *resolve.Catalog, kind, model, section string, changes map[string]json.RawMessage) error {
	if catalog == nil {
		return errors.New("settings catalog is unavailable")
	}
	var allowed map[string]bool
	switch {
	case kind == "process" && section == "values", kind == "printer" && section == "printOverrides":
		allowed = catalog.PrintSettings()
	case kind == "printer" && section == "machine":
		allowed = catalog.PrinterSettings(model)
	case kind == "filament" && section == "settings":
		allowed = catalog.FilamentSettings()
	default:
		return fmt.Errorf("invalid settings section %q for %s profiles", section, kind)
	}
	var options map[string]settingOption
	if err := json.Unmarshal(dictionary, &options); err != nil || len(options) == 0 {
		return errors.New("settings option metadata is unavailable")
	}
	for _, key := range sortedKeys(changes) {
		if blockedSettingChange(key) {
			return fmt.Errorf("setting %q is blocked", key)
		}
		option := options[key]
		if !settingKey.MatchString(key) || !allowed[key] || option.Type == "" ||
			slices.Contains(resolve.HardwareKeys, key) ||
			(section == "printOverrides" && key == "layer_height") ||
			(kind == "filament" && slices.Contains(resolve.ColourKeys, key)) {
			return fmt.Errorf("unknown setting %q for %s %s", key, kind, section)
		}
		if option.Readonly {
			return fmt.Errorf("setting %q is read-only", key)
		}
		if err := option.validate(changes[key]); err != nil {
			return fmt.Errorf("setting %q: %w", key, err)
		}
	}
	return nil
}

func blockedSettingChange(key string) bool {
	key = strings.ToLower(key)
	if resolve.HiddenSetting(key) {
		return true
	}
	compact := strings.ReplaceAll(key, "_", "")
	for _, part := range []string{
		"accesscode", "apikey", "auth", "bearer", "connection", "credential", "hostname", "ipaddress",
		"lanmode", "lanonly", "lansettings", "network", "password", "printerhost", "printerip", "printerport",
		"printerurl", "serialnumber", "token", "username", "wifi",
	} {
		if strings.Contains(compact, part) {
			return true
		}
	}
	for word := range strings.SplitSeq(key, "_") {
		switch word {
		case "host", "ip", "lan", "mqtt", "port", "proxy", "serial", "ssid", "ssl", "tls", "url":
			return true
		}
	}
	return key == "key" || key == "clear_key"
}

func (option settingOption) validate(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("expected a JSON setting value")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return errors.New("expected a JSON setting value")
	}
	if value == nil {
		return nil
	}
	kind := strings.ReplaceAll(strings.ToLower(option.Type), "_", "")
	elementType, vector := settingVectorTypes[kind]
	if !vector {
		return option.validateScalar(kind, false, value)
	}
	switch value := value.(type) {
	case []any:
		if len(value) == 0 && elementType != "string" {
			return errors.New("expected a nonempty flat list")
		}
		for i, item := range value {
			if err := option.validateScalar(elementType, true, item); err != nil {
				return fmt.Errorf("list item %d: %w", i+1, err)
			}
		}
	case string:
		if elementType == "string" {
			return validateSerializedSettingStrings(value)
		}
		if _, err := resolve.ScalarConfigValue(value); err != nil {
			return err
		}
		for i, item := range strings.Split(value, ",") {
			if err := option.validateScalar(elementType, true, item); err != nil {
				return fmt.Errorf("list item %d: %w", i+1, err)
			}
		}
	default:
		return option.validateScalar(elementType, true, value)
	}
	return nil
}

func (option settingOption) validateScalar(kind string, vector bool, value any) error {
	text, err := resolve.ScalarConfigValue(value)
	if err != nil {
		return err
	}
	_, isString := value.(string)
	if kind == "string" {
		if !isString {
			return errors.New("expected text")
		}
		return nil
	}
	if strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return errors.New("setting contains invalid control characters")
	}
	if kind != "enum" || vector {
		text = strings.Trim(text, " ")
	}
	if vector && option.Nullable && isString && text == "nil" {
		return nil
	}
	switch kind {
	case "bool":
		if text != "0" && text != "1" {
			return errors.New("expected a boolean or serialized 0 or 1")
		}
	case "enum":
		if !isString || !slices.Contains(option.EnumValues, text) {
			return errors.New("expected one of the engine's enum values")
		}
	case "int", "float", "percent", "floatorpercent":
		if _, isBool := value.(bool); isBool {
			return errors.New("expected a number")
		}
		return option.validateNumber(kind, text)
	default:
		return fmt.Errorf("unsupported setting type %q", option.Type)
	}
	return nil
}

func (option settingOption) validateNumber(kind, text string) error {
	var n float64
	if kind == "int" {
		integer, err := strconv.ParseInt(text, 10, 32)
		if err != nil {
			return errors.New("expected an integer")
		}
		n = float64(integer)
	} else {
		if kind == "percent" || kind == "floatorpercent" {
			text = strings.TrimRight(strings.TrimSuffix(text, "%"), " ")
		}
		var err error
		n, err = strconv.ParseFloat(text, 64)
		if err != nil || !settingDecimal.MatchString(text) {
			return fmt.Errorf("expected a number of type %s", option.Type)
		}
	}
	if option.Min != nil && n < *option.Min {
		return fmt.Errorf("value must be at least %g", *option.Min)
	}
	if option.Max != nil && n > *option.Max {
		return fmt.Errorf("value must be at most %g", *option.Max)
	}
	return nil
}

func validateSerializedSettingStrings(text string) error {
	if _, err := resolve.ScalarConfigValue(text); err != nil {
		return err
	}
	invalid := errors.New("invalid serialized string list")
	for text = strings.TrimLeft(text, " \t"); text != ""; text = strings.TrimLeft(text, " \t") {
		if text[0] == '"' {
			rest := text[1:]
			for {
				end := strings.IndexAny(rest, `\"`)
				if end < 0 {
					return invalid
				}
				if rest[end] == '"' {
					text = strings.TrimLeft(rest[end+1:], " \t")
					break
				}
				if end+1 == len(rest) {
					return invalid
				}
				rest = rest[end+2:]
			}
			if text != "" && text[0] != ';' {
				return invalid
			}
		}
		_, text, _ = strings.Cut(text, ";")
	}
	return nil
}
