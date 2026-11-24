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
type shelfImportGroup struct {
	name    string
	files   []shelfImportFile
	folders []string
}
type shelfImportBatch struct {
	groups  []shelfImportGroup
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
	maxSegments := shelfMaxFolderDepth - 1
	if !dir {
		maxSegments++
	}
	if len(pieces) > maxSegments {
		return "", shelfProblem(400, "zip_depth", "The ZIP and its folders exceed 32 levels.")
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
	if err := b.addEntry(limits); err != nil {
		return err
	}
	base := strings.TrimSpace(strings.TrimSuffix(name, path.Ext(name)))
	if !shelfName(base) {
		base = name
	}
	group := shelfImportGroup{name: base}
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
			group.folders = append(group.folders, relative)
			continue
		}
		group.folders = append(group.folders, path.Dir(relative))
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
			group.files = append(group.files, shelfImportFile{relative: relative, source: source, file: dst, format: ext, size: n})
		} else {
			b.skipped = append(b.skipped, shelfSkipped{Path: source, Reason: "unsupported model format"})
		}
	}
	b.groups = append(b.groups, group)
	return nil
}

func (b *shelfImportBatch) prepareTree(parentDepth, limit int) error {
	entries := 0
	for i := range b.groups {
		group := &b.groups[i]
		entries += len(group.files)
		if group.name != "" {
			entries++
		}
		if entries > limit {
			return shelfProblem(413, "upload_tree_limit", "The expanded folder and model tree exceeds its entry limit.")
		}
		if group.name == "" {
			continue
		}
		if parentDepth >= shelfMaxFolderDepth {
			return shelfProblem(400, "folder_depth", "Import would exceed 32 folder levels.")
		}
		seen := map[string]bool{".": true}
		var folders []string
		var collect func(string) error
		collect = func(folder string) error {
			if seen[folder] {
				return nil
			}
			if parentDepth+2+strings.Count(folder, "/") > shelfMaxFolderDepth {
				return shelfProblem(400, "folder_depth", "Import would exceed 32 folder levels.")
			}
			if err := collect(path.Dir(folder)); err != nil {
				return err
			}
			entries++
			if entries > limit {
				return shelfProblem(413, "upload_tree_limit", "The expanded folder and model tree exceeds its entry limit.")
			}
			seen[folder] = true
			folders = append(folders, folder)
			return nil
		}
		for _, folder := range group.folders {
			if err := collect(folder); err != nil {
				return err
			}
		}
		group.folders = folders
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
			batch.groups = append(batch.groups, shelfImportGroup{files: []shelfImportFile{part.file}})
		}
	}
	if err = batch.prepareTree(current.depth(parent), limits.Entries); err != nil {
		shelfFail(w, err)
		return
	}
	for _, group := range batch.groups {
		for i := range group.files {
			if err := s.prepareShelfModel(r.Context(), &group.files[i]); err != nil {
				shelfFail(w, err)
				return
			}
		}
	}
	imported := []shelfImported{}
	created := []shelfFolder{}
	d, err := s.changeShelf(expected, func(d *shelfDocument) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if !d.parentExists(parent) {
			return shelfProblem(404, "folder_missing", "Shelf folder not found.")
		}
		names := newShelfSiblingNames(*d)
		createFolder := func(parentID, name string) string {
			f := shelfFolder{ID: newID("folder"), Parent: parentID, Name: names.reserve(parentID, name, "")}
			d.Folders[f.ID] = f
			created = append(created, f)
			return f.ID
		}
		for _, group := range batch.groups {
			root := parent
			if group.name != "" {
				root = createFolder(parent, group.name)
			}
			folders := map[string]string{".": root}
			for _, folder := range group.folders {
				folders[folder] = createFolder(folders[path.Dir(folder)], path.Base(folder))
			}
			for _, file := range group.files {
				if err := copyShelfBlob(file.file, s.blobs.path(file.sum)); err != nil {
					return err
				}
				if err := durableWrite(s.shelfThumbnailPath(file.sum, file.format), file.thumbnail); err != nil {
					return err
				}
				parentID, name := folders[path.Dir(file.relative)], path.Base(file.relative)
				model := shelfModel{ID: newID("model"), Parent: parentID, Name: names.reserve(parentID, name, path.Ext(name)), SHA256: file.sum, Format: file.format, Size: file.size}
				d.Models[model.ID] = model
				imported = append(imported, shelfImported{Path: file.source, Model: model, Preview: file.preview})
			}
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

func (s *Server) prepareShelfModel(ctx context.Context, f *shelfImportFile) error {
	var err error
	f.sum, err = mcpFileHash(f.file)
	if err != nil {
		return err
	}
	triangles, parseErr := readShelfMesh(ctx, s.cfg, f.file, f.format)
	if parseErr != nil && f.format != ".step" && f.format != ".stp" {
		return shelfProblem(400, "model_invalid", "A model contains invalid or unsupported geometry.")
	}
	if parseErr == nil {
		f.thumbnail, err = renderShelfThumbnail(triangles)
		if err == nil {
			f.preview = "rendered"
			return nil
		}
	}
	f.preview = "placeholder"
	f.thumbnail, err = shelfPlaceholderThumbnail()
	return err
}

func (s *Server) shelfThumbnailPath(sum, format string) string {
	return filepath.Join(s.library.dir, "shelf-thumbnails", sum+format+".v2.png")
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
