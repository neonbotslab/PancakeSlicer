package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const shelfPlacementMaxInstances = 2048

type shelfSavedInstance struct {
	X, Y *float64
	RotZ float64
}

type shelfSavedVolume struct {
	Type     string
	Matrix   []float64
	Native   *int
	Geometry struct{ Position json.RawMessage }
}

type shelfSavedObject struct {
	Source     string
	RotX, RotY float64
	Scale      []float64
	Instances  []shelfSavedInstance
	Volumes    []shelfSavedVolume
	Project    *struct {
		BuildIndex           *int
		Placeholder, Missing bool
	}
	Native *struct {
		Files map[string]string
		Frame []float64
		Main  int
		Entry struct {
			Volumes []struct {
				File      string
				Transform json.RawMessage
			}
		}
	}
}

type shelfProjectGeometry struct {
	file  *os.File
	pkg   *shelf3MFPackage
	model *shelf3MFModel
}

type shelfPlacementGeometry struct {
	ctx                           context.Context
	server                        *Server
	files                         map[string]string
	project                       string
	archive                       shelfProjectGeometry
	meshes                        map[string][]meshPoint
	readPoints, transformedPoints int
}

var errShelfPlacementGeometry = errors.New("unsupported saved plate geometry")

func (g *shelfPlacementGeometry) object(object shelfSavedObject) ([]meshPoint, error) {
	if object.Project != nil {
		return g.projectObject(object)
	}
	key := object.Source
	if object.Native != nil {
		native := object.Native
		if native.Main < 0 || native.Main >= len(native.Entry.Volumes) {
			return nil, errShelfPlacementGeometry
		}
		main := native.Entry.Volumes[native.Main]
		if native.Files[main.File] == key {
			return g.nativeObject(key, native.Frame, main.Transform)
		}
	}
	if points, present := g.meshes[key]; present {
		return points, nil
	}
	name, ok := g.files[key]
	if !ok || key == "" {
		return nil, errShelfPlacementGeometry
	}
	format := strings.ToLower(filepath.Ext(name))
	if format != ".stl" && format != ".obj" {
		return nil, errShelfPlacementGeometry
	}
	triangles, err := readShelfMesh(g.ctx, g.server.cfg, g.server.workspace.assetPath(key), format)
	if err != nil {
		return nil, err
	}
	points, err := g.pointsFromTriangles(triangles)
	if err == nil {
		g.meshes[key] = points
	}
	return points, err
}

func (g *shelfPlacementGeometry) projectObject(object shelfSavedObject) ([]meshPoint, error) {
	p := object.Project
	if p.Missing || p.Placeholder || p.BuildIndex == nil || g.project == "" || len(object.Volumes) > 0 {
		return nil, errShelfPlacementGeometry
	}
	if g.archive.file == nil {
		if _, ok := g.files[g.project]; !ok {
			return nil, errShelfPlacementGeometry
		}
		archive, err := g.openProject(g.project)
		if err != nil {
			return nil, err
		}
		g.archive = archive
	}
	index := *p.BuildIndex
	model := g.archive.model
	if index < 0 || index >= len(model.build) {
		return nil, errShelfPlacementGeometry
	}
	triangles, err := g.archive.pkg.buildTriangles(model, model.build[index:index+1])
	if err != nil {
		return nil, err
	}
	return g.pointsFromTriangles(triangles)
}

func (g *shelfPlacementGeometry) openProject(source string) (shelfProjectGeometry, error) {
	var archive shelfProjectGeometry
	f, size, err := openShelfMesh(g.server.workspace.assetPath(source))
	if err != nil {
		return archive, err
	}
	pkg, err := openShelf3MF(g.ctx, f, size)
	if err != nil {
		f.Close()
		return archive, err
	}
	model, err := pkg.load(pkg.root, true)
	if err != nil {
		f.Close()
		return archive, err
	}
	return shelfProjectGeometry{file: f, pkg: pkg, model: model}, nil
}

