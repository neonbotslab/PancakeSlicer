package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDevicePreferences(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	routes := func() *http.ServeMux {
		mux := http.NewServeMux()
		(&workspaceStore{root: root}).register(mux)
		return mux
	}
	check := func(mux *http.ServeMux, method, body string, status int, revision uint64, open bool, delay int, custom string) {
		t.Helper()
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(method, "/api/preferences", strings.NewReader(body)))
		var got struct {
			Revision uint64 `json:"revision"`
			Document struct {
				Open   *bool           `json:"orca.web.openDeviceAfterPrint"`
				Delay  *int            `json:"orca.web.openDeviceDelay"`
				Custom json.RawMessage `json:"orca.example"`
			} `json:"document"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || res.Code != status || got.Revision != revision ||
			got.Document.Open == nil || *got.Document.Open != open || got.Document.Delay == nil || *got.Document.Delay != delay || string(got.Document.Custom) != custom {
			t.Fatalf("%s preferences = %d %s, want HTTP %d, revision %d, open %t, delay %d, custom %s", method, res.Code, res.Body, status, revision, open, delay, custom)
		}
	}
	check(routes(), http.MethodGet, "", http.StatusOK, 0, true, 5, "")
	legacy := newWorkspaceData()
	legacy.Values["preferences"] = workspaceValue{Revision: 7, Document: json.RawMessage(`{"orca.example":{"keep":[1,2]}}`)}
	if err := (&workspaceStore{root: root}).save(legacy); err != nil {
		t.Fatal(err)
	}
	const custom = `{"keep":[1,2]}`
	mux := routes()
	check(mux, http.MethodGet, "", http.StatusOK, 8, true, 5, custom)
	check(routes(), http.MethodGet, "", http.StatusOK, 8, true, 5, custom)
	for _, tt := range []struct {
		name, open, delay string
		wantOpen          bool
		wantDelay         int
	}{
		{"disabled and zero", `false`, `0`, false, 0},
		{"upper bound", `true`, `99`, true, 60},
		{"lower bound", `false`, `-1`, false, 0},
		{"fractional", `true`, `6.5`, true, 7},
		{"invalid types", `"false"`, `"18"`, true, 5},
		{"null", `null`, `null`, true, 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
			var current workspaceValue
			if err := json.Unmarshal(res.Body.Bytes(), &current); err != nil || res.Code != http.StatusOK {
				t.Fatalf("load current revision = %d %s", res.Code, res.Body)
			}
			body := fmt.Sprintf(`{"operationId":"setting-%d","expectedRevision":%d,"document":{"orca.web.openDeviceAfterPrint":%s,"orca.web.openDeviceDelay":%s,"orca.example":%s}}`,
				current.Revision, current.Revision, tt.open, tt.delay, custom)
			check(mux, http.MethodPut, body, http.StatusOK, current.Revision+1, tt.wantOpen, tt.wantDelay, custom)
			check(routes(), http.MethodPut, body, http.StatusOK, current.Revision+1, tt.wantOpen, tt.wantDelay, custom)
			check(routes(), http.MethodGet, "", http.StatusOK, current.Revision+1, tt.wantOpen, tt.wantDelay, custom)
		})
	}
	check(routes(), http.MethodPut, `{"operationId":"stale","expectedRevision":7,"document":{}}`, http.StatusConflict, 14, true, 5, custom)
}
