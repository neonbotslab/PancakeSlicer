package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestShelfZIPManyFiles(t *testing.T) {
	for _, count := range []int{10000, 10037} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, h := shelfFixture(t)
			entries := map[string][]byte{"model.stl": []byte(shelfTestSTL)}
			for i := 1; i < count; i++ {
				entries[fmt.Sprintf("notes/%d.txt", i)] = []byte("note")
			}
			for i := 0; i < 2100; i++ {
				entries[fmt.Sprintf("empty/%d/", i)] = nil
				entries[fmt.Sprintf("__MACOSX/._%d.stl", i)] = []byte("metadata")
				entries[fmt.Sprintf(".hidden/%d.stl", i)] = []byte("metadata")
			}
			w := shelfUpload(t, h, 0, "", map[string][]byte{"many.zip": shelfZIP(t, entries)})
			if count == 10000 {
				shelfRequireStatus(t, w, http.StatusCreated)
				d, err := s.shelfSnapshot()
				if err != nil || len(d.Models) != 1 || d.Revision != 1 {
					t.Fatalf("imported shelf %+v, error %v", d, err)
				}
				for _, folder := range d.Folders {
					if strings.HasPrefix(folder.Name, ".") || folder.Name == "__MACOSX" {
						t.Fatalf("imported metadata folder %+v", folder)
					}
				}
				return
			}
			shelfRequireStatus(t, w, http.StatusRequestEntityTooLarge)
			var failure struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Code != "zip_entry_limit" || !strings.Contains(failure.Error, "10037") || !strings.Contains(failure.Error, "10000") {
				t.Fatalf("missing file count or limit in %s", w.Body)
			}
			d, err := s.shelfSnapshot()
			if err != nil || len(d.Models) != 0 || len(d.Folders) != 0 || d.Revision != 0 {
				t.Fatalf("partially imported shelf %+v, error %v", d, err)
			}
		})
	}
}
