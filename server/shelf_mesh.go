package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	shelfMeshMaxBytes      = 128 << 20
	shelfMeshMaxTriangles  = 1_000_000
	shelfMeshMaxVertices   = 3 * shelfMeshMaxTriangles
	shelfMeshMaxCoordinate = 1_000_000
	shelf3MFMaxXML         = 96 << 20
	shelf3MFMaxParts       = 2048
	shelf3MFMaxDepth       = 32
)

type shelfTriangle [3]meshPoint

var (
	errShelfEmptyMesh = errors.New("model contains no triangles")
	errShelfMeshLimit = errors.New("model geometry exceeds the processing limits")
)

func readShelfMesh(ctx context.Context, cfg Config, file, format string) ([]shelfTriangle, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > shelfMeshMaxBytes {
		return nil, errShelfMeshLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var triangles []shelfTriangle
	switch format {
	case ".stl":
		triangles, err = readShelfSTL(ctx, f, info.Size())
	case ".obj":
		triangles, err = readShelfOBJ(ctx, f)
	case ".3mf":
		triangles, err = readShelf3MF(ctx, f, info.Size())
	case ".step", ".stp":
		triangles, err = readShelfSTEP(ctx, cfg, f, format)
	default:
		return nil, errors.New("unsupported model format")
	}
	if err != nil {
		return nil, err
	}
	if len(triangles) == 0 {
		return nil, errShelfEmptyMesh
	}
	return triangles, nil
}

func shelfPointValid(point meshPoint) bool {
	for _, coordinate := range point {
		if math.IsNaN(coordinate) || math.Abs(coordinate) > shelfMeshMaxCoordinate {
			return false
		}
	}
	return true
}

func parseShelfPoint(values [3]string, scale float64) (meshPoint, error) {
	var point meshPoint
	for i, raw := range values {
		coordinate, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return point, errors.New("invalid model vertex coordinates")
		}
		point[i] = coordinate * scale
	}
	if !shelfPointValid(point) {
		return point, errors.New("invalid model vertex coordinates")
	}
	return point, nil
}

func readShelfSTL(ctx context.Context, file *os.File, size int64) ([]shelfTriangle, error) {
	var header [84]byte
	if _, err := file.ReadAt(header[:], 0); err == nil {
		count := binary.LittleEndian.Uint32(header[80:])
		if 84+int64(count)*50 == size {
			if count > shelfMeshMaxTriangles {
				return nil, errShelfMeshLimit
			}
			reader := bufio.NewReader(io.NewSectionReader(file, 84, size-84))
			triangles := make([]shelfTriangle, count)
			var record [50]byte
			for i := range triangles {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if _, err := io.ReadFull(reader, record[:]); err != nil {
					return nil, err
				}
				for j := range triangles[i] {
					var point meshPoint
					for k := range point {
						bits := binary.LittleEndian.Uint32(record[12+(j*3+k)*4:])
						point[k] = float64(math.Float32frombits(bits))
					}
					if !shelfPointValid(point) {
						return nil, errors.New("invalid STL vertex coordinates")
					}
					triangles[i][j] = point
				}
			}
			return triangles, nil
		}
	}
	return readShelfASCIISTL(ctx, io.NewSectionReader(file, 0, size))
}

func shelfLineScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16<<10), 1<<20)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if newline := bytes.IndexAny(data, "\r\n"); newline >= 0 {
			return newline + 1, data[:newline], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return scanner
}

