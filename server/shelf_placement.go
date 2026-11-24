package main

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/OrcaSlicer/OrcaSlicer/server/resolve"
)

const (
	shelfPlacementGap     = 3
	shelfPlacementEpsilon = 1e-6
)

type shelfFootprint struct {
	min, max shelfPlanePoint
}

type shelfBed struct {
	polygon  []shelfPlanePoint
	bounds   shelfFootprint
	excluded []shelfPlanePoint
}

func (s *Server) shelfPlateConfig(doc map[string]json.RawMessage) map[string]json.RawMessage {
	config := make(map[string]json.RawMessage)
	var dirty struct{ Machine map[string]json.RawMessage }
	json.Unmarshal(doc["dirty"], &dirty)
	var imported struct{ Machine map[string]json.RawMessage }
	json.Unmarshal(doc["projectPresets"], &imported)
	printerID := documentPrinter(doc, "")
	if printerID == "" {
		maps.Copy(config, imported.Machine)
		maps.Copy(config, dirty.Machine)
		return config
	}
	var snapshots struct{ Printer *resolve.Printer }
	json.Unmarshal(doc["profileSnapshots"], &snapshots)
	printer := snapshots.Printer
	if (printer == nil || printer.ID != printerID) && s.library != nil {
		if library, err := s.libraryDocument(); err == nil {
			printer = library.Printers[printerID]
		}
	}
	variant := printerID
	if printer != nil && printer.ID == printerID {
		if len(printer.Nozzles) > 0 {
			variant = printer.Nozzles[0].Variant
		}
	}
	if s.catalog != nil {
		if machine := s.catalog.Machine(variant); machine != nil {
			maps.Copy(config, machine.Config)
		}
	}
	if printer != nil && printer.ID == printerID {
		maps.Copy(config, printer.Machine)
	}
	maps.Copy(config, imported.Machine)
	maps.Copy(config, dirty.Machine)
	return config
}

func shelfBedPolygon(area json.RawMessage) []shelfPlanePoint {
	values := resolve.ConfigList(area)
	if len(values) == 1 {
		values = strings.Split(values[0], ";")
	}
	if len(values) < 3 || len(values) > 512 {
		return nil
	}
	points := make([]shelfPlanePoint, 0, len(values))
	for _, value := range values {
		x, y, ok := strings.Cut(value, "x")
		xPos, errX := strconv.ParseFloat(strings.TrimSpace(x), 64)
		yPos, errY := strconv.ParseFloat(strings.TrimSpace(y), 64)
		p := meshPoint{xPos, yPos, 0}
		if !ok || errX != nil || errY != nil || !shelfPointValid(p) {
			return nil
		}
		point := shelfPlanePoint{xPos, yPos}
		if len(points) == 0 || points[len(points)-1] != point {
			points = append(points, point)
		}
	}
	if len(points) >= 2 && points[0] == points[len(points)-1] {
		points = points[:len(points)-1]
	}
	return points
}

func shelfXYBounds(bounds meshBounds) shelfFootprint {
	return shelfFootprint{
		min: shelfPlanePoint{bounds.min[0], bounds.min[1]},
		max: shelfPlanePoint{bounds.max[0], bounds.max[1]},
	}
}

func (bed shelfBed) contains(footprint shelfFootprint) bool {
	for _, x := range [...]float64{footprint.min[0], footprint.max[0]} {
		for _, y := range [...]float64{footprint.min[1], footprint.max[1]} {
			point := shelfPlanePoint{x, y}
			if !shelfPolygonContains(bed.polygon, point) || shelfPolygonContains(bed.excluded, point) {
				return false
			}
		}
	}
	previous := bed.polygon[len(bed.polygon)-1]
	for _, point := range bed.polygon {
		if footprint.crossesInterior(previous, point) {
			return false
		}
		previous = point
	}
	if len(bed.excluded) >= 3 {
		previous = bed.excluded[len(bed.excluded)-1]
		for _, point := range bed.excluded {
			if footprint.crossesInterior(previous, point) {
				return false
			}
			previous = point
		}
	}
	return true
}

