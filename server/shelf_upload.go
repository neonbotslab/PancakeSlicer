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

const (
	shelfUploadMaxBytes = 2 << 30
	shelfMaxUploadParts = 10_000
	shelfMaxZIPRecords  = 65_535
	shelfMaxTreeNodes   = 100_000
)

type shelfLimits struct {
	RequestBytes, FileBytes, TotalBytes int64
	Entries                             int
	Ratio                               uint64
}

func shelfUploadLimits(c Config) shelfLimits {
	payload := c.ShelfMaxUploadSize
	if payload <= 0 {
		payload = c.MaxUploadSize
	}
	if payload <= 0 {
		payload = shelfUploadMaxBytes
	}
	payload = min(payload, shelfUploadMaxBytes)
	return shelfLimits{RequestBytes: payload + (1 << 20), FileBytes: payload, TotalBytes: 8 << 30, Entries: 10_000, Ratio: 200}
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
	groups     []shelfImportGroup
	skipped    []shelfSkipped
	total      int64
	entries    int
	zipRecords int
}

type shelfZIPEntry struct {
	file     *zip.File
	relative string
	ignored  bool
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
func shelfIgnoredZIPPath(relative string) bool {
	for component := range strings.SplitSeq(relative, "/") {
		if strings.HasPrefix(component, ".") || component == "__MACOSX" {
			return true
		}
	}
	return false
}

func (b *shelfImportBatch) addEntries(n, limit int) error {
	b.entries += n
	if b.entries > limit {
		return shelfProblem(413, "zip_entry_limit", fmt.Sprintf("The upload contains %d files. The limit is %d.", b.entries, limit))
	}
	return nil
}
func readShelfUploadFile(ctx context.Context, src io.Reader, dst string, limit int64) (int64, error) {
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
		return 0, shelfProblem(413, "upload_size_limit", fmt.Sprintf("The file exceeds the remaining upload limit of %d bytes.", limit))
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return 0, shelfProblem(413, "upload_request_limit", fmt.Sprintf("The upload exceeds the %d-byte server request limit.", tooLarge.Limit))
		}
		return 0, shelfProblem(400, "upload_invalid", "The upload is interrupted or has invalid archive contents.")
	}
	return n, nil
}
func (b *shelfImportBatch) expandZIP(ctx context.Context, file, name, dir string, limits shelfLimits) error {
	base := strings.TrimSpace(strings.TrimSuffix(name, path.Ext(name)))
	if !shelfName(base) {
		base = name
	}
	group := shelfImportGroup{name: base}
	archive, err := os.Open(file)
	if err != nil {
		return err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	recordsRemaining := shelfMaxZIPRecords - b.zipRecords
	metadataLimitMessage := fmt.Sprintf("ZIP metadata exceeds the limit of %d records per upload or %d MiB per archive.", shelfMaxZIPRecords, shelfZIPDirectoryMaxBytes>>20)
	err = shelfZIPDirectory(archive, info.Size(), recordsRemaining)
	if errors.Is(err, errShelfMeshLimit) {
		return shelfProblem(413, "zip_metadata_limit", metadataLimitMessage)
	}
	if err != nil {
		return shelfProblem(400, "zip_invalid", "The ZIP archive is invalid.")
	}
	z, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return shelfProblem(400, "zip_invalid", "The ZIP archive is invalid.")
	}
	if len(z.File) > recordsRemaining {
		return shelfProblem(413, "zip_metadata_limit", metadataLimitMessage)
	}
	firstRecord := b.zipRecords
	b.zipRecords += len(z.File)
	entries := make([]shelfZIPEntry, 0, len(z.File))
	declared := uint64(b.total)
	eligibleCount := 0
	for _, f := range z.File {
		mode := f.Mode()
		relative, err := shelfArchivePath(f.Name, mode.IsDir())
		if err != nil {
			return err
		}
		if mode.Type() != 0 && mode.Type() != os.ModeDir {
			return shelfProblem(400, "zip_entry_type", "ZIP symlinks and special files are not allowed.")
		}
		if f.UncompressedSize64 > uint64(limits.FileBytes) {
			return shelfProblem(413, "zip_size_limit", fmt.Sprintf("A ZIP file exceeds the expanded file limit of %d bytes.", limits.FileBytes))
		}
		if f.UncompressedSize64 > uint64(limits.TotalBytes)-declared {
			return shelfProblem(413, "zip_size_limit", fmt.Sprintf("ZIP expansion exceeds the upload limit of %d bytes.", limits.TotalBytes))
		}
		declared += f.UncompressedSize64
		if f.UncompressedSize64 > 0 && (f.CompressedSize64 == 0 || f.UncompressedSize64/f.CompressedSize64 > limits.Ratio || f.UncompressedSize64/f.CompressedSize64 == limits.Ratio && f.UncompressedSize64%f.CompressedSize64 > 0) {
			return shelfProblem(413, "zip_ratio_limit", fmt.Sprintf("A ZIP entry exceeds the %d:1 decompression ratio limit.", limits.Ratio))
		}
		ignored := shelfIgnoredZIPPath(relative)
		if mode.IsDir() {
			if !ignored {
				group.folders = append(group.folders, relative)
			}
			continue
		}
		if !ignored {
			eligibleCount++
			group.folders = append(group.folders, path.Dir(relative))
		}
		entries = append(entries, shelfZIPEntry{file: f, relative: relative, ignored: ignored})
	}
	if err := b.addEntries(eligibleCount, limits.Entries); err != nil {
		return err
	}
	for i, entry := range entries {
		f, relative := entry.file, entry.relative
		ext := strings.ToLower(path.Ext(relative))
		keep := !entry.ignored && shelfModelFormat(ext)
		dst := ""
		if keep {
			dst = filepath.Join(dir, fmt.Sprintf("entry-%d%s", firstRecord+i, ext))
		}
		r, err := f.Open()
		if err != nil {
			return shelfProblem(400, "zip_invalid", "A ZIP entry cannot be read.")
		}
		n, readErr := readShelfUploadFile(ctx, r, dst, min(limits.FileBytes, limits.TotalBytes-b.total))
		closeErr := r.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil || uint64(n) != f.UncompressedSize64 {
			return shelfProblem(400, "zip_invalid", "A ZIP entry has invalid contents or size.")
		}
		b.total += n
		if entry.ignored {
			continue
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
	limitMessage := fmt.Sprintf("The Shelf tree exceeds the limit of %d folders and models per upload.", limit)
	for i := range b.groups {
		group := &b.groups[i]
		entries += len(group.files)
		if group.name != "" {
			entries++
		}
		if entries > limit {
			return shelfProblem(413, "upload_tree_limit", limitMessage)
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
				return shelfProblem(413, "upload_tree_limit", limitMessage)
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
	requestLimitMessage := fmt.Sprintf("The upload exceeds the %d-byte server request limit.", limits.RequestBytes)
	if r.ContentLength > limits.RequestBytes {
		shelfFail(w, shelfProblem(413, "upload_request_limit", requestLimitMessage))
		return
	}
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
	payloadRemaining := limits.FileBytes
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				shelfFail(w, shelfProblem(413, "upload_request_limit", requestLimitMessage))
			} else {
				shelfFail(w, shelfProblem(400, "multipart_invalid", "The multipart upload is invalid."))
			}
			return
		}
		partCount++
		if partCount > shelfMaxUploadParts {
			shelfFail(w, shelfProblem(413, "upload_entry_limit", fmt.Sprintf("The upload exceeds the limit of %d multipart parts.", shelfMaxUploadParts)))
			return
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		name := params["filename"]
		if err != nil || !shelfName(name) {
			shelfFail(w, shelfProblem(400, "upload_name_invalid", "Each part must contain a file with a safe basename."))
			return
		}
		ext := strings.ToLower(filepath.Ext(name))
		archive := ext == ".zip"
		if !archive {
			if err = batch.addEntries(1, limits.Entries); err != nil {
				shelfFail(w, err)
				return
			}
		}
		dst := ""
		if archive || shelfModelFormat(ext) {
			dst = filepath.Join(dir, fmt.Sprintf("part-%d%s", partCount, ext))
		}
		n, err := readShelfUploadFile(r.Context(), part, dst, payloadRemaining)
		if err != nil {
			shelfFail(w, err)
			return
		}
		payloadRemaining -= n
		if archive {
			parts = append(parts, shelfUploadPart{archive: true, name: name, file: shelfImportFile{file: dst}})
		} else {
			batch.total += n
			if dst == "" {
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
	if err = batch.prepareTree(current.depth(parent), shelfMaxTreeNodes); err != nil {
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