func readShelfASCIISTL(ctx context.Context, reader io.Reader) ([]shelfTriangle, error) {
	var triangles []shelfTriangle
	var triangle shelfTriangle
	scanner := shelfLineScanner(reader)
	solid, inFacet, inLoop := false, false, false
	vertices := 0
	invalid := errors.New("invalid ASCII STL structure")
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields := strings.Fields(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if len(fields) == 0 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "solid":
			if solid {
				return nil, invalid
			}
			solid = true
		case "facet":
			if !solid || inFacet || len(fields) != 5 || !strings.EqualFold(fields[1], "normal") {
				return nil, invalid
			}
			inFacet, vertices = true, 0
		case "outer":
			if !inFacet || inLoop || vertices != 0 || len(fields) != 2 || !strings.EqualFold(fields[1], "loop") {
				return nil, invalid
			}
			inLoop = true
		case "vertex":
			if !inLoop || vertices == 3 || len(fields) != 4 {
				return nil, invalid
			}
			point, err := parseShelfPoint([3]string(fields[1:]), 1)
			if err != nil {
				return nil, err
			}
			triangle[vertices] = point
			vertices++
		case "endloop":
			if !inLoop || vertices != 3 {
				return nil, invalid
			}
			inLoop = false
		case "endfacet":
			if !inFacet || inLoop || vertices != 3 {
				return nil, invalid
			}
			if len(triangles) == shelfMeshMaxTriangles {
				return nil, errShelfMeshLimit
			}
			triangles = append(triangles, triangle)
			inFacet = false
		case "endsolid":
			if !solid || inFacet {
				return nil, invalid
			}
			solid = false
		default:
			return nil, invalid
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if solid || inFacet || inLoop {
		return nil, invalid
	}
	return triangles, nil
}

func readShelfOBJ(ctx context.Context, reader io.Reader) ([]shelfTriangle, error) {
	var vertices []meshPoint
	var triangles []shelfTriangle
	polygon := make([]meshPoint, 0, 8)
	work := 16_000_000
	scanner := shelfLineScanner(reader)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, _, _ := strings.Cut(strings.TrimPrefix(scanner.Text(), "\ufeff"), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "v":
			if len(fields) < 4 {
				return nil, errors.New("invalid OBJ vertex")
			}
			if len(vertices) == shelfMeshMaxVertices {
				return nil, errShelfMeshLimit
			}
			point, err := parseShelfPoint([3]string(fields[1:4]), 1)
			if err != nil {
				return nil, err
			}
			vertices = append(vertices, point)
		case "f":
			if len(fields) < 4 {
				return nil, errors.New("invalid OBJ face")
			}
			if len(fields) > 2049 || len(triangles)+len(fields)-3 > shelfMeshMaxTriangles {
				return nil, errShelfMeshLimit
			}
			polygon = polygon[:0]
			for _, field := range fields[1:] {
				raw, _, _ := strings.Cut(field, "/")
				number, err := strconv.ParseInt(raw, 10, 32)
				if err != nil || number == 0 {
					return nil, errors.New("invalid OBJ face vertex index")
				}
				index := int(number) - 1
				if number < 0 {
					index = len(vertices) + int(number)
				}
				if index < 0 || index >= len(vertices) {
					return nil, errors.New("OBJ face references a missing vertex")
				}
				polygon = append(polygon, vertices[index])
			}
			if len(polygon) == 3 {
				triangles = append(triangles, shelfTriangle(polygon))
				continue
			}
			var err error
			triangles, err = triangulateShelfPolygon(polygon, triangles, &work)
			if err != nil {
				return nil, err
			}
		}
	}
	return triangles, scanner.Err()
}

type shelfPlanePoint [2]float64

func shelfEdge(a, b, p shelfPlanePoint) float64 {
	return (b[0]-a[0])*(p[1]-a[1]) - (b[1]-a[1])*(p[0]-a[0])
}