func shelfPolygonContains(polygon []shelfPlanePoint, p shelfPlanePoint) bool {
	if len(polygon) < 3 {
		return false
	}
	inside := false
	a := polygon[len(polygon)-1]
	for _, b := range polygon {
		if math.Abs(shelfEdge(a, b, p)) <= shelfPlacementEpsilon*math.Hypot(b[0]-a[0], b[1]-a[1]) &&
			p[0] >= min(a[0], b[0])-shelfPlacementEpsilon && p[0] <= max(a[0], b[0])+shelfPlacementEpsilon &&
			p[1] >= min(a[1], b[1])-shelfPlacementEpsilon && p[1] <= max(a[1], b[1])+shelfPlacementEpsilon {
			return true
		}
		if (a[1] > p[1]) != (b[1] > p[1]) && p[0] < a[0]+(p[1]-a[1])*(b[0]-a[0])/(b[1]-a[1]) {
			inside = !inside
		}
		a = b
	}
	return inside
}

func (box shelfFootprint) crossesInterior(a, b shelfPlanePoint) bool {
	start, finish := 0.0, 1.0
	for axis := range a {
		low, high := box.min[axis]+shelfPlacementEpsilon, box.max[axis]-shelfPlacementEpsilon
		if a[axis] == b[axis] {
			if a[axis] <= low || a[axis] >= high {
				return false
			}
			continue
		}
		first, last := (low-a[axis])/(b[axis]-a[axis]), (high-a[axis])/(b[axis]-a[axis])
		if first > last {
			first, last = last, first
		}
		start, finish = max(start, first), min(finish, last)
	}
	return start < finish
}

func shelfPlacementAxis(low, high float64) []float64 {
	const steps = 64
	samples := make([]float64, steps+1)
	for i := range samples {
		samples[i] = low + (high-low)*float64(i)/steps
	}
	return samples
}

func (bed shelfBed) freeSpot(ctx context.Context, half shelfPlanePoint, occupied []shelfFootprint) (shelfPlanePoint, bool) {
	xMin, xMax := bed.bounds.min[0]+half[0], bed.bounds.max[0]-half[0]
	yMin, yMax := bed.bounds.min[1]+half[1], bed.bounds.max[1]-half[1]
	if xMin > xMax || yMin > yMax {
		return shelfPlanePoint{}, false
	}
	spots := shelfPlacementAxis(yMin, yMax)
	addRow := func(y float64) {
		if y >= yMin && y <= yMax {
			spots = append(spots, y)
		}
	}
	for _, point := range bed.polygon {
		addRow(point[1] - half[1])
		addRow(point[1] + half[1])
	}
	for _, point := range bed.excluded {
		addRow(point[1] - half[1] - 2*shelfPlacementEpsilon)
		addRow(point[1] + half[1] + 2*shelfPlacementEpsilon)
	}
	for i := range occupied {
		box := &occupied[i]
		box.min[0] -= half[0] + shelfPlacementGap
		box.max[0] += half[0] + shelfPlacementGap
		box.min[1] -= half[1] + shelfPlacementGap
		box.max[1] += half[1] + shelfPlacementGap
		addRow(box.min[1])
		addRow(box.max[1])
	}
	slices.Sort(spots)
	xSpots := shelfPlacementAxis(xMin, xMax)
	for _, point := range bed.polygon {
		xSpots = append(xSpots, point[0]-half[0], point[0]+half[0])
	}
	for _, point := range bed.excluded {
		xSpots = append(xSpots, point[0]-half[0]-2*shelfPlacementEpsilon, point[0]+half[0]+2*shelfPlacementEpsilon)
	}
	slices.Sort(xSpots)
	xSpots = slices.Compact(xSpots)
	slices.SortFunc(occupied, func(a, b shelfFootprint) int { return cmp.Compare(a.min[0], b.min[0]) })
	for _, y := range slices.Backward(slices.Compact(spots)) {
		if ctx.Err() != nil {
			break
		}
		tryGap := func(left, right float64) (shelfPlanePoint, bool) {
			if left > right {
				return shelfPlanePoint{}, false
			}
			fits := func(x float64) bool {
				return bed.contains(shelfFootprint{min: shelfPlanePoint{x - half[0], y - half[1]}, max: shelfPlanePoint{x + half[0], y + half[1]}})
			}
			for _, x := range [...]float64{left, right, (left + right) / 2} {
				if fits(x) {
					return shelfPlanePoint{x, y}, true
				}
			}
			start, _ := slices.BinarySearch(xSpots, left)
			for _, x := range xSpots[start:] {
				if x >= right {
					break
				}
				if fits(x) {
					return shelfPlanePoint{x, y}, true
				}
			}
			return shelfPlanePoint{}, false
		}
		left := xMin
		for _, box := range occupied {
			if y <= box.min[1] || y >= box.max[1] || box.max[0] <= left {
				continue
			}
			if point, ok := tryGap(left, min(xMax, box.min[0])); ok {
				return point, true
			}
			left = box.max[0]
			if left > xMax {
				break
			}
		}
		if point, ok := tryGap(left, xMax); ok {
			return point, true
		}
	}
	return shelfPlanePoint{}, false
}

