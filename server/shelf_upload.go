package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

type shelfLimits struct {
	RequestBytes, FileBytes, TotalBytes int64
	Entries                             int
	Ratio                               uint64
}

func shelfUploadLimits(c Config) shelfLimits {
	request := c.MaxUploadSize
	if request <= 0 {
		request = 512 << 20
	}
	request = min(request, 512<<20)
	return shelfLimits{RequestBytes: request, FileBytes: min(request, 128<<20), TotalBytes: 512 << 20, Entries: 2048, Ratio: 200}
}
func shelfModelFormat(ext string) bool {
	switch ext {
	case ".stl", ".obj", ".3mf", ".step", ".stp":
		return true
	}
	return false
}

type shelfUploadPart struct {
	archive bool
	name    string
	file    shelfImportFile
}

type shelfImportFile struct {
	relative, source, file, sum, format, preview string
	size                                         int64
	thumbnail                                    []byte
}
type shelfSkipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type shelfImported struct {
	Path    string     `json:"path"`
	Model   shelfModel `json:"model"`
	Preview string     `json:"preview"`
}
type shelfImportBatch struct {
	files   []shelfImportFile
	folders []string
	skipped []shelfSkipped
	total   int64
	entries int
}

func shelfArchivePath(name string, dir bool) (string, error) {
	if len(name) > 4096 || !utf8.ValidString(name) || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return "", shelfProblem(400, "zip_path_invalid", "ZIP entries must have safe relative paths.")
	}
	if dir {
		name = strings.TrimSuffix(name, "/")
	}
	pieces := strings.Split(name, "/")
	maxSegments := 32
	if !dir {
		maxSegments++
	}
	if len(pieces) > maxSegments {
		return "", shelfProblem(400, "zip_depth", "ZIP paths exceed 32 levels.")
	}
	for _, piece := range pieces {
		if !shelfName(piece) {
			return "", shelfProblem(400, "zip_path_invalid", "ZIP entries must have safe relative paths.")
		}
	}
	return strings.Join(pieces, "/"), nil
}
func (b *shelfImportBatch) addEntry(limits shelfLimits) error {
	b.entries++
	if b.entries > limits.Entries {
		return shelfProblem(413, "zip_entry_limit", "The upload contains too many entries.")
	}
	return nil
}
func (b *shelfImportBatch) readFile(ctx context.Context, src io.Reader, dst string, limits shelfLimits) (int64, error) {
	if limits.TotalBytes-b.total < 0 {
		return 0, shelfProblem(413, "upload_total_limit", "The upload exceeds its total expanded size limit.")
	}
	limit := min(limits.FileBytes, limits.TotalBytes-b.total)
	var out io.Writer = io.Discard
	var file *os.File
	var err error
	if dst != "" {
		file, err = os.Create(dst)
		if err != nil {
			return 0, err
		}
		out = file
	}
	n, err := io.Copy(out, io.LimitReader(mcpContextReader{ctx: ctx, reader: src}, limit+1))
	if file != nil {
		err = errors.Join(err, file.Close())
	}
	if n > limit {
		return 0, shelfProblem(413, "upload_size_limit", "A file or the total expanded upload exceeds its size limit.")
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return 0, shelfProblem(413, "upload_request_limit", "The upload exceeds the server request size limit.")
		}
		return 0, shelfProblem(400, "upload_invalid", "The upload is interrupted or has invalid archive contents.")
	}
	b.total += n
	return n, nil
}
func (b *shelfImportBatch) expandZIP(ctx context.Context, file, name, dir string, limits shelfLimits) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil {
		err = shelfZIPDirectory(f, info.Size(), limits.Entries-b.entries)
	}
	err = errors.Join(err, f.Close())
	if errors.Is(err, errShelfMeshLimit) {
		return shelfProblem(413, "zip_entry_limit", "The ZIP exceeds its entry count or directory size limit.")
	}
	if err != nil {
		return shelfProblem(400, "zip_invalid", "The ZIP archive is invalid.")
	}
	z, err := zip.OpenReader(file)
	if err != nil {
		return shelfProblem(400, "zip_invalid", "The ZIP archive is invalid.")
	}
	defer z.Close()
	if len(z.File) > limits.Entries-b.entries {
		return shelfProblem(413, "zip_entry_limit", "The upload contains too many ZIP entries.")
	}
	var declared uint64
	for _, f := range z.File {
		if err = b.addEntry(limits); err != nil {
			return err
		}
		relative, err := shelfArchivePath(f.Name, f.FileInfo().IsDir())
		if err != nil {
			return err
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 || !mode.IsRegular() && !mode.IsDir() {
			return shelfProblem(400, "zip_entry_type", "ZIP symlinks and special files are not allowed.")
		}
		if f.UncompressedSize64 > uint64(limits.FileBytes) || f.UncompressedSize64 > uint64(limits.TotalBytes)-declared {
			return shelfProblem(413, "zip_size_limit", "The ZIP exceeds its file or total expanded size limit.")
		}
		declared += f.UncompressedSize64
		if f.UncompressedSize64 > 0 && (f.CompressedSize64 == 0 || f.UncompressedSize64/f.CompressedSize64 > limits.Ratio || f.UncompressedSize64/f.CompressedSize64 == limits.Ratio && f.UncompressedSize64%f.CompressedSize64 > 0) {
			return shelfProblem(413, "zip_ratio_limit", "A ZIP entry exceeds the decompression ratio limit.")
		}
		if mode.IsDir() {
			b.folders = append(b.folders, relative)
			continue
		}
		ext := strings.ToLower(path.Ext(relative))
		keep := shelfModelFormat(ext)
		dst := ""
		if keep {
			dst = filepath.Join(dir, fmt.Sprintf("entry-%d%s", b.entries, ext))
		}
		r, err := f.Open()
		if err != nil {
			return shelfProblem(400, "zip_invalid", "A ZIP entry cannot be read.")
		}
		n, readErr := b.readFile(ctx, r, dst, limits)
		closeErr := r.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil || uint64(n) != f.UncompressedSize64 {
			return shelfProblem(400, "zip_invalid", "A ZIP entry has invalid contents or size.")
		}
		source := name + "/" + relative
		if keep {
			b.files = append(b.files, shelfImportFile{relative: relative, source: source, file: dst, format: ext, size: n})
		} else {
			b.skipped = append(b.skipped, shelfSkipped{Path: source, Reason: "unsupported model format"})
		}
	}
	return nil
}
func (s *Server) uploadShelf(w http.ResponseWriter, r *http.Request) {
	expected, err := shelfExpected(r)
	if err != nil {
		shelfFail(w, err)
		return
	}
	limits := shelfUploadLimits(s.cfg)
	r.Body = http.MaxBytesReader(w, r.Body, limits.RequestBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		shelfFail(w, shelfProblem(400, "multipart_required", "Upload model files or ZIP archives as multipart/form-data."))
		return
	}
	current, err := s.shelfSnapshot()
	if err != nil {
		shelfFail(w, err)
		return
	}
	if err = current.checkRevision(expected); err != nil {
		shelfFail(w, err)
		return
	}
	parent := r.URL.Query().Get("folder")
	if !current.parentExists(parent) {
		shelfFail(w, shelfProblem(404, "folder_missing", "Shelf folder not found."))
		return
	}
	dir, err := os.MkdirTemp("", "shelf-upload-*")
	if err != nil {
		shelfFail(w, err)
		return
	}
	defer os.RemoveAll(dir)
	batch := shelfImportBatch{skipped: []shelfSkipped{}}
	var parts []shelfUploadPart
	partCount := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				shelfFail(w, shelfProblem(413, "upload_request_limit", "The upload exceeds the server request size limit."))
			} else {
				shelfFail(w, shelfProblem(400, "multipart_invalid", "The multipart upload is invalid."))
			}
			return
		}
		partCount++
		if partCount > limits.Entries {
			shelfFail(w, shelfProblem(413, "upload_entry_limit", "The upload contains too many parts."))
			return
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		name := params["filename"]
		if err != nil || !shelfName(name) {
			shelfFail(w, shelfProblem(400, "upload_name_invalid", "Each part must contain a file with a safe basename."))
			return
		}
		ext := strings.ToLower(filepath.Ext(name))
		dst := filepath.Join(dir, fmt.Sprintf("part-%d%s", partCount, ext))
		if ext == ".zip" {
			f, err := os.Create(dst)
			if err != nil {
				shelfFail(w, err)
				return
			}
			n, copyErr := io.Copy(f, io.LimitReader(part, limits.RequestBytes+1))
			copyErr = errors.Join(copyErr, f.Close())
			if n > limits.RequestBytes {
				shelfFail(w, shelfProblem(413, "upload_request_limit", "The ZIP exceeds the request size limit."))
				return
			}
			if copyErr != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(copyErr, &tooLarge) {
					shelfFail(w, shelfProblem(413, "upload_request_limit", "The upload exceeds the request size limit."))
				} else {
					shelfFail(w, shelfProblem(400, "upload_invalid", "The upload did not finish."))
				}
				return
			}
			parts = append(parts, shelfUploadPart{archive: true, name: name, file: shelfImportFile{file: dst}})
		} else {
			if err = batch.addEntry(limits); err != nil {
				shelfFail(w, err)
				return
			}
			target := dst
			if !shelfModelFormat(ext) {
				target = ""
			}
			n, err := batch.readFile(r.Context(), part, target, limits)
			if err != nil {
				shelfFail(w, err)
				return
			}
			if target == "" {
				batch.skipped = append(batch.skipped, shelfSkipped{Path: name, Reason: "unsupported model format"})
			} else {
				parts = append(parts, shelfUploadPart{file: shelfImportFile{relative: name, source: name, file: dst, format: ext, size: n}})
			}
		}
		if err = part.Close(); err != nil {
			shelfFail(w, shelfProblem(400, "upload_invalid", "The upload did not finish."))
			return
		}
	}
	if partCount == 0 {
		shelfFail(w, shelfProblem(400, "upload_empty", "At least one file is required."))
		return
	}
	for _, part := range parts {
		if part.archive {
			if err = batch.expandZIP(r.Context(), part.file.file, part.name, dir, limits); err != nil {
				shelfFail(w, err)
				return
			}
		} else {
			batch.files = append(batch.files, part.file)
		}
	}
	paths := append([]string{}, batch.folders...)
	for _, file := range batch.files {
		paths = append(paths, path.Dir(file.relative))
	}
	implicit := map[string]bool{}
	for _, folder := range paths {
		for folder != "." && folder != "" {
			implicit[folder] = true
			if len(implicit)+len(batch.files) > limits.Entries {
				shelfFail(w, shelfProblem(413, "upload_tree_limit", "The expanded folder and model tree exceeds its entry limit."))
				return
			}
			folder = path.Dir(folder)
		}
	}
	for i := range batch.files {
		f := &batch.files[i]
		sum, err := mcpFileHash(f.file)
		if err != nil {
			shelfFail(w, err)
			return
		}
		f.sum = sum
		triangles, parseErr := readShelfMesh(r.Context(), s.cfg, f.file, f.format)
		if parseErr != nil {
			if f.format != ".step" && f.format != ".stp" {
				shelfFail(w, shelfProblem(400, "model_invalid", "A model contains invalid or unsupported geometry."))
				return
			}
			f.thumbnail, err = shelfPlaceholderThumbnail()
			f.preview = "placeholder"
		} else {
			f.thumbnail, err = renderShelfThumbnail(triangles)
			f.preview = "rendered"
			if err != nil {
				f.thumbnail, err = shelfPlaceholderThumbnail()
				f.preview = "placeholder"
			}
		}
		if err != nil {
			shelfFail(w, err)
			return
		}
	}
	imported := make([]shelfImported, 0, len(batch.files))
	created := []shelfFolder{}
	d, err := s.changeShelf(expected, func(d *shelfDocument) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if !d.parentExists(parent) {
			return shelfProblem(404, "folder_missing", "Shelf folder not found.")
		}
		names := newShelfSiblingNames(*d)
		folders := map[string]string{"": parent}
		var ensure func(string) (string, error)
		ensure = func(relative string) (string, error) {
			if id, ok := folders[relative]; ok {
				return id, nil
			}
			above := path.Dir(relative)
			if above == "." {
				above = ""
			}
			parentID, err := ensure(above)
			if err != nil {
				return "", err
			}
			name := path.Base(relative)
			if id := names.folders[parentID][shelfFoldName(name)]; id != "" {
				folders[relative] = id
				return id, nil
			}
			if d.depth(parentID) >= 32 {
				return "", shelfProblem(400, "folder_depth", "Import would exceed 32 folder levels.")
			}
			f := shelfFolder{ID: newID("folder"), Parent: parentID, Name: names.reserve(parentID, name)}
			names.addFolder(f)
			d.Folders[f.ID] = f
			folders[relative] = f.ID
			created = append(created, f)
			return f.ID, nil
		}
		for _, folder := range batch.folders {
			if _, err := ensure(folder); err != nil {
				return err
			}
		}
		for _, file := range batch.files {
			folder := path.Dir(file.relative)
			if folder == "." {
				folder = ""
			}
			parentID, err := ensure(folder)
			if err != nil {
				return err
			}
			if err = copyShelfBlob(file.file, s.blobs.path(file.sum)); err != nil {
				return err
			}
			if err = durableWrite(s.shelfThumbnailPath(file.sum, file.format), file.thumbnail); err != nil {
				return err
			}
			model := shelfModel{ID: newID("model"), Parent: parentID, Name: names.reserve(parentID, path.Base(file.relative)), SHA256: file.sum, Format: file.format, Size: file.size}
			d.Models[model.ID] = model
			imported = append(imported, shelfImported{Path: file.source, Model: model, Preview: file.preview})
		}
		return nil
	})
	if err != nil {
		shelfFail(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(d.Revision, 10)))
	writeJSON(w, 201, map[string]any{"revision": d.Revision, "imported": imported, "skipped": batch.skipped, "folders": created})
	go s.blobs.evict()
}
func (s *Server) shelfThumbnailPath(sum, format string) string {
	return filepath.Join(s.library.dir, "shelf-thumbnails", sum+format+".png")
}
func (s *Server) shelfThumbnail(w http.ResponseWriter, r *http.Request) {
	model, file, cleanup, err := s.shelfSource(r.PathValue("id"), nil)
	if err != nil {
		shelfFail(w, err)
		return
	}
	defer cleanup()
	cached := s.shelfThumbnailPath(model.SHA256, model.Format)
	data, err := os.ReadFile(cached)
	if errors.Is(err, os.ErrNotExist) {
		triangles, parseErr := readShelfMesh(r.Context(), s.cfg, file, model.Format)
		if parseErr != nil && model.Format != ".step" && model.Format != ".stp" {
			shelfFail(w, shelfProblem(422, "thumbnail_unavailable", "The model preview could not be rendered."))
			return
		}
		if parseErr != nil {
			data, err = shelfPlaceholderThumbnail()
		} else {
			data, err = renderShelfThumbnail(triangles)
			if err != nil {
				data, err = shelfPlaceholderThumbnail()
			}
		}
		if err == nil {
			err = durableWrite(cached, data)
		}
	}
	if err != nil {
		shelfFail(w, err)
		return
	}
	sum := sha256.Sum256(data)
	etag := strconv.Quote(hex.EncodeToString(sum[:]))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", privateRevalidate)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	w.WriteHeader(200)
	w.Write(data)
}
