package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestShelfSTEPWithNativeEngine(t *testing.T) {
	tool, resources := os.Getenv("SHELF_REAL_TOOL"), os.Getenv("SHELF_REAL_RESOURCES")
	if tool == "" || resources == "" {
		t.Skip("Set SHELF_REAL_TOOL and SHELF_REAL_RESOURCES to check STEP with the native engine.")
	}
	for _, filename := range []string{"security_classification.step", "utf8_part_names.step", "cone_tilted_seam_pcurve.step"} {
		t.Run(filename, func(t *testing.T) {
			s, _ := shelfFixture(t)
			s.cfg.Tool, s.cfg.Resources = tool, resources
			h := shelfWorkspaceFixture(t, s)
			contents, err := os.ReadFile(filepath.Join("..", "engine", "tests", "data", filename))
			if err != nil {
				t.Fatal(err)
			}
			upload := shelfUpload(t, h, 0, "", map[string][]byte{filename: contents})
			shelfRequireStatus(t, upload, http.StatusCreated)
			var report struct{ Imported []shelfImported }
			if json.Unmarshal(upload.Body.Bytes(), &report) != nil || len(report.Imported) != 1 || report.Imported[0].Preview != "rendered" {
				t.Fatalf("native STEP preview failed: %s", upload.Body)
			}
			model := report.Imported[0].Model
			thumbnail := shelfJSON(t, h, http.MethodGet, "/api/shelf/models/"+model.ID+"/thumbnail.png", nil, nil)
			shelfRequireStatus(t, thumbnail, http.StatusOK)
			image, err := png.Decode(bytes.NewReader(thumbnail.Body.Bytes()))
			if err != nil || image.Bounds().Dx() != 320 || image.Bounds().Dy() != 320 {
				t.Fatalf("invalid native STEP thumbnail: %v", err)
			}
			if evidence := os.Getenv("SHELF_EVIDENCE_DIR"); evidence != "" {
				if err := os.WriteFile(filepath.Join(evidence, filename+".png"), thumbnail.Body.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
			geometry := shelfJSON(t, h, http.MethodGet, "/api/shelf/models/"+model.ID+"/geometry.stl?revision=1", nil, nil)
			shelfRequireStatus(t, geometry, http.StatusOK)
			if geometry.Body.Len() <= 84 {
				t.Fatal("STEP conversion returned no triangles")
			}
			shelfCreateProject(t, h, shelfTestProject, json.RawMessage(`{"meta":{"name":"STEP fixture","savedAt":1,"objects":0},"doc":{"objects":[],"dirty":{"machine":{"printable_area":"0x0,5000x0,5000x5000,0x5000","printable_height":5000}}}}`))
			request := shelfTestPlacementRequest(1, "native-step-first")
			placed := shelfProjectJSON(t, h, http.MethodPost, "/api/shelf/models/"+model.ID+"/add-to-plate", shelfTestSession, request)
			shelfRequireStatus(t, placed, http.StatusOK)
			var result shelfTestPlacement
			if json.Unmarshal(placed.Body.Bytes(), &result) != nil {
				t.Fatal("invalid native STEP placement result")
			}
			asset := shelfJSON(t, h, http.MethodGet, "/api/assets/"+result.Source, nil, nil)
			shelfRequireStatus(t, asset, http.StatusOK)
			if !bytes.Equal(geometry.Body.Bytes(), asset.Body.Bytes()) {
				t.Fatal("placed STEP copy differs from the canonical geometry")
			}
			t.Logf("rendered 320x320 preview and placed %d canonical STL bytes", geometry.Body.Len())
		})
	}
}
