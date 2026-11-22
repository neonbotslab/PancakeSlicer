package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpClientHeader = "X-Pancake-MCP-Client"

type mcpService struct {
	server               *Server
	now                  func() time.Time
	ttl                  time.Duration
	mu                   sync.Mutex
	prepared             map[string]*mcpPreparedPrint
	preparedShelfDeletes map[string]mcpPreparedShelfDelete
	preparing            int
	copies               chan struct{}
}

type mcpPreparedPrint struct {
	session      string
	expires      time.Time
	job          *Job
	printer      *PrinterConfig
	request      sendRequest
	artifactHash string
	nozzleHash   string
	jobHash      string
	digest       string
	summary      map[string]any
}

type mcpPrepareInput struct {
	JobID                string `json:"job_id" jsonschema:"Completed slice job ID"`
	Printer              string `json:"printer" jsonschema:"Saved printer setup ID returned by list_printers"`
	Filename             string `json:"filename,omitempty"`
	BedLeveling          bool   `json:"bed_leveling"`
	FlowCalibration      string `json:"flow_calibration" jsonschema:"auto, on, or off"`
	VibrationCalibration bool   `json:"vibration_calibration"`
	Timelapse            bool   `json:"timelapse"`
}

type mcpConfirmInput struct {
	Token         string `json:"token"`
	Digest        string `json:"digest" jsonschema:"Exact digest returned by prepare_print"`
	HumanApproved bool   `json:"human_approved" jsonschema:"True only after the human explicitly approves the entire prepared summary"`
}

func validMCPOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func (s *Server) mcpHandler() http.Handler {
	if !s.cfg.MCPEnabled {
		return http.NotFoundHandler()
	}
	return newMCPService(s).handler()
}

func (service *mcpService) handler() http.Handler {
	s := service.server
	protocol := mcp.NewServer(&mcp.Implementation{Name: "PancakeSlicer", Version: "1.0.0"}, nil)
	service.addDataTools(protocol)
	service.addSettingsTools(protocol)
	service.addShelfTools(protocol)
	mcpTool(protocol, "prepare_print", "Validate a completed slice and prepare a print without sending it. Show the FULL returned summary to the human and obtain explicit approval before confirm_print. Stores approval metadata and a short-lived single-use token. No printer upload occurs.", false, service.preparePrint)
	mcpTool(protocol, "confirm_print", "Send and start ONLY the exact prepared print after the human explicitly approves its full summary. Requires the token, exact digest, and human_approved=true. Tokens are session-bound, expire, and are consumed on every confirmation attempt. Elicitation must also be accepted when supported. Never call automatically or infer approval.", false, service.confirmPrint)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return protocol }, &mcp.StreamableHTTPOptions{SessionTimeout: 15 * time.Minute})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins, present := r.Header["Origin"]
		if present && (len(origins) != 1 || !validMCPOrigin(origins[0]) || !slices.Contains(s.cfg.MCPOrigins, origins[0])) {
			http.Error(w, "MCP origin refused", http.StatusForbidden)
			return
		}
		if s.cfg.MCPToken != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.cfg.MCPToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "MCP bearer token required", http.StatusUnauthorized)
			return
		}
		if s.cfg.BearerToken != "" {
			check := r
			if s.cfg.MCPToken != "" && s.cfg.MCPToken != s.cfg.BearerToken {
				check = r.Clone(r.Context())
				check.Header = r.Header.Clone()
				check.Header.Del("Authorization")
			}
			if !s.authorized(check) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "Server access token or access cookie required", http.StatusUnauthorized)
				return
			}
		}
		r = r.Clone(r.Context())
		r.Header = r.Header.Clone()
		r.Header.Set(mcpClientHeader, s.clientIP(r))
		limit := int64(base64.StdEncoding.EncodedLen(int(s.cfg.MaxUploadSize))) + (1 << 20)
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		w.Header().Set("Cache-Control", "no-store")
		transport.ServeHTTP(w, r)
	})
}

func newMCPService(s *Server) *mcpService {
	ttl := s.cfg.MCPConfirmationTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &mcpService{
		server: s, now: time.Now, ttl: ttl, prepared: make(map[string]*mcpPreparedPrint),
		preparedShelfDeletes: make(map[string]mcpPreparedShelfDelete), copies: make(chan struct{}, 2),
	}
}