func triangulateShelfPolygon(points []meshPoint, out []shelfTriangle, work *int) ([]shelfTriangle, error) {
	if points[0] == points[len(points)-1] {
		points = points[:len(points)-1]
	}
	var normal meshPoint
	for i := 1; i+1 < len(points); i++ {
		facet := shelfTriangleNormal(shelfTriangle{points[0], points[i], points[i+1]})
		for axis := range normal {
			normal[axis] += facet[axis]
		}
	}
	drop := 0
	for axis := 1; axis < 3; axis++ {
		if math.Abs(normal[axis]) > math.Abs(normal[drop]) {
			drop = axis
		}
	}
	area := normal[drop]
	if area == 0 {
		return nil, errors.New("OBJ face has no polygon area")
	}
	x, y := (drop+1)%3, (drop+2)%3
	projected := make([]shelfPlanePoint, len(points))
	remaining := make([]int, len(points))
	for i, point := range points {
		projected[i] = shelfPlanePoint{point[x] - points[0][x], point[y] - points[0][y]}
		remaining[i] = i
	}
	winding := math.Copysign(1, area)
	for len(remaining) > 3 {
		found := false
		for i, current := range remaining {
			*work -= len(remaining)
			if *work < 0 {
				return nil, errShelfMeshLimit
			}
			prev := remaining[(i+len(remaining)-1)%len(remaining)]
			next := remaining[(i+1)%len(remaining)]
			a, b, c := projected[prev], projected[current], projected[next]
			if winding*shelfEdge(a, b, c) <= 0 {
				continue
			}
			containsVertex := false
			for _, other := range remaining {
				if other == prev || other == current || other == next {
					continue
				}
				p := projected[other]
				if winding*shelfEdge(a, b, p) >= 0 && winding*shelfEdge(b, c, p) >= 0 && winding*shelfEdge(c, a, p) >= 0 {
					containsVertex = true
					break
				}
			}
			if containsVertex {
				continue
			}
			out = append(out, shelfTriangle{points[prev], points[current], points[next]})
			remaining = append(remaining[:i], remaining[i+1:]...)
			found = true
			break
		}
		if !found {
			return nil, errors.New("OBJ face cannot be triangulated")
		}
	}
	return append(out, shelfTriangle{points[remaining[0]], points[remaining[1]], points[remaining[2]]}), nil
}