func (s *Server) placeShelfGeometry(ctx context.Context, plate shelfPlateEdit, triangles []shelfTriangle) (shelfPlanePoint, error) {
	config := s.shelfPlateConfig(plate.doc)
	polygon := shelfBedPolygon(config["printable_area"])
	if len(polygon) < 3 {
		return shelfPlanePoint{}, shelfProblem(400, "placement_required", "The saved printer has no usable build plate shape. Supply explicit x and y coordinates.")
	}
	var bedBounds meshBounds
	for _, point := range polygon {
		_ = bedBounds.add(meshPoint{point[0], point[1], 0})
	}
	bed := shelfBed{polygon: polygon, bounds: shelfXYBounds(bedBounds), excluded: shelfBedPolygon(config["bed_exclude_area"])}
	bounds, err := shelfMeshBounds(triangles)
	if err != nil {
		return shelfPlanePoint{}, shelfProblem(422, "model_geometry_unavailable", "The server could not determine the size of this Shelf model.")
	}
	var half shelfPlanePoint
	for axis := range half {
		half[axis] = max(float64(float32((bounds.max[axis]-bounds.min[axis])/2)), shelfPlacementEpsilon)
	}
	var height float64
	if raw := config["printable_height"]; json.Unmarshal(raw, &height) != nil {
		height, _ = strconv.ParseFloat(stringValue(raw), 64)
	}
	noRoom := shelfProblem(409, "placement_no_room", "There is no free spot for this model on the saved plate.")
	if height > 0 && float64(float32(bounds.max[2]-bounds.min[2])) > height {
		return shelfPlanePoint{}, noRoom
	}
	g := shelfPlacementGeometry{ctx: ctx, server: s, meshes: make(map[string][]meshPoint)}
	if raw := plate.doc["files"]; len(raw) > 0 && json.Unmarshal(raw, &g.files) != nil {
		return shelfPlanePoint{}, shelfProblem(400, "project_invalid", "The saved project asset list is unreadable.")
	}
	json.Unmarshal(plate.doc["project"], &g.project)
	defer func() {
		if g.archive.file != nil {
			g.archive.file.Close()
		}
	}()
	occupied, err := g.occupied(plate.objects)
	if err != nil {
		return shelfPlanePoint{}, shelfProblem(422, "placement_geometry_unavailable", "The server could not determine where an existing model sits. Supply explicit x and y coordinates.")
	}
	point, ok := bed.freeSpot(ctx, half, occupied)
	if !ok {
		return shelfPlanePoint{}, noRoom
	}
	return point, nil
}