func mcpTool[In any](server *mcp.Server, name, description string, readOnly bool, handler func(context.Context, *mcp.CallToolRequest, In) (map[string]any, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}}, func(ctx context.Context, request *mcp.CallToolRequest, in In) (*mcp.CallToolResult, map[string]any, error) {
		out, err := handler(ctx, request, in)
		return nil, out, err
	})
}

type mcpResponse struct {
	header http.Header
	bytes.Buffer
	status int
}

func (w *mcpResponse) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *mcpResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *mcpResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.Buffer.Write(p)
}
func (*mcpResponse) SetReadDeadline(time.Time) error { return nil }
func (w *mcpResponse) result() (map[string]any, error) {
	if w.status >= 400 {
		return nil, fmt.Errorf("request refused (%d): %s", w.status, w.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Bytes(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func mcpHTTPRequest(ctx context.Context, call *mcp.CallToolRequest, method, path string, body io.Reader) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, method, path, body)
	r.RemoteAddr = "mcp"
	if call != nil && call.Extra != nil {
		r.RemoteAddr = call.Extra.Header.Get(mcpClientHeader)
	}
	return r
}

func mcpHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func mcpFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type mcpContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r mcpContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (m *mcpService) snapshotArtifact(ctx context.Context, j *Job) (string, string, int64, error) {
	select {
	case m.copies <- struct{}{}:
		defer func() { <-m.copies }()
	case <-ctx.Done():
		return "", "", 0, ctx.Err()
	}
	limit := m.server.cfg.MaxUploadSize
	if limit <= 0 {
		limit = 512 << 20
	}
	if quota := m.server.cfg.DiskQuota; quota > 0 {
		used, err := directoryBytes(m.server.cfg.JobRoot)
		if err != nil {
			return "", "", 0, err
		}
		limit = min(limit, max(0, quota-used))
	}
	source, err := os.Open(j.OutputPath(j.Request.Output))
	if err != nil {
		return "", "", 0, errors.New("Sliced artifact is unavailable")
	}
	defer source.Close()
	f, err := os.CreateTemp(filepath.Join(j.Dir, "tmp"), ".mcp-print-*")
	if err != nil {
		return "", "", 0, errors.New("Cannot create prepared artifact")
	}
	path := f.Name()
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(path)
		}
	}()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hash), mcpContextReader{ctx, io.LimitReader(source, limit+1)})
	if err != nil {
		return "", "", 0, err
	}
	if size > limit {
		return "", "", 0, errors.New("Prepared artifact exceeds upload or disk quota limits")
	}
	if err := f.Close(); err != nil {
		return "", "", 0, err
	}
	if err := os.Chmod(path, 0400); err != nil {
		return "", "", 0, err
	}
	if quota := m.server.cfg.DiskQuota; quota > 0 {
		used, err := directoryBytes(m.server.cfg.JobRoot)
		if err != nil || used > quota {
			return "", "", 0, errors.New("Prepared artifact exceeds disk quota")
		}
	}
	keep = true
	return path, hex.EncodeToString(hash.Sum(nil)), size, nil
}

func cleanupMCPArtifacts(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "tmp", ".mcp-print-*"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *mcpService) retainJob(j *Job) (func(), error) {
	jobs := m.server.jobs
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if jobs.jobs[j.ID] != j || jobs.closing {
		return nil, errors.New("Slice job is no longer available")
	}
	j.mu.Lock()
	j.artifactReaders++
	j.mu.Unlock()
	return func() { j.mu.Lock(); j.artifactReaders--; j.mu.Unlock() }, nil
}

func (m *mcpService) cleanupLocked() {
	for token, p := range m.prepared {
		if !m.now().Before(p.expires) {
			delete(m.prepared, token)
		}
	}
}