func (g *shelfPlacementGeometry) nativeObject(source string, frame []float64, raw json.RawMessage) ([]meshPoint, error) {
	if _, ok := g.files[source]; !ok || source == "" || len(raw) == 0 || !shelfIsAffine(frame) {
		return nil, errShelfPlacementGeometry
	}
	transform, err := volumeTransform(raw)
	if err != nil || transform[12] != 0 || transform[13] != 0 || transform[14] != 0 || transform[15] != 1 {
		return nil, errShelfPlacementGeometry
	}
	archive, err := g.openProject(source)
	if err != nil {
		return nil, err
	}
	defer archive.file.Close()
	if len(archive.model.objects) != 1 || archive.model.unit != 1 {
		return nil, errShelfPlacementGeometry
	}
	for _, object := range archive.model.objects {
		if len(object.vertices) == 0 || len(object.components) != 0 || !g.reservePoints(len(object.vertices)) {
			return nil, errShelfPlacementGeometry
		}
		points := make([]meshPoint, len(object.vertices))
		for i, point := range object.vertices {
			p := meshPoint{float64(float32(point[0])), float64(float32(point[1])), float64(float32(point[2]))}
			var local meshPoint
			for axis := range local {
				row := transform[axis*4 : axis*4+4]
				local[axis] = row[0]*p[0] + row[1]*p[1] + row[2]*p[2] + row[3]
			}
			points[i] = shelfApplyMatrix(frame, local)
			for axis := range points[i] {
				points[i][axis] = float64(float32(points[i][axis]))
			}
		}
		return points, nil
	}
	return nil, errShelfPlacementGeometry
}

func (g *shelfPlacementGeometry) reservePoints(count int) bool {
	g.readPoints += count
	return count > 0 && g.readPoints <= 2*shelfMeshMaxVertices
}

func (g *shelfPlacementGeometry) pointsFromTriangles(triangles []shelfTriangle) ([]meshPoint, error) {
	if !g.reservePoints(3 * len(triangles)) {
		return nil, errShelfMeshLimit
	}
	points := make([]meshPoint, 0, 3*len(triangles))
	for _, triangle := range triangles {
		points = append(points, triangle[:]...)
	}
	var bounds meshBounds
	for i, point := range points {
		for axis, coordinate := range point {
			points[i][axis] = float64(float32(coordinate))
		}
		_ = bounds.add(points[i])
	}
	offset := meshPoint{(bounds.min[0] + bounds.max[0]) / 2, (bounds.min[1] + bounds.max[1]) / 2, bounds.min[2]}
	for i, point := range points {
		for axis := range point {
			points[i][axis] = float64(float32(point[axis] - offset[axis]))
		}
	}
	return points, nil
}

type shelfVolumePoints struct {
	matrix []float64
	points []meshPoint
}

func (g *shelfPlacementGeometry) volume(v shelfSavedVolume) (shelfVolumePoints, error) {
	var volume shelfVolumePoints
	if !shelfIsAffine(v.Matrix) || len(v.Geometry.Position) == 0 {
		return volume, errShelfPlacementGeometry
	}
	var wire struct {
		Type string `json:"$orcaBinary"`
		Data string
	}
	if json.Unmarshal(v.Geometry.Position, &wire) != nil || wire.Type != "Float32Array" ||
		len(wire.Data) > base64.StdEncoding.EncodedLen(12*shelfMeshMaxVertices) {
		return volume, errShelfPlacementGeometry
	}
	bytes, err := base64.StdEncoding.DecodeString(wire.Data)
	if err != nil || len(bytes)%12 != 0 || !g.reservePoints(len(bytes)/12) {
		return volume, errShelfPlacementGeometry
	}
	points := make([]meshPoint, len(bytes)/12)
	var bounds meshBounds
	for i := range points {
		for axis := range points[i] {
			points[i][axis] = float64(math.Float32frombits(binary.LittleEndian.Uint32(bytes[i*12+axis*4:])))
		}
		if !shelfPointValid(points[i]) {
			return volume, errShelfPlacementGeometry
		}
		_ = bounds.add(points[i])
	}
	if v.Native == nil {
		for i, point := range points {
			for axis := range point {
				points[i][axis] = float64(float32(point[axis] - (bounds.min[axis]+bounds.max[axis])/2))
			}
		}
	}
	return shelfVolumePoints{matrix: v.Matrix, points: points}, nil
}

