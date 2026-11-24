package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestRepetierSlug(t *testing.T) {
	store := &printerStore{}
	sp := storedPrinter{ID: "0123456789abcdef", Kind: "repetier", URL: "http://printer.example:3344"}
	for _, slug := range []string{"Printer_1", "printer-2.1~lab", strings.Repeat("x", 200)} {
		sp.Slug = slug
		p, err := store.config(sp)
		if err != nil || p.Slug != slug {
			t.Errorf("config did not preserve valid slug %q: %v, %v", slug, p, err)
		}
	}

	base, err := url.Parse(sp.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{
		"", ".", "..", "model/other", `model\other`, "%2e%2e", "printer%2Fother", "printer%252Fother",
		" printer", "printer ", "printer\tother", "printer\u00a0other", "printer\n", "printer?other", "printer#other", strings.Repeat("x", 201),
	} {
		sp.Slug = slug
		if _, err := store.config(sp); err == nil {
			t.Errorf("config accepted invalid slug %q", slug)
		}
		u := &upload{printer: &PrinterConfig{Slug: slug}}
		if req, err := repetierUpload(context.Background(), base, u, nil); req != nil || err == nil {
			t.Errorf("upload accepted invalid slug %q", slug)
		}
	}
}

func TestRepetierStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		response string
		wantErr  bool
	}{
		{"server reachable with offline printer", http.StatusOK, `{"software":"Repetier-Server","printers":[{"slug":"printer","online":0}]}`, false},
		{"server reachable with busy printer", http.StatusOK, `{"software":"Repetier-Server","printers":[{"slug":"printer","online":1,"job":"part.gcode"}]}`, false},
		{"wrong software", http.StatusOK, `{"software":"OctoPrint"}`, true},
		{"rejected key", http.StatusForbidden, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/printer/info" || r.Header.Get("X-Api-Key") != "test-key" {
					t.Error("expected an authenticated GET to /printer/info")
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			h := &hostSession{client: server.Client(), printer: &PrinterConfig{Kind: "repetier", Slug: "printer"}, secret: "test-key"}
			st, err := statusHTTP(context.Background(), h, base)
			if tc.wantErr {
				if err == nil || st.Online {
					t.Fatalf("failed probe returned status %+v, error %v", st, err)
				}
				return
			}
			if err != nil || !st.Online || st.State != "unknown" {
				t.Fatalf("reachable server returned status %+v, error %v", st, err)
			}
		})
	}
}

func TestRepetierUpload(t *testing.T) {
	base, err := url.Parse("http://printer.example:3344")
	if err != nil {
		t.Fatal(err)
	}
	const filename, gcode = "part.gcode", "G28\n"
	for _, tc := range []struct {
		name   string
		start  bool
		path   string
		fields url.Values
	}{
		{"upload model", false, "/printer/model/Printer_1.2", url.Values{"a": {"upload"}}},
		{"upload job with autostart", true, "/printer/job/Printer_1.2", url.Values{"a": {"upload"}, "name": {filename}, "autostart": {"true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &upload{printer: &PrinterConfig{Slug: "Printer_1.2"}, filename: filename, size: int64(len(gcode)), start: tc.start}
			req, err := repetierUpload(context.Background(), base, u, strings.NewReader(gcode))
			if err != nil {
				t.Fatal(err)
			}
			if req.Method != http.MethodPost || req.URL.RequestURI() != tc.path {
				t.Errorf("request = %s %s, want POST %s", req.Method, req.URL.RequestURI(), tc.path)
			}
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(url.Values(req.MultipartForm.Value), tc.fields) {
				t.Errorf("form fields = %v, want %v", req.MultipartForm.Value, tc.fields)
			}
			file, header, err := req.FormFile("filename")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			body, err := io.ReadAll(file)
			if err != nil || header.Filename != filename || string(body) != gcode {
				t.Errorf("uploaded file = %q %q, error %v", header.Filename, body, err)
			}
		})
	}
}
