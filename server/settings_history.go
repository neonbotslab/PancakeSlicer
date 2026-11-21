package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"
)

type settingsRevision struct {
	ID       string          `json:"id"`
	Revision uint64          `json:"revision"`
	At       time.Time       `json:"at"`
	Document json.RawMessage `json:"document"`
}

type settingsChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

func appendSettingsRevision(history []settingsRevision, revision uint64, document json.RawMessage) []settingsRevision {
	if len(history) > 0 && history[len(history)-1].Revision == revision {
		return history
	}
	out := append(slices.Clone(history), settingsRevision{ID: newID("revision"), Revision: revision, At: time.Now().UTC(), Document: slices.Clone(document)})
	if len(out) > 64 {
		out = slices.Clone(out[len(out)-64:])
	}
	return out
}

func settingsJSON(value any) any {
	var out any
	json.Unmarshal(must(json.Marshal(value)), &out)
	return out
}

func settingsDiff(before, after any) []settingsChange {
	out := []settingsChange{}
	var walk func(string, any, any)
	walk = func(path string, a, b any) {
		if reflect.DeepEqual(a, b) {
			return
		}
		am, aok := a.(map[string]any)
		bm, bok := b.(map[string]any)
		if aok && bok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			for _, k := range sortedKeys(keys) {
				part := strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
				walk(path+"/"+part, am[k], bm[k])
			}
			return
		}
		out = append(out, settingsChange{Path: path, Before: a, After: b})
	}
	walk("", settingsJSON(before), settingsJSON(after))
	return out
}