func shelfIsAffine(m []float64) bool {
	return len(m) == 16 && m[3] == 0 && m[7] == 0 && m[11] == 0 && m[15] == 1
}

func shelfApplyMatrix(m []float64, p meshPoint) meshPoint {
	return meshPoint{
		m[0]*p[0] + m[4]*p[1] + m[8]*p[2] + m[12],
		m[1]*p[0] + m[5]*p[1] + m[9]*p[2] + m[13],
		m[2]*p[0] + m[6]*p[1] + m[10]*p[2] + m[14],
	}
}

type shelfRotation struct {
	sin, cos float64
}

func shelfDegrees(degrees float64) shelfRotation {
	sin, cos := math.Sincos(degrees * (math.Pi / 180))
	return shelfRotation{sin, cos}
}

func (r shelfRotation) apply(a, b float64) (float64, float64) {
	return r.cos*a - r.sin*b, r.sin*a + r.cos*b
}

func (g *shelfPlacementGeometry) bounds(points []meshPoint, volumes []shelfVolumePoints, object shelfSavedObject, instance shelfSavedInstance) (shelfFootprint, error) {
	if instance.X == nil || instance.Y == nil || math.Abs(*instance.X) > math.MaxFloat32 || math.Abs(*instance.Y) > math.MaxFloat32 {
		return shelfFootprint{}, errShelfPlacementGeometry
	}
	xRot, yRot, zRot := shelfDegrees(object.RotX), shelfDegrees(object.RotY), shelfDegrees(instance.RotZ)
	transform := func(p meshPoint) meshPoint {
		x, y, z := p[0]*object.Scale[0], p[1]*object.Scale[1], p[2]*object.Scale[2]
		y, z = xRot.apply(y, z)
		z, x = yRot.apply(z, x)
		x, y = zRot.apply(x, y)
		return meshPoint{x, y, z}
	}
	var base, all meshBounds
	add := func(bounds *meshBounds, points []meshPoint, matrix []float64) error {
		g.transformedPoints += len(points)
		if g.transformedPoints > 8*shelfMeshMaxVertices {
			return errShelfMeshLimit
		}
		for i, point := range points {
			if i%4096 == 0 && g.ctx.Err() != nil {
				return g.ctx.Err()
			}
			if matrix != nil {
				point = shelfApplyMatrix(matrix, point)
			}
			if err := bounds.add(transform(point)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(&base, points, nil); err != nil {
		return shelfFootprint{}, err
	}
	all = base
	for _, volume := range volumes {
		if err := add(&all, volume.points, volume.matrix); err != nil {
			return shelfFootprint{}, err
		}
	}
	box := shelfXYBounds(all)
	position := shelfPlanePoint{*instance.X, *instance.Y}
	for axis := range position {
		center := (base.min[axis] + base.max[axis]) / 2
		box.min[axis] += position[axis] - center
		box.max[axis] += position[axis] - center
	}
	return box, nil
}

func (g *shelfPlacementGeometry) occupied(objects []json.RawMessage) ([]shelfFootprint, error) {
	var occupied []shelfFootprint
	for _, raw := range objects {
		var object shelfSavedObject
		if json.Unmarshal(raw, &object) != nil || len(object.Instances) == 0 || len(object.Scale) != 3 ||
			len(occupied)+len(object.Instances) > shelfPlacementMaxInstances {
			return nil, errShelfPlacementGeometry
		}
		points, err := g.object(object)
		if err != nil {
			return nil, err
		}
		var volumes []shelfVolumePoints
		for _, v := range object.Volumes {
			if v.Type == "part" {
				volume, err := g.volume(v)
				if err != nil {
					return nil, err
				}
				volumes = append(volumes, volume)
			}
		}
		for _, instance := range object.Instances {
			box, err := g.bounds(points, volumes, object, instance)
			if err != nil {
				return nil, err
			}
			occupied = append(occupied, box)
		}
	}
	return occupied, nil
}