func shelfTriangleNormal(t shelfTriangle) meshPoint {
	a := meshPoint{t[1][0] - t[0][0], t[1][1] - t[0][1], t[1][2] - t[0][2]}
	b := meshPoint{t[2][0] - t[0][0], t[2][1] - t[0][1], t[2][2] - t[0][2]}
	return meshPoint{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}

func shelfMeshBounds(triangles []shelfTriangle) (meshBounds, error) {
	var bounds meshBounds
	if len(triangles) == 0 {
		return bounds, errShelfEmptyMesh
	}
	if len(triangles) > shelfMeshMaxTriangles {
		return bounds, errShelfMeshLimit
	}
	for _, triangle := range triangles {
		for _, point := range triangle {
			if !shelfPointValid(point) {
				return bounds, errors.New("invalid model vertex coordinates")
			}
			_ = bounds.add(point)
		}
	}
	return bounds, nil
}

func shelfSTL(triangles []shelfTriangle) ([]byte, meshPoint, error) {
	bounds, err := shelfMeshBounds(triangles)
	if err != nil {
		return nil, meshPoint{}, err
	}
	if bounds.max[2]-bounds.min[2] > shelfMeshMaxCoordinate {
		return nil, meshPoint{}, errShelfMeshLimit
	}
	var center meshPoint
	for axis := range center {
		center[axis] = (bounds.min[axis] + bounds.max[axis]) / 2
	}
	offset := center
	offset[2] = bounds.min[2]
	data := make([]byte, 84+50*len(triangles))
	copy(data, "Orca model shelf")
	binary.LittleEndian.PutUint32(data[80:84], uint32(len(triangles)))
	for i, triangle := range triangles {
		record := data[84+i*50:]
		for j := range triangle {
			for axis := range triangle[j] {
				triangle[j][axis] -= offset[axis]
			}
		}
		normal := shelfTriangleNormal(triangle)
		length := math.Hypot(math.Hypot(normal[0], normal[1]), normal[2])
		if length > 0 {
			for axis := range normal {
				normal[axis] /= length
			}
		}
		for j, point := range [4]meshPoint{normal, triangle[0], triangle[1], triangle[2]} {
			for axis, coordinate := range point {
				binary.LittleEndian.PutUint32(record[(j*3+axis)*4:], math.Float32bits(float32(coordinate)))
			}
		}
	}
	return data, center, nil
}

type shelf3MFRef struct {
	id        uint32
	part      string
	transform modelTransform
}

type shelf3MFObject struct {
	vertices   []meshPoint
	triangles  [][3]uint32
	components []shelf3MFRef
}

type shelf3MFModel struct {
	unit    float64
	objects map[uint32]*shelf3MFObject
	build   []shelf3MFRef
}

type shelf3MFPackage struct {
	ctx        context.Context
	entries    map[string]*zip.File
	models     map[string]*shelf3MFModel
	remaining  int64
	vertices   int
	triangles  int
	references int
	objects    int
}

func shelfZIPDirectory(file *os.File, size int64, maxEntries int) error {
	const endSize = 22
	footer := make([]byte, min(size, endSize+65535))
	if _, err := file.ReadAt(footer, size-int64(len(footer))); err != nil {
		return err
	}
	for i := len(footer) - endSize; i >= 0; i-- {
		end := footer[i:]
		if string(end[:4]) != "PK\x05\x06" {
			continue
		}
		if int(binary.LittleEndian.Uint16(end[20:])) != len(end)-endSize {
			return zip.ErrFormat
		}
		position := uint64(size - int64(len(footer)) + int64(i))
		count := uint64(binary.LittleEndian.Uint16(end[10:]))
		length := uint64(binary.LittleEndian.Uint32(end[12:]))
		offset := uint64(binary.LittleEndian.Uint32(end[16:]))
		if binary.LittleEndian.Uint32(end[4:]) != 0 || uint64(binary.LittleEndian.Uint16(end[8:])) != count {
			return zip.ErrFormat
		}
		if length > position || offset != position-length {
			locator, err := shelfZIP64Directory(file, position)
			if err != nil {
				return err
			}
			if (count != locator.count && count != 0xffff) ||
				(length != locator.length && length != 0xffffffff) ||
				(offset != locator.offset && offset != 0xffffffff) {
				return zip.ErrFormat
			}
			count, length, offset, position = locator.count, locator.length, locator.offset, locator.position
		}
		if count > uint64(maxEntries) || length > 4<<20 {
			return errShelfMeshLimit
		}
		if length > position || offset != position-length {
			return zip.ErrFormat
		}
		var header [46]byte
		for i := uint64(0); i < count; i++ {
			if position-offset < uint64(len(header)) {
				return zip.ErrFormat
			}
			if _, err := file.ReadAt(header[:], int64(offset)); err != nil {
				return err
			}
			if string(header[:4]) != "PK\x01\x02" {
				return zip.ErrFormat
			}
			headerLength := uint64(len(header)) +
				uint64(binary.LittleEndian.Uint16(header[28:])) +
				uint64(binary.LittleEndian.Uint16(header[30:])) +
				uint64(binary.LittleEndian.Uint16(header[32:]))
			if headerLength > position-offset {
				return zip.ErrFormat
			}
			offset += headerLength
		}
		if offset != position {
			return zip.ErrFormat
		}
		return nil
	}
	return zip.ErrFormat
}

type shelfZipDirectory struct {
	count, length, offset, position uint64
}

func shelfZIP64Directory(file *os.File, position uint64) (shelfZipDirectory, error) {
	var directory shelfZipDirectory
	var locator [20]byte
	if position < uint64(len(locator)) {
		return directory, zip.ErrFormat
	}
	locatorPosition := position - uint64(len(locator))
	if _, err := file.ReadAt(locator[:], int64(locatorPosition)); err != nil {
		return directory, err
	}
	directory.position = binary.LittleEndian.Uint64(locator[8:])
	if string(locator[:4]) != "PK\x06\x07" || binary.LittleEndian.Uint32(locator[4:]) != 0 ||
		binary.LittleEndian.Uint32(locator[16:]) != 1 || directory.position >= locatorPosition {
		return directory, zip.ErrFormat
	}
	var end [56]byte
	if _, err := file.ReadAt(end[:], int64(directory.position)); err != nil {
		return directory, err
	}
	recordLength := binary.LittleEndian.Uint64(end[4:])
	directory.count = binary.LittleEndian.Uint64(end[32:])
	directory.length = binary.LittleEndian.Uint64(end[40:])
	directory.offset = binary.LittleEndian.Uint64(end[48:])
	if string(end[:4]) != "PK\x06\x06" || locatorPosition-directory.position < uint64(len(end)) ||
		locatorPosition-directory.position-12 != recordLength ||
		binary.LittleEndian.Uint64(end[16:]) != 0 || binary.LittleEndian.Uint64(end[24:]) != directory.count {
		return directory, zip.ErrFormat
	}
	return directory, nil
}

func shelf3MFPart(parent, raw string) (string, error) {
	name, err := url.PathUnescape(raw)
	if err != nil || name == "" || len(name) > 4096 || strings.ContainsAny(name, "\\:\x00?#") {
		return "", errors.New("invalid 3MF package path")
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return "", errors.New("invalid 3MF package path")
		}
	}
	if !strings.HasPrefix(name, "/") {
		name = path.Join(path.Dir(parent), name)
	}
	return strings.ToLower(path.Clean(name)), nil
}