func (m *mcpService) preparePrint(ctx context.Context, call *mcp.CallToolRequest, in mcpPrepareInput) (map[string]any, error) {
	m.mu.Lock()
	m.cleanupLocked()
	if len(m.prepared)+m.preparing >= 128 || m.preparing >= 2 {
		m.mu.Unlock()
		return nil, errors.New("Too many prepared prints. Retry after current preparations or tokens finish")
	}
	m.preparing++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.preparing--; m.mu.Unlock() }()
	s := m.server
	defer s.sends.hub.subscribe()()
	j := s.jobs.Get(in.JobID)
	if j == nil || j.View().LifecycleState != StateDone {
		return nil, errors.New("A completed slice job is required")
	}
	releaseJob, err := m.retainJob(j)
	if err != nil {
		return nil, err
	}
	defer releaseJob()
	p := s.sends.printer(in.Printer)
	if p == nil {
		return nil, errors.New("Printer is not connected")
	}
	if err := m.checkPrinter(p); err != nil {
		return nil, err
	}
	if in.FlowCalibration != "auto" && in.FlowCalibration != "on" && in.FlowCalibration != "off" {
		return nil, errors.New("flow_calibration must be auto, on, or off")
	}
	mode, _ := json.Marshal(in.FlowCalibration)
	body := sendRequest{Printer: in.Printer, Start: ptr(true), Filename: in.Filename, BedLeveling: &in.BedLeveling, FlowCaliMode: mode, Timelapse: in.Timelapse, VibrationCali: &in.VibrationCalibration}
	if p.Kind == "flashforge" {
		name := body.Filename
		if name == "" {
			name = j.DownloadName()
		}
		body.Filename = flashforgeFilename(name)
	}
	artifact, hash, size, err := m.snapshotArtifact(ctx, j)
	if err != nil {
		return nil, err
	}
	defer os.Remove(artifact)

	if p.Kind == "bambu-lan" {
		if _, err := mcpPackagePlate(artifact); err != nil {
			return nil, err
		}
		sliced, err := readBambuJobMapping(artifact)
		if err != nil {
			return nil, err
		}
		lib, err := s.libraryDocument()
		if err != nil {
			return nil, err
		}
		status, _ := s.sends.hub.current(in.Printer)
		plan, err := s.printerSendPlan(lib, sendPlanInput{Source: j.Request.Library, TargetID: in.Printer, Sliced: sliced, TargetLive: status})
		if err != nil {
			return nil, err
		}
		for _, slot := range plan.Slots {
			body.ExpectedPlan = append(body.ExpectedPlan, slot.Proof)
		}
	}
	r := mcpHTTPRequest(ctx, call, http.MethodPost, "/api/jobs/"+j.ID+"/send", nil)
	w := &mcpResponse{}
	validated := s.validateSend(w, r, j, body, artifact, false)
	if validated == nil {
		_, err := w.result()
		return nil, err
	}
	if validated.printer != p {
		return nil, errors.New("Printer connection changed during preparation")
	}
	originalHash, err := mcpFileHash(j.OutputPath(j.Request.Output))
	if err != nil || hash != originalHash {
		return nil, errors.New("Sliced artifact changed during preparation")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	expires := m.now().Add(m.ttl)
	view := j.View()
	numbers := versionSlice{}
	sliceNumbers(view.Slice, &numbers)
	var record map[string]any
	json.Unmarshal(view.Slice, &record)
	effectiveFlow := in.FlowCalibration
	if effectiveFlow == "auto" && !p.supportsAutoFlowCalibration() {
		effectiveFlow = "on"
	}
	if p.Kind != "bambu-lan" {
		effectiveFlow = "not_supported"
	}
	summary := map[string]any{
		"job_id":                     j.ID,
		"printer":                    in.Printer,
		"connection_id":              p.id,
		"printer_name":               p.Name,
		"printer_kind":               p.Kind,
		"filename":                   validated.filename,
		"artifact_sha256":            hash,
		"artifact_bytes":             size,
		"plate_index":                max(1, j.Request.PlateIndex),
		"source_plates":              j.Request.SourcePlates,
		"library":                    j.Request.Library,
		"settings":                   j.Request.Submission,
		"estimated_time_s":           numbers.TimeS,
		"filament_grams":             numbers.Grams,
		"warnings":                   record["warnings"],
		"flow_calibration":           in.FlowCalibration,
		"effective_flow_calibration": effectiveFlow,
		"bed_leveling":               in.BedLeveling,
		"vibration_calibration":      in.VibrationCalibration,
		"timelapse":                  in.Timelapse,
		"start":                      true,
		"mapping":                    validated.plan,
		"expires_at":                 expires,
	}
	summary["bed_type"] = mcpBedType(j.Request)
	effective := map[string]any{"bed_leveling": nil, "flow_calibration": effectiveFlow, "vibration_calibration": nil, "timelapse": nil}
	if p.Kind == "bambu-lan" || p.Kind == "elegoolink" {
		effective["bed_leveling"] = validated.options.bedLeveling
		effective["timelapse"] = validated.options.timelapse
	}
	if p.Kind == "bambu-lan" {
		effective["vibration_calibration"] = validated.options.vibrationCali
	}
	summary["effective_options"] = effective
	summary["source_project"] = j.Request.SourceProject
	if source := j.Request.SourceProject; source != nil {
		summary["plate_index"] = source.Plate
	}
	if p.Kind == "bambu-lan" {
		summary["dispatch_bed_type"] = "auto"
		summary["package_plate"], _ = mcpPackagePlate(artifact)
		summary["ams_mapping"] = validated.options.amsMapping
		summary["ams_mapping_v2"] = validated.options.amsMapping2
		summary["use_ams"] = validated.options.useAMS
	}
	if p.Kind == "bambu-lan" {
		mapping, _ := readBambuJobMapping(artifact)
		summary["nozzle_mapping"] = mapping
	}
	var nozzles []NozzleStatus
	if validated.status != nil {
		nozzles = validated.status.Nozzles
	}
	for _, n := range nozzles {
		if n.Abnormal && !n.Rack {
			return nil, errors.New("Printer reports an abnormal installed nozzle")
		}
	}
	summary["nozzles"] = nozzles
	prepared := &mcpPreparedPrint{session: call.Session.ID(), expires: expires, job: j, printer: p, request: body, artifactHash: hash, jobHash: mcpHash(j.Request), summary: summary}
	prepared.nozzleHash = mcpNozzleHash(nozzles)
	prepared.digest = mcpHash(struct {
		Summary map[string]any
		Request sendRequest
		JobHash string
	}{summary, body, prepared.jobHash})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	if len(m.prepared) >= 128 {
		return nil, errors.New("Too many prepared prints. Wait for approval tokens to expire")
	}
	m.prepared[token] = prepared
	time.AfterFunc(m.ttl, func() { m.mu.Lock(); defer m.mu.Unlock(); m.cleanupLocked() })
	return map[string]any{"token": token, "digest": prepared.digest, "summary": summary, "approval_required": true}, nil
}

