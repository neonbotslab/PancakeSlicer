package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func shelfMeshTestFile(t *testing.T, data []byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func shelfMeshTest3MF(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range entries {
		part, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

const shelfMeshFixtureObject = `<object id="1"><mesh><vertices>
		<vertex x="0" y="0" z="0"/><vertex x="1" y="0" z="0"/><vertex x="0" y="1" z="0"/>
		</vertices><triangles><triangle v1="0" v2="1" v3="2"/></triangles></mesh></object>`

const shelfMeshFixture3MF = `<model unit="millimeter"><resources>` + shelfMeshFixtureObject + `</resources><build>
		 <item objectid="1"/></build></model>`

func TestShelfMeshReadsSTLAndCanonicalizesPlacement(t *testing.T) {
	ascii := "solid model\nfacet normal 0 0 1\nouter loop\nvertex 10 20 30\nvertex 18 20 34\nvertex 10 26 32\nendloop\nendfacet\nendsolid model\n"
	expected := shelfTriangle{{10, 20, 30}, {18, 20, 34}, {10, 26, 32}}
	binarySTL := make([]byte, 134)
	copy(binarySTL, "solid binary header")
	binary.LittleEndian.PutUint32(binarySTL[80:], 1)
	for i, point := range expected {
		for j, coordinate := range point {
			binary.LittleEndian.PutUint32(binarySTL[96+(i*3+j)*4:], math.Float32bits(float32(coordinate)))
		}
	}
	for name, data := range map[string][]byte{
		"ASCII LF":   []byte(ascii),
		"ASCII CRLF": []byte(strings.ReplaceAll(ascii, "\n", "\r\n")),
		"ASCII CR":   []byte(strings.ReplaceAll(ascii, "\n", "\r")),
		"binary":     binarySTL,
	} {
		t.Run(name, func(t *testing.T) {
			triangles, err := readShelfMesh(t.Context(), Config{}, shelfMeshTestFile(t, data), ".stl")
			if err != nil || len(triangles) != 1 || triangles[0] != expected {
				t.Fatalf("parsed %v, %v", triangles, err)
			}
			canonical, center, err := shelfSTL(triangles)
			if err != nil || center != (meshPoint{14, 23, 32}) {
				t.Fatalf("canonical STL center %v, %v", center, err)
			}
			result, err := readShelfMesh(t.Context(), Config{}, shelfMeshTestFile(t, canonical), ".stl")
			placed := shelfTriangle{{-4, -3, 0}, {4, -3, 4}, {-4, 3, 2}}
			if err != nil || len(result) != 1 || result[0] != placed || triangles[0] != expected {
				t.Fatalf("canonical STL has %v, %v", result, err)
			}
		})
	}
}

func TestShelfMeshTriangulatesConcaveOBJFaces(t *testing.T) {
	obj := `# Concave pentagon
v 0 0 0
v 4 0 0
v 4 4 0
v 2 2 0
v 0 4 0
f -5/1/1 -4//1 -3 -2/4 -1 # vertices relative to the latest vertex
`
	triangles, err := readShelfMesh(t.Context(), Config{Tool: "unconfigured-test-engine"}, shelfMeshTestFile(t, []byte(obj)), ".obj")
	if err != nil || len(triangles) != 3 {
		t.Fatalf("expected three triangles, got %v, %v", triangles, err)
	}
	var area float64
	for _, triangle := range triangles {
		a, b, c := triangle[0], triangle[1], triangle[2]
		cross := (b[0]-a[0])*(c[1]-a[1]) - (b[1]-a[1])*(c[0]-a[0])
		if cross <= 0 {
			t.Fatalf("reversed or degenerate triangle %v", triangle)
		}
		area += cross / 2
	}
	if area != 12 {
		t.Fatalf("polygon area was %f", area)
	}
}

func TestShelfMeshUses3MFBuildAndComponentTransforms(t *testing.T) {
	root := `<model xmlns:p="http://schemas.microsoft.com/3dmanufacturing/production/2015/06" unit="centimeter">
	<resources>` + shelfMeshFixtureObject + `
		<object id="5"><components>
			<component objectid="1" transform="1 0 0 0 1 0 0 0 1 1 2 3"/>
			<component objectid="1" p:path="/3D/external%20part.model" transform="-1 0 0 0 1 0 0 0 1 0 0 0"/>
		</components></object>
	</resources>
	<build><item objectid="5" transform="0 1 0 -1 0 0 0 0 1 2 3 4"/>
		<item objectid="1" p:path="/3D/external%20part.model"/>
	</build>
</model>`
	external := `<model unit="inch"><resources>` + shelfMeshFixtureObject + `</resources><build><item objectid="99"/></build></model>`
	data := shelfMeshTest3MF(t, map[string]string{
		"_rels/.rels":   `<Relationships><Relationship Type="http://schemas.microsoft.com/3dmanufacturing/2013/01/3dmodel" Target="/3D/root.model"/></Relationships>`,
		"3D/root.model": "\ufeff" + root, "3D/external part.model": external,
	})
	triangles, err := readShelfMesh(t.Context(), Config{}, shelfMeshTestFile(t, data), ".3mf")
	if err != nil || len(triangles) != 3 {
		t.Fatalf("expected three placed triangles, got %v, %v", triangles, err)
	}
	expected := []shelfTriangle{
		{{0, 40, 70}, {0, 50, 70}, {-10, 40, 70}},
		{{20, 30, 40}, {-5.4, 30, 40}, {20, 4.6, 40}},
		{{0, 0, 0}, {25.4, 0, 0}, {0, 25.4, 0}},
	}
	for i, triangle := range triangles {
		for j, point := range triangle {
			for k, coordinate := range point {
				if math.Abs(coordinate-expected[i][j][k]) > 1e-9 {
					t.Fatalf("triangle %d was %v, want %v", i, triangle, expected[i])
				}
			}
		}
	}
}

func shelfMeshTestZIP64(data []byte, saturated bool) []byte {
	endAt := len(data) - 22
	originalEnd := append([]byte(nil), data[endAt:]...)
	end := make([]byte, 56)
	copy(end, "PK\x06\x06")
	binary.LittleEndian.PutUint64(end[4:], 44)
	binary.LittleEndian.PutUint64(end[24:], uint64(binary.LittleEndian.Uint16(originalEnd[8:])))
	binary.LittleEndian.PutUint64(end[32:], uint64(binary.LittleEndian.Uint16(originalEnd[10:])))
	binary.LittleEndian.PutUint64(end[40:], uint64(binary.LittleEndian.Uint32(originalEnd[12:])))
	binary.LittleEndian.PutUint64(end[48:], uint64(binary.LittleEndian.Uint32(originalEnd[16:])))
	locator := make([]byte, 20)
	copy(locator, "PK\x06\x07")
	binary.LittleEndian.PutUint64(locator[8:], uint64(endAt))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	if saturated {
		for i := 8; i < 20; i++ {
			originalEnd[i] = 0xff
		}
	}
	return append(append(append([]byte(nil), data[:endAt]...), end...), append(locator, originalEnd...)...)
}

func TestShelfMeshAcceptsZIP64Geometry(t *testing.T) {
	data := shelfMeshTest3MF(t, map[string]string{"3D/3dmodel.model": shelfMeshFixture3MF})
	for _, saturated := range []bool{false, true} {
		fixture := shelfMeshTestZIP64(data, saturated)
		triangles, err := readShelfMesh(t.Context(), Config{}, shelfMeshTestFile(t, fixture), ".3mf")
		if err != nil || len(triangles) != 1 || triangles[0] != (shelfTriangle{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}) {
			t.Fatalf("ZIP64 saturated=%v, got %v, %v", saturated, triangles, err)
		}
	}
}

func TestShelfMeshRejectsHostileGeometry(t *testing.T) {
	binarySTL := make([]byte, 134)
	binary.LittleEndian.PutUint32(binarySTL[80:], 1)
	binary.LittleEndian.PutUint32(binarySTL[96:], math.Float32bits(float32(math.Inf(1))))
	mf := func(model string) []byte {
		return shelfMeshTest3MF(t, map[string]string{"3D/3dmodel.model": model})
	}
	cycle := `<model><resources><object id="1"><components><component objectid="2"/></components></object>
		<object id="2"><components><component objectid="1"/></components></object>
		</resources><build><item objectid="1"/></build></model>`
	badIndex := strings.Replace(shelfMeshFixture3MF, `v3="2"`, `v3="3"`, 1)
	badTransform := strings.Replace(shelfMeshFixture3MF, `<item objectid="1"/>`,
		`<item objectid="1" transform="1 0 0 0 1 0 0 0 1 1000000 0 0"/>`, 1)
	traversal := strings.Replace(shelfMeshFixture3MF, `<item objectid="1"/>`, `<item objectid="1" path="/../safe"/>`, 1)
	xmlBomb := strings.Replace(shelfMeshFixture3MF, "<resources>", "<!--"+strings.Repeat("a", 1<<20)+"--><resources>", 1)
	for _, tt := range []struct {
		name, format string
		data         []byte
	}{
		{"non-finite binary STL", ".stl", binarySTL},
		{"empty STL", ".stl", []byte("solid empty\nendsolid empty\n")},
		{"unbounded OBJ coordinate", ".obj", []byte("v 0 0 1000001\n")},
		{"non-finite OBJ coordinate", ".obj", []byte("v 0 NaN 0\n")},
		{"OBJ bad index", ".obj", []byte("v 0 0 0\nf 1 1 -2\n")},
		{"3MF cycle", ".3mf", mf(cycle)},
		{"3MF bad index", ".3mf", mf(badIndex)},
		{"3MF transformed outside bounds", ".3mf", mf(badTransform)},
		{"3MF traversing package path", ".3mf", mf(traversal)},
		{"3MF expansion ratio", ".3mf", mf(xmlBomb)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if result, err := readShelfMesh(t.Context(), Config{}, shelfMeshTestFile(t, tt.data), tt.format); err == nil {
				t.Fatalf("accepted invalid geometry %v", result)
			}
		})
	}
}

func TestShelfMeshRejectsBinarySTLAboveTriangleLimit(t *testing.T) {
	input := shelfMeshTestFile(t, make([]byte, 84))
	file, err := os.OpenFile(input, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	const count = 1_000_001
	var recordCount [4]byte
	binary.LittleEndian.PutUint32(recordCount[:], count)
	if _, err := file.WriteAt(recordCount[:], 80); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(84 + count*50); err != nil {
		t.Fatal(err)
	}
	if _, err := readShelfMesh(t.Context(), Config{}, input, ".stl"); !errors.Is(err, errShelfMeshLimit) {
		t.Fatalf("expected a triangle limit error, got %v", err)
	}
}

func TestShelfMeshSTEPUsesGeometryConversion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture tool uses a Unix shell")
	}
	dir := t.TempDir()
	fixture := shelfMeshTestZIP64(shelfMeshTest3MF(t, map[string]string{"3D/3dmodel.model": shelfMeshFixture3MF}), false)
	if err := os.WriteFile(filepath.Join(dir, "fixture.3mf"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(dir, "engine")
	script := "#!/bin/sh\n[ \"$#\" = 3 ] && [ \"$1\" = --export-3mf-generic ] || exit 1\n" +
		"case \"$2\" in *.stp) ;; *) exit 2 ;; esac\n" +
		"grep -q '^ISO-10303-21;' \"$2\" || exit 3\n" +
		"cp \"$(dirname \"$0\")/fixture.3mf\" \"$3\"\n"
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	source := shelfMeshTestFile(t, []byte("ISO-10303-21;\n"))
	triangles, err := readShelfMesh(t.Context(), Config{Tool: tool}, source, ".stp")
	if err != nil || len(triangles) != 1 || triangles[0] != (shelfTriangle{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}) {
		t.Fatalf("STEP conversion returned %v, %v", triangles, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readShelfMesh(ctx, Config{Tool: tool}, source, ".stp"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled conversion returned %v", err)
	}
	oversized := "#!/bin/sh\ndd if=/dev/null of=\"$3\" bs=1 seek=134217729 2>/dev/null\n"
	if err := os.WriteFile(tool, []byte(oversized), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := readShelfMesh(t.Context(), Config{Tool: tool}, source, ".stp"); !errors.Is(err, errShelfMeshLimit) {
		t.Fatalf("oversized conversion output returned %v", err)
	}
}