func readShelf3MF(ctx context.Context, file *os.File, size int64) ([]shelfTriangle, error) {
	if err := shelfZIPDirectory(file, size, shelf3MFMaxParts); err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(file, size)
	if err != nil {
		return nil, err
	}
	if len(archive.File) > shelf3MFMaxParts {
		return nil, errShelfMeshLimit
	}
	pkg := shelf3MFPackage{
		ctx: ctx, remaining: shelf3MFMaxXML,
		entries: make(map[string]*zip.File, len(archive.File)),
		models:  make(map[string]*shelf3MFModel),
	}
	for _, entry := range archive.File {
		part, err := shelf3MFPart("/", entry.Name)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(entry.Name, "/") {
			continue
		}
		if pkg.entries[part] != nil {
			return nil, errors.New("duplicate 3MF package path")
		}
		pkg.entries[part] = entry
	}
	root := "/3d/3dmodel.model"
	if pkg.entries["/_rels/.rels"] != nil {
		found, err := pkg.rootPart()
		if err != nil {
			return nil, err
		}
		if found != "" {
			root = found
		}
	}
	model, err := pkg.load(root, true)
	if err != nil {
		return nil, err
	}
	var out []shelfTriangle
	active := make(map[shelf3MFObjectKey]bool)
	visits := 0
	for _, item := range model.build {
		if err := pkg.appendTriangles(root, model.unit, item, modelTransform{1, 0, 0, 0, 1, 0, 0, 0, 1}, active, &visits, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type shelfContextReader struct {
	ctx context.Context
	io.Reader
}

func (r shelfContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}

func (p *shelf3MFPackage) readXML(part string, maxBytes int64, read func(*xml.Decoder) error) error {
	entry := p.entries[part]
	if entry == nil {
		return fmt.Errorf("missing 3MF package part %q", part)
	}
	limit := min(maxBytes, p.remaining)
	if entry.UncompressedSize64 > uint64(limit) ||
		entry.CompressedSize64 > shelfMeshMaxBytes ||
		entry.UncompressedSize64 > 200*entry.CompressedSize64 {
		return errShelfMeshLimit
	}
	f, err := entry.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	reader := &io.LimitedReader{R: f, N: limit + 1}
	text := bufio.NewReader(shelfContextReader{ctx: p.ctx, Reader: reader})
	if prefix, _ := text.Peek(3); string(prefix) == "\xef\xbb\xbf" {
		_, _ = text.Discard(len(prefix))
	}
	decoder := xml.NewDecoder(text)
	if err := read(decoder); err != nil {
		return err
	}
	if reader.N == 0 {
		return errShelfMeshLimit
	}
	p.remaining -= limit + 1 - reader.N
	return nil
}

func (p *shelf3MFPackage) rootPart() (string, error) {
	var root string
	err := p.readXML("/_rels/.rels", 1<<20, func(decoder *xml.Decoder) error {
		var rels struct {
			XMLName xml.Name `xml:"Relationships"`
			Items   []struct {
				Target string `xml:"Target,attr"`
				Type   string `xml:"Type,attr"`
				Mode   string `xml:"TargetMode,attr"`
			} `xml:"Relationship"`
		}
		if err := decoder.Decode(&rels); err != nil {
			return err
		}
		for _, rel := range rels.Items {
			if !strings.HasSuffix(rel.Type, "/3dmodel") {
				continue
			}
			if root != "" || strings.EqualFold(rel.Mode, "External") {
				return errors.New("invalid 3MF model relationship")
			}
			part, err := shelf3MFPart("/", rel.Target)
			if err != nil {
				return err
			}
			root = part
		}
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			switch value := token.(type) {
			case xml.Comment, xml.ProcInst:
			case xml.CharData:
				if len(strings.TrimSpace(string(value))) != 0 {
					return errors.New("invalid 3MF relationships")
				}
			default:
				return errors.New("invalid 3MF relationships")
			}
		}
	})
	return root, err
}

func shelfXMLAttr(token xml.StartElement, name string) string {
	for _, attr := range token.Attr {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func shelf3MFID(raw string, allowZero bool) (uint32, error) {
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || (id == 0 && !allowZero) {
		return 0, errors.New("invalid 3MF vertex or object index")
	}
	return uint32(id), nil
}

func (p *shelf3MFPackage) load(part string, root bool) (*shelf3MFModel, error) {
	if model := p.models[part]; model != nil {
		return model, nil
	}
	model := &shelf3MFModel{objects: make(map[uint32]*shelf3MFObject)}
	err := p.readXML(part, shelf3MFMaxXML, func(decoder *xml.Decoder) error {
		var stack [shelf3MFMaxDepth]string
		depth := 0
		seenModel := false
		var object *shelf3MFObject
		for {
			token, err := decoder.Token()
			if err == io.EOF && seenModel && depth == 0 {
				return nil
			}
			if err != nil {
				return err
			}
			switch token := token.(type) {
			case xml.StartElement:
				if depth == len(stack) {
					return errShelfMeshLimit
				}
				name := token.Name.Local
				stack[depth] = name
				depth++
				switch {
				case depth == 1:
					if seenModel || name != "model" {
						return errors.New("invalid 3MF model XML")
					}
					unit := shelfXMLAttr(token, "unit")
					model.unit = map[string]float64{"": 1, "micron": 0.001, "millimeter": 1, "centimeter": 10, "inch": 25.4, "foot": 304.8, "meter": 1000}[unit]
					if model.unit == 0 {
						return errors.New("unsupported 3MF measurement unit")
					}
					seenModel = true
				case depth == 3 && stack[1] == "resources" && name == "object":
					id, err := shelf3MFID(shelfXMLAttr(token, "id"), false)
					if err != nil {
						return err
					}
					if model.objects[id] != nil {
						return errors.New("duplicate 3MF object ID")
					}
					p.objects++
					if p.objects > 100_000 {
						return errShelfMeshLimit
					}
					object = new(shelf3MFObject)
					model.objects[id] = object
				case depth == 3 && root && stack[1] == "build" && name == "item":
					ref, err := p.ref(part, token)
					if err != nil {
						return err
					}
					model.build = append(model.build, ref)
				case depth == 5 && object != nil && stack[3] == "components" && name == "component":
					ref, err := p.ref(part, token)
					if err != nil {
						return err
					}
					object.components = append(object.components, ref)
				case depth == 6 && object != nil && stack[3] == "mesh" && stack[4] == "vertices" && name == "vertex":
					p.vertices++
					if p.vertices > shelfMeshMaxVertices {
						return errShelfMeshLimit
					}
					point, err := parseShelfPoint([3]string{shelfXMLAttr(token, "x"), shelfXMLAttr(token, "y"), shelfXMLAttr(token, "z")}, model.unit)
					if err != nil {
						return err
					}
					object.vertices = append(object.vertices, point)
				case depth == 6 && object != nil && stack[3] == "mesh" && stack[4] == "triangles" && name == "triangle":
					p.triangles++
					if p.triangles > shelfMeshMaxTriangles {
						return errShelfMeshLimit
					}
					var indices [3]uint32
					for axis, attr := range [3]string{"v1", "v2", "v3"} {
						index, err := shelf3MFID(shelfXMLAttr(token, attr), true)
						if err != nil {
							return err
						}
						indices[axis] = index
					}
					object.triangles = append(object.triangles, indices)
				}
			case xml.EndElement:
				if depth == 3 && object != nil {
					for _, indices := range object.triangles {
						for _, index := range indices {
							if int64(index) >= int64(len(object.vertices)) {
								return errors.New("3MF triangle references a missing vertex")
							}
						}
					}
					object = nil
				}
				depth--
			case xml.Directive:
				return errors.New("invalid 3MF model directive")
			case xml.CharData:
				if depth == 0 && len(strings.TrimSpace(string(token))) != 0 {
					return errors.New("invalid 3MF model XML")
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	p.models[part] = model
	return model, nil
}

func (p *shelf3MFPackage) ref(part string, token xml.StartElement) (shelf3MFRef, error) {
	p.references++
	if p.references > shelfMeshMaxTriangles {
		return shelf3MFRef{}, errShelfMeshLimit
	}
	id, err := shelf3MFID(shelfXMLAttr(token, "objectid"), false)
	if err != nil {
		return shelf3MFRef{}, err
	}
	if raw := shelfXMLAttr(token, "path"); raw != "" {
		part, err = shelf3MFPart(part, raw)
		if err != nil {
			return shelf3MFRef{}, err
		}
	}
	transform, err := transformValue(strings.TrimSpace(shelfXMLAttr(token, "transform")))
	if err != nil {
		return shelf3MFRef{}, err
	}
	for i, value := range transform {
		if math.IsNaN(value) || math.IsInf(value, 0) || (i < 9 && math.Abs(value) > shelfMeshMaxCoordinate) {
			return shelf3MFRef{}, errors.New("invalid 3MF transform")
		}
	}
	return shelf3MFRef{id: id, part: part, transform: transform}, nil
}

func shelfComposeTransform(parent, local modelTransform) modelTransform {
	var combined modelTransform
	for row := 0; row < 4; row++ {
		for column := 0; column < 3; column++ {
			for i := 0; i < 3; i++ {
				combined[row*3+column] += local[row*3+i] * parent[i*3+column]
			}
		}
	}
	for i := 9; i < 12; i++ {
		combined[i] += parent[i]
	}
	return combined
}

type shelf3MFObjectKey struct {
	part string
	id   uint32
}

func (p *shelf3MFPackage) appendTriangles(root string, parentUnit float64, ref shelf3MFRef, parent modelTransform, active map[shelf3MFObjectKey]bool, visits *int, out *[]shelfTriangle) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	*visits += 1
	if len(active) >= shelf3MFMaxDepth || *visits > 2*shelfMeshMaxTriangles {
		return errShelfMeshLimit
	}
	key := shelf3MFObjectKey{ref.part, ref.id}
	if active[key] {
		return errors.New("cyclic 3MF component assembly")
	}
	model, err := p.load(ref.part, ref.part == root)
	if err != nil {
		return err
	}
	object := model.objects[ref.id]
	if object == nil {
		return fmt.Errorf("missing 3MF object %d", ref.id)
	}
	if len(object.triangles) == 0 && len(object.components) == 0 {
		return errShelfEmptyMesh
	}
	if len(*out)+len(object.triangles) > shelfMeshMaxTriangles {
		return errShelfMeshLimit
	}
	local := ref.transform
	for i := 9; i < 12; i++ {
		local[i] *= parentUnit
	}
	transform := shelfComposeTransform(parent, local)
	for _, value := range transform {
		if math.IsNaN(value) || math.Abs(value) > shelfMeshMaxCoordinate {
			return errors.New("invalid 3MF assembly transform")
		}
	}
	determinant := transform[0]*(transform[4]*transform[8]-transform[5]*transform[7]) -
		transform[1]*(transform[3]*transform[8]-transform[5]*transform[6]) +
		transform[2]*(transform[3]*transform[7]-transform[4]*transform[6])
	for _, indices := range object.triangles {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		var triangle shelfTriangle
		for i, index := range indices {
			point := transform.apply(object.vertices[index])
			if !shelfPointValid(point) {
				return errors.New("invalid 3MF transformed vertex coordinates")
			}
			triangle[i] = point
		}
		if determinant < 0 {
			triangle[1], triangle[2] = triangle[2], triangle[1]
		}
		*out = append(*out, triangle)
	}
	active[key] = true
	defer delete(active, key)
	for _, component := range object.components {
		if err := p.appendTriangles(root, model.unit, component, transform, active, visits, out); err != nil {
			return err
		}
	}
	return nil
}

var shelfSTEPWorker = make(chan struct{}, 1)

func readShelfSTEP(ctx context.Context, cfg Config, input *os.File, format string) ([]shelfTriangle, error) {
	if cfg.Tool == "" {
		return nil, errors.New("STEP geometry conversion is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	select {
	case shelfSTEPWorker <- struct{}{}:
		defer func() { <-shelfSTEPWorker }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	dir, err := os.MkdirTemp("", "orca-shelf-step-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "input"+format), filepath.Join(dir, "output.3mf")
	copyFile, err := os.OpenFile(in, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(copyFile, shelfContextReader{ctx: ctx, Reader: input})
	if err := errors.Join(copyErr, copyFile.Close()); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, cfg.Tool, "--export-3mf-generic", in, out)
	if cmd.Path != "" && !filepath.IsAbs(cmd.Path) {
		absolute, err := filepath.Abs(cmd.Path)
		if err != nil {
			return nil, err
		}
		cmd.Path = absolute
	}
	cmd.Dir = dir
	cmd.Env = append(engineEnv(dir), "OMP_NUM_THREADS=2")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	configureKill(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("STEP geometry conversion is unavailable: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-wait:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				return nil, fmt.Errorf("STEP geometry conversion failed: %w", err)
			}
			return readShelfSTEPOutput(ctx, out)
		case <-ticker.C:
			info, err := os.Lstat(out)
			if err == nil && (!info.Mode().IsRegular() || info.Size() > shelfMeshMaxBytes) {
				cancel()
				<-wait
				return nil, errShelfMeshLimit
			}
		}
	}
}

func readShelfSTEPOutput(ctx context.Context, output string) ([]shelfTriangle, error) {
	info, err := os.Lstat(output)
	if err != nil {
		return nil, errors.New("STEP conversion produced no 3MF geometry")
	}
	if !info.Mode().IsRegular() || info.Size() > shelfMeshMaxBytes {
		return nil, errShelfMeshLimit
	}
	f, err := os.Open(output)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readShelf3MF(ctx, f, info.Size())
}
