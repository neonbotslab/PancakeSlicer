package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestShelfThumbnailRendersVisibleGeometry(t *testing.T) {
	front := shelfTriangle{{0, 0, 0}, {16, 0, 0}, {0, 18, 0}}
	side := shelfTriangle{{0, 0, 0}, {0, 0, 12}, {16, 0, 0}}
	data, err := renderShelfThumbnail([]shelfTriangle{front, side})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if preview.Bounds() != image.Rect(0, 0, 320, 320) {
		t.Fatalf("preview dimensions %s", preview.Bounds())
	}
	background := preview.At(0, 0)
	colors := map[color.Color]int{}
	for y := 0; y < 320; y++ {
		for x := 0; x < 320; x++ {
			pixel := preview.At(x, y)
			if pixel != background {
				if x < 20 || y < 20 || x >= 300 || y >= 300 {
					t.Fatalf("geometry too close to the image edge at %d,%d", x, y)
				}
				colors[pixel]++
			}
		}
	}
	if len(colors) < 2 {
		t.Fatalf("expected visible facets with different shading, got %v", colors)
	}
}

func TestShelfThumbnailResolvesOcclusionRegardlessOfTriangleOrder(t *testing.T) {
	far := shelfTriangle{{0, 0, -10}, {0, 10, 10}, {0, -10, 10}}
	near := shelfTriangle{{100, 100, 100}, {110, 100, 100}, {100, 110, 100}}
	data, err := renderShelfThumbnail([]shelfTriangle{far, near})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := renderShelfThumbnail([]shelfTriangle{near, far})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, reversed) {
		t.Fatal("triangle draw order changed the preview")
	}
	preview, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	overlap := color.RGBAModel.Convert(preview.At(147, 214))
	if overlap != (color.RGBA{R: 137, G: 162, B: 190, A: 255}) {
		t.Fatalf("nearer facet was not visible in the overlap: %v", overlap)
	}
}

func TestShelfThumbnailPlaceholderAndEmptyGeometry(t *testing.T) {
	if data, err := renderShelfThumbnail(nil); err == nil || data != nil {
		t.Fatalf("empty geometry returned a preview and error %v", err)
	}
	data, err := shelfPlaceholderThumbnail()
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if placeholder.Bounds() != image.Rect(0, 0, 320, 320) {
		t.Fatalf("placeholder dimensions %s", placeholder.Bounds())
	}
	if placeholder.At(160, 160) == placeholder.At(0, 0) {
		t.Fatal("missing preview mark is not visible in the image")
	}
}
