package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

const (
	shelfThumbnailSize       = 320
	shelfThumbnailPadding    = 24
	shelfThumbnailPixelLimit = 24_000_000
)

type shelfProjectedTriangle struct {
	points [3]shelfPlanePoint
	depths [3]float64
	bounds image.Rectangle
}

type shelfView struct {
	center meshPoint
	offset shelfPlanePoint
	scale  float64
}

func (v shelfView) project(point meshPoint) (shelfPlanePoint, float64) {
	x, y, z := point[0]-v.center[0], point[1]-v.center[1], point[2]-v.center[2]
	return shelfPlanePoint{
		(x-y)*0.7071067811865476*v.scale + v.offset[0],
		((x+y)*0.408248290463863-z*0.816496580927726)*v.scale + v.offset[1],
	}, x + y + z
}

func (v shelfView) triangle(t shelfTriangle) shelfProjectedTriangle {
	var projected shelfProjectedTriangle
	for i, point := range t {
		projected.points[i], projected.depths[i] = v.project(point)
	}
	a, b, c := projected.points[0], projected.points[1], projected.points[2]
	projected.bounds = image.Rect(
		int(math.Floor(min(a[0], b[0], c[0]))), int(math.Floor(min(a[1], b[1], c[1]))),
		int(math.Ceil(max(a[0], b[0], c[0]))), int(math.Ceil(max(a[1], b[1], c[1]))),
	).Intersect(image.Rect(0, 0, shelfThumbnailSize, shelfThumbnailSize))
	return projected
}

func shelfThumbnailCanvas() *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, shelfThumbnailSize, shelfThumbnailSize))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 242, G: 244, B: 247, A: 255}}, image.Point{}, draw.Src)
	return canvas
}

func encodeShelfThumbnail(canvas *image.RGBA) ([]byte, error) {
	var data bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&data, canvas); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func renderShelfThumbnail(triangles []shelfTriangle) ([]byte, error) {
	bounds, err := shelfMeshBounds(triangles)
	if err != nil {
		return nil, err
	}
	view := shelfView{scale: 1}
	for axis := range view.center {
		view.center[axis] = (bounds.min[axis] + bounds.max[axis]) / 2
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, triangle := range triangles {
		for _, point := range triangle {
			p, _ := view.project(point)
			minX, minY = min(minX, p[0]), min(minY, p[1])
			maxX, maxY = max(maxX, p[0]), max(maxY, p[1])
		}
	}
	span := max(maxX-minX, maxY-minY)
	if span <= 0 {
		return nil, errors.New("model has no visible area")
	}
	view.scale = float64(shelfThumbnailSize-2*shelfThumbnailPadding) / span
	if math.IsInf(view.scale, 0) {
		return nil, errors.New("model has no visible area")
	}
	view.offset = shelfPlanePoint{
		shelfThumbnailSize/2 - (minX+maxX)/2*view.scale,
		shelfThumbnailSize/2 - (minY+maxY)/2*view.scale,
	}
	var estimatedWork int64
	for _, triangle := range triangles {
		box := view.triangle(triangle).bounds
		estimatedWork += int64(box.Dx() * box.Dy())
	}
	stride := int(max(1, (estimatedWork+shelfThumbnailPixelLimit-1)/shelfThumbnailPixelLimit))
	canvas := shelfThumbnailCanvas()
	depths := make([]float64, shelfThumbnailSize*shelfThumbnailSize)
	for i := range depths {
		depths[i] = math.Inf(-1)
	}
	budget := shelfThumbnailPixelLimit
	visible := false
	for i := 0; i < len(triangles); i += stride {
		triangle := triangles[i]
		projected := view.triangle(triangle)
		cost := projected.bounds.Dx() * projected.bounds.Dy()
		if cost > budget {
			continue
		}
		budget -= cost
		if rasterShelfTriangle(canvas, depths, projected, shelfFacetColor(triangle)) {
			visible = true
		}
	}
	if !visible {
		return nil, errors.New("model has no visible area")
	}
	return encodeShelfThumbnail(canvas)
}

func shelfFacetColor(t shelfTriangle) color.RGBA {
	normal := shelfTriangleNormal(t)
	length := math.Hypot(math.Hypot(normal[0], normal[1]), normal[2])
	shade := 0.0
	if length > 0 {
		shade = math.Abs((normal[0]*0.32 + normal[1]*0.38 + normal[2]*0.86) / length)
	}
	return color.RGBA{R: uint8(62 + 88*shade), G: uint8(82 + 94*shade), B: uint8(111 + 93*shade), A: 255}
}

func rasterShelfTriangle(canvas *image.RGBA, depths []float64, t shelfProjectedTriangle, fill color.RGBA) bool {
	a, b, c := t.points[0], t.points[1], t.points[2]
	area := shelfEdge(a, b, c)
	if area == 0 || t.bounds.Empty() {
		return false
	}
	invArea := 1 / area
	visible := false
	for y := t.bounds.Min.Y; y < t.bounds.Max.Y; y++ {
		for x := t.bounds.Min.X; x < t.bounds.Max.X; x++ {
			p := shelfPlanePoint{float64(x) + 0.5, float64(y) + 0.5}
			u := shelfEdge(b, c, p) * invArea
			v := shelfEdge(c, a, p) * invArea
			w := 1 - u - v
			if u < -1e-10 || v < -1e-10 || w < -1e-10 {
				continue
			}
			depth := u*t.depths[0] + v*t.depths[1] + w*t.depths[2]
			index := y*shelfThumbnailSize + x
			if depth <= depths[index] {
				continue
			}
			depths[index] = depth
			canvas.SetRGBA(x, y, fill)
			visible = true
		}
	}
	return visible
}

func shelfPlaceholderThumbnail() ([]byte, error) {
	canvas := shelfThumbnailCanvas()
	stroke := color.RGBA{R: 127, G: 138, B: 155, A: 255}
	for _, line := range [][2]image.Point{
		{{X: 125, Y: 137}, {X: 195, Y: 137}},
		{{X: 195, Y: 137}, {X: 195, Y: 184}},
		{{X: 195, Y: 184}, {X: 125, Y: 184}},
		{{X: 125, Y: 184}, {X: 125, Y: 137}},
		{{X: 126, Y: 175}, {X: 146, Y: 155}},
		{{X: 146, Y: 155}, {X: 163, Y: 172}},
		{{X: 163, Y: 172}, {X: 177, Y: 157}},
		{{X: 177, Y: 157}, {X: 194, Y: 174}},
	} {
		drawShelfThumbnailLine(canvas, line[0], line[1], stroke)
	}
	drawShelfThumbnailLine(canvas, image.Pt(118, 195), image.Pt(203, 125), color.RGBA{R: 72, G: 85, B: 104, A: 255})
	return encodeShelfThumbnail(canvas)
}

func drawShelfThumbnailLine(canvas *image.RGBA, from, to image.Point, stroke color.RGBA) {
	dx, dy := to.X-from.X, to.Y-from.Y
	steps := int(max(math.Abs(float64(dx)), math.Abs(float64(dy))))
	for i := 0; i <= steps; i++ {
		x, y := from.X+dx*i/steps, from.Y+dy*i/steps
		box := image.Rect(x-1, y-1, x+2, y+2)
		draw.Draw(canvas, box, &image.Uniform{C: stroke}, image.Point{}, draw.Src)
	}
}