func (m *mcpService) checkPrinter(p *PrinterConfig) error {
	status, ok := m.server.sends.hub.current(p.Printer)
	hub := m.server.sends.hub
	hub.mu.Lock()
	seen := status.At
	if last := hub.lastSeen[p.Printer]; last != nil && last.After(seen) {
		seen = *last
	}
	hub.mu.Unlock()
	if !ok || !status.Online || time.Since(seen) > 30*time.Second {
		return errors.New("A fresh online printer status is required. Retry list_printers first")
	}
	m.server.sends.mu.Lock()
	busy := m.server.sends.busy[p.Printer]
	m.server.sends.mu.Unlock()
	if busy {
		return errors.New("Printer is receiving another file")
	}
	if status.State != "idle" && status.State != "ready" && status.State != "finished" {
		return fmt.Errorf("Printer is not ready (%s)", status.State)
	}
	if _, err := p.secret(); err != nil {
		return err
	}
	return nil
}

func (m *mcpService) confirmPrint(ctx context.Context, call *mcp.CallToolRequest, in mcpConfirmInput) (map[string]any, error) {
	m.mu.Lock()
	p := m.prepared[in.Token]
	if p != nil && p.session == call.Session.ID() {
		delete(m.prepared, in.Token)
	} else {
		p = nil
	}
	m.cleanupLocked()
	m.mu.Unlock()
	if p == nil {
		return nil, errors.New("Unknown or already used confirmation token")
	}
	if !m.now().Before(p.expires) {
		return nil, errors.New("Confirmation token expired. Prepare again and obtain fresh approval")
	}
	if call.Session.ID() != p.session || in.Digest != p.digest {
		return nil, errors.New("Confirmation does not match the prepared print")
	}
	if !in.HumanApproved {
		return nil, errors.New("Explicit human approval is required")
	}
	if params := call.Session.InitializeParams(); params != nil && params.Capabilities != nil && params.Capabilities.Elicitation != nil {
		caps := params.Capabilities.Elicitation
		if caps.Form == nil && caps.URL != nil {
			return nil, errors.New("Client only supports URL elicitation. Print approval requires form elicitation")
		}
		summary, _ := json.MarshalIndent(p.summary, "", "  ")
		approvalCtx, cancel := context.WithTimeout(ctx, p.expires.Sub(m.now()))
		defer cancel()
		answer, err := call.Session.Elicit(approvalCtx, &mcp.ElicitParams{Message: "Approve sending and starting this exact print?\n" + string(summary), RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"approve": map[string]any{"type": "boolean", "title": "Send and start this print"}}, "required": []string{"approve"}}})
		if err != nil || answer == nil || answer.Action != "accept" || answer.Content["approve"] != true {
			return nil, errors.New("Human elicitation was declined, cancelled, or failed. Print refused")
		}
	}
	if !m.now().Before(p.expires) {
		return nil, errors.New("Confirmation token expired during approval")
	}
	s := m.server
	defer s.sends.hub.subscribe()()
	if s.jobs.Get(p.job.ID) != p.job || s.sends.printer(p.request.Printer) != p.printer || mcpHash(p.job.Request) != p.jobHash {
		return nil, errors.New("Prepared job or printer changed. Prepare again")
	}
	if err := m.checkPrinter(p.printer); err != nil {
		return nil, err
	}
	releaseJob, err := m.retainJob(p.job)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			releaseJob()
		}
	}()
	artifact, hash, _, err := m.snapshotArtifact(ctx, p.job)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !transferred {
			os.Remove(artifact)
		}
	}()
	if hash != p.artifactHash {
		return nil, errors.New("Prepared artifact mismatch. Prepare again")
	}

	r := mcpHTTPRequest(ctx, call, http.MethodPost, "/api/jobs/"+p.job.ID+"/send", nil)
	w := &mcpResponse{}
	validated := s.validateSend(w, r, p.job, p.request, artifact, false)
	if validated == nil {
		return w.result()
	}
	if validated.printer != p.printer {
		return nil, errors.New("Printer connection changed during confirmation")
	}
	if validated.status != nil && mcpNozzleHash(validated.status.Nozzles) != p.nozzleHash {
		return nil, errors.New("Prepared nozzle topology changed. Prepare again")
	}
	checkStart := validated.checkStart
	if checkStart != nil {
		validated.checkStart = func() error {
			if err := checkStart(); err != nil {
				return err
			}
			status, _ := s.sends.hub.current(p.request.Printer)
			if mcpNozzleHash(status.Nozzles) != p.nozzleHash {
				return errors.New("Approved nozzle topology changed during upload")
			}
			return nil
		}
	}
	f, err := os.Open(artifact)
	if err != nil {
		return nil, err
	}
	fileHash := sha256.New()
	if _, err := io.Copy(fileHash, f); err != nil || hex.EncodeToString(fileHash.Sum(nil)) != p.artifactHash {
		f.Close()
		return nil, errors.New("Prepared artifact mismatch")
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, err
	}
	if !m.now().Before(p.expires) {
		f.Close()
		return nil, errors.New("Confirmation token expired before dispatch")
	}
	validated.cleanup = func() { os.Remove(artifact); releaseJob() }
	s.launchSend(w, r, validated, f)
	transferred = w.status == http.StatusAccepted
	return w.result()
}

func mcpBedType(req JobRequest) any {
	for _, pairs := range [][][2]string{req.ProjectSettings, req.Overrides} {
		for _, kv := range pairs {
			if kv[0] == "curr_bed_type" {
				return kv[1]
			}
		}
	}
	if req.Submission != nil && req.Submission.Profiles.Printer != nil {
		return req.Submission.Profiles.Printer.Config["curr_bed_type"]
	}
	return nil
}

func mcpNozzleHash(nozzles []NozzleStatus) string {
	values := make([]string, 0, len(nozzles))
	for _, n := range nozzles {
		raw, _ := json.Marshal(n)
		values = append(values, string(raw))
	}
	slices.Sort(values)
	return mcpHash(values)
}

func mcpPackagePlate(path string) (string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer z.Close()
	entry := ""
	for _, f := range z.File {
		if plateEntry.MatchString(f.Name) {
			if entry != "" {
				return "", errors.New("Print package must contain exactly one G-code plate")
			}
			entry = f.Name
		}
	}
	if entry == "" {
		return "", errors.New("Print package contains no G-code plate")
	}
	return entry, nil
}
