package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

type PrinterConfig struct {
	Printer        string
	Name           string
	Kind           string
	URL            string
	Host           string
	Serial         string
	Auth           string
	Username       string
	Storage        string
	CredentialFile string
	TLS            string
	Fingerprint    string

	id          string
	fingerprint []byte
	bambuRoots  *x509.CertPool
	hosts       *hostPolicy
	model       string
	trusted     func(mode string, fp []byte)
}

var connectionKinds = map[string]string{
	"bambu-lan": "Bambu Lab (LAN)", "bambu-connect": "Bambu Connect", "moonraker": "Klipper (Moonraker)",
	"octoprint": "OctoPrint", "prusalink": "PrusaLink", "duet": "Duet (RepRapFirmware)", "flashforge": "Flashforge (LAN)",
	"elegoolink": "Elegoo Link",
}

type ConnectionView struct {
	Printer             string `json:"printer"`
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Kind                string `json:"kind"`
	Host                string `json:"host,omitempty"`
	Serial              string `json:"serial,omitempty"`
	Model               string `json:"model,omitempty"`
	AutoFlowCalibration bool   `json:"auto_flow_calibration,omitempty"`
	URL                 string `json:"url,omitempty"`
	Auth                string `json:"auth,omitempty"`
	Username            string `json:"username,omitempty"`
	HasSecret           bool   `json:"has_secret"`
	Trust               string `json:"trust,omitempty"`
	Limited             bool   `json:"limited_controls,omitempty"`
}

func (p *PrinterConfig) view() ConnectionView {
	_, err := os.Stat(p.CredentialFile)
	return ConnectionView{Printer: p.Printer, ID: p.id, Name: p.Name, Kind: p.Kind, Host: p.Host, Serial: p.Serial, Model: p.model,
		AutoFlowCalibration: p.supportsAutoFlowCalibration(),
		URL:                 p.URL, Auth: p.Auth, Username: p.Username, HasSecret: p.CredentialFile != "" && err == nil, Trust: p.TLS, Limited: p.limitedControls()}
}

var bambuSerial = regexp.MustCompile(`^[A-Za-z0-9]{6,32}$`)

func (p *PrinterConfig) validate(resources string) error {
	if len(p.Name) > 200 || strings.IndexFunc(p.Name, unicode.IsControl) >= 0 {
		return errors.New("Printer name must be 200 characters or fewer (with no control characters).")
	}
	switch p.Kind {
	case "prusalink", "octoprint", "moonraker", "duet", "elegoolink":
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("Please enter a base http:// or https:// URL without login credentials, query parameters, or #fragments.")
		}
		if p.TLS == "" && u.Scheme == "https" {
			p.TLS = "system"
		}
		if u.Scheme == "http" && p.TLS != "" {
			return errors.New("TLS security settings only apply to HTTPS URLs.")
		}
		if p.TLS == "bambu-ca" || p.TLS == "tofu" {
			return errors.New("TLS mode " + p.TLS + " only applies to Bambu Lab printers.")
		}
		if p.Host != "" || p.Serial != "" {
			return errors.New("Host IP and serial number fields only apply to Bambu Lab printers.")
		}
		if p.Kind == "elegoolink" && !strings.HasPrefix(p.model, "Elegoo Centauri") {
			return errors.New("Elegoo Link works with Elegoo Centauri printers—connect other Elegoo printers with OctoPrint or Klipper (Moonraker).")
		}
	case "bambu-lan":
		if p.Host == "" || strings.ContainsAny(p.Host, "/:@ ") && net.ParseIP(p.Host) == nil {
			return errors.New("Host must be your printer's IP address or hostname.")
		}
		if !bambuSerial.MatchString(p.Serial) {
			return errors.New("Serial must be your printer's serial number.")
		}
		if p.URL != "" {
			return errors.New("Bambu Lab printers use the Host IP field rather than a URL.")
		}
		if p.TLS == "" {
			p.TLS = "bambu-ca"
		}
		if p.TLS == "system" {
			return errors.New("Bambu Lab printers use Bambu-signed certificates—set TLS mode to bambu-ca or pin.")
		}
		if p.TLS == "bambu-ca" || p.TLS == "tofu" {
			pem, err := os.ReadFile(resources + "/cert/printer.cer")
			if err != nil {
				return fmt.Errorf("tls bambu-ca needs resources/cert/printer.cer: %w", err)
			}
			p.bambuRoots = x509.NewCertPool()
			if !p.bambuRoots.AppendCertsFromPEM(pem) {
				return errors.New("resources/cert/printer.cer holds no certificates")
			}
		}
	case "flashforge":
		if p.Host == "" || strings.ContainsAny(p.Host, "/:@ ") && net.ParseIP(p.Host) == nil {
			return errors.New("Host must be your printer's IP address or hostname.")
		}
		if p.Serial != "" && !bambuSerial.MatchString(p.Serial) {
			return errors.New("Serial must be your printer's serial number.")
		}
		if p.URL != "" || p.TLS != "" {
			return errors.New("Flashforge printers use the Host IP field instead of a URL.")
		}
	case "bambu-connect":
		if p.URL != "" || p.Host != "" || p.TLS != "" {
			return errors.New("Bambu Connect doesn't need an IP address or URL.")
		}
		return nil
	default:
		return errors.New("kind must be bambu-lan, bambu-connect, moonraker, octoprint, prusalink, duet, flashforge or elegoolink")
	}
	switch p.TLS {
	case "", "system", "bambu-ca", "tofu":
		if p.Fingerprint != "" {
			return errors.New("fingerprint needs tls pin")
		}
	case "pin":
		fp, err := hex.DecodeString(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimPrefix(strings.ToLower(p.Fingerprint), "sha256:")))
		if err != nil || len(fp) != sha256.Size {
			return errors.New("tls pin needs fingerprint: the certificate's SHA-256")
		}
		p.fingerprint = fp
	default:
		return errors.New("tls must be system, pin or bambu-ca")
	}
	if p.Kind == "prusalink" {
		if p.Auth == "" {
			p.Auth = "apikey"
		}
		if p.Auth != "apikey" && p.Auth != "digest" {
			return errors.New("auth must be apikey or digest")
		}
		if p.Auth == "digest" && p.Username == "" {
			return errors.New("HTTP Digest login requires a username.")
		}
		if p.Storage == "" {
			p.Storage = "usb"
		}
	} else if p.Auth != "" || p.Username != "" || p.Storage != "" {
		return errors.New("Auth mode, username, and storage settings only apply to PrusaLink printers.")
	}
	return nil
}

func secretRequired(kind string) bool {
	return kind == "bambu-lan" || kind == "octoprint" || kind == "prusalink"
}

func readCredential(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("unreadable")
	}
	value := strings.TrimRight(string(b), "\r\n")
	if value == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", errors.New("must hold one nonempty line")
	}
	return value, nil
}

func (p *PrinterConfig) secret() (string, error) {
	if _, err := os.Stat(p.CredentialFile); errors.Is(err, os.ErrNotExist) && !secretRequired(p.Kind) {
		return "", nil
	}
	value, err := readCredential(p.CredentialFile)
	if err != nil {
		return "", errors.New("Oops! We couldn't read the saved access code or API key for this printer. Please re-enter it in Connection settings.")
	}
	return value, nil
}

func (p *PrinterConfig) resolvedHost(ctx context.Context) (string, error) {
	target, err := p.hosts.resolve(ctx, &url.URL{Scheme: "tcp", Host: net.JoinHostPort(p.Host, "8883")})
	if err != nil {
		var refused *hostRefusal
		if errors.As(err, &refused) {
			audit(map[string]any{"event": "printer_host_refused", "printer": p.Printer, "reason": refused.reason})
		}
		return "", err
	}
	return target.Addr().String(), nil
}

func (p *PrinterConfig) tlsConfig(serverName string) *tls.Config {
	c := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	switch p.TLS {
	case "pin":
		c.InsecureSkipVerify = true
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("The printer didn't present a TLS security certificate.")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if string(sum[:]) != string(p.fingerprint) {
				return errPinMismatch
			}
			return nil
		}
	case "bambu-ca":
		c.InsecureSkipVerify = true
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			leaf, err := p.bambuLeaf(cs)
			if err != nil {
				return &bambuCertificateError{cause: err}
			}
			if !strings.EqualFold(leaf.Subject.CommonName, p.Serial) {
				return &serialMismatch{leaf.Subject.CommonName}
			}
			return nil
		}
	case "tofu":
		c.InsecureSkipVerify = true
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("The printer didn't present a TLS security certificate.")
			}
			if leaf, err := p.bambuLeaf(cs); err == nil {
				if !strings.EqualFold(leaf.Subject.CommonName, p.Serial) {
					return &serialMismatch{leaf.Subject.CommonName}
				}
				p.trust("bambu-ca", nil)
				return nil
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			p.trust("pin", sum[:])
			return nil
		}
	}
	return c
}

var errPinMismatch = errors.New("The printer's security certificate doesn't match the expected fingerprint.")

type bambuCertificateError struct{ cause error }

func (e *bambuCertificateError) Error() string {
	return "The printer's security certificate wasn't signed by the Bambu printer CA."
}

func (e *bambuCertificateError) Unwrap() error { return e.cause }

type serialMismatch struct{ actual string }

func (e *serialMismatch) Error() string { return "the printer at this address has serial " + e.actual }

func (p *PrinterConfig) bambuLeaf(cs tls.ConnectionState) (*x509.Certificate, error) {
	if len(cs.PeerCertificates) == 0 || p.bambuRoots == nil {
		return nil, errors.New("The printer didn't present a TLS security certificate.")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range cs.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	leaf := cs.PeerCertificates[0]
	_, err := leaf.Verify(x509.VerifyOptions{Roots: p.bambuRoots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return leaf, err
}

func (p *PrinterConfig) trust(mode string, fp []byte) {
	if p.trusted != nil {
		p.trusted(mode, fp)
	}
}

type Transfer struct {
	ID        string           `json:"id"`
	Job       string           `json:"job"`
	Printer   string           `json:"printer"`
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Start     bool             `json:"start"`
	Filename  string           `json:"filename"`
	State     TransferState    `json:"state"`
	Sent      int64            `json:"sent"`
	Total     int64            `json:"total"`
	Message   string           `json:"message,omitempty"`
	Error     string           `json:"error,omitempty"`
	Outcome   string           `json:"outcome,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	Recovery  string           `json:"recovery,omitempty"`
	JobEnd    string           `json:"job_end,omitempty"`
	JobEndAt  *time.Time       `json:"job_end_at,omitempty"`
	Running   bool             `json:"running,omitempty"`
	JobErrors []PrinterMessage `json:"job_errors,omitempty"`
	Project   string           `json:"project,omitempty"`
	Version   uint64           `json:"version,omitempty"`
	Started   time.Time        `json:"started"`
	Finished  *time.Time       `json:"finished,omitempty"`

	sent   *atomic.Int64
	cancel context.CancelCauseFunc
}

type TransferState string

const (
	TransferUploading TransferState = "uploading"
	TransferStarting  TransferState = "starting"
	TransferDone      TransferState = "done"
	TransferFailed    TransferState = "failed"
	TransferCancelled TransferState = "cancelled"
)

var transferTransitions = map[TransferState]map[TransferState]bool{
	TransferUploading: {TransferStarting: true, TransferDone: true, TransferFailed: true, TransferCancelled: true},
	TransferStarting:  {TransferDone: true, TransferFailed: true, TransferCancelled: true},
	TransferDone:      {},
	TransferFailed:    {},
	TransferCancelled: {},
}

func (t *Transfer) to(next TransferState) bool {
	if !transferTransitions[t.State][next] {
		log.Printf("send %s: illegal transition %s -> %s", t.ID, t.State, next)
		return false
	}
	t.State = next
	return true
}

func (t *Transfer) active() bool { return t.State == TransferUploading || t.State == TransferStarting }

type SendManager struct {
	store   *printerStore
	mu      sync.Mutex
	conns   []*PrinterConfig
	busy    map[string]bool
	live    map[string]*Job
	cards   map[string]string
	buckets map[string]tokenBucket
	cameras *cameraHub
	hub     *statusHub
	history *sendHistory
}

const sendBurst = 3
const sendRate = 0.1

const deviceBurst, deviceRate = 20, 2

func NewSendManager(store *printerStore) *SendManager {
	m := &SendManager{store: store, busy: map[string]bool{}, live: map[string]*Job{}, cards: map[string]string{}, buckets: map[string]tokenBucket{}}
	m.hub = newStatusHub(m)
	m.cameras = &cameraHub{feeds: make(map[string]*cameraFeed), idleGrace: cameraIdleGrace, run: runCamera}
	if store != nil {
		m.conns = store.printers
		for _, p := range m.conns {
			m.watchTrust(p)
		}
		m.history = loadSendHistory(store.dir)
		m.cards = m.history.cards()
	}
	return m
}

func (m *SendManager) printer(key string) *PrinterConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.printerLocked(key)
}

func (m *SendManager) printerLocked(key string) *PrinterConfig {
	for _, p := range m.conns {
		if p.Printer == key && key != "" {
			return p
		}
	}
	return nil
}

func (m *SendManager) holds(p *PrinterConfig) bool { return slices.Contains(m.conns, p) }

func (m *SendManager) connections() []*PrinterConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

func (m *SendManager) allow(client string) (bool, int) {
	return m.allowBurst(client, sendBurst, sendRate)
}

func (m *SendManager) allowBurst(client string, burst, rate float64) (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	b, ok := m.buckets[client]
	if !ok {
		b = tokenBucket{burst, now}
	}
	b.tokens = math.Min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	b.at = now
	if b.tokens < 1 {
		m.buckets[client] = b
		return false, int(math.Ceil((1 - b.tokens) / rate))
	}
	b.tokens--
	m.buckets[client] = b
	if len(m.buckets) > 4096 {
		for key, old := range m.buckets {
			if now.Sub(old.at) > time.Minute {
				delete(m.buckets, key)
			}
		}
	}
	return true, 0
}

func (j *Job) transferView() *Transfer {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.send == nil {
		return nil
	}
	view := *j.send
	view.Sent = j.send.sent.Load()
	return &view
}

func (s *Server) getSend(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	if t := j.transferView(); t != nil {
		if t.Outcome == "unconfirmed" || t.Outcome == "printing" {
			if recorded, ok := s.sends.history.lookup(t.ID); ok && !recorded.active() {
				t = &recorded
			}
		}
		writeJSON(w, 200, t)
		return
	}
	fail(w, 404, "job_hasnt_been_sent")
}

func (s *Server) cancelSend(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	j.mu.Lock()
	t := j.send
	if t == nil || !t.active() {
		j.mu.Unlock()
		fail(w, 409, "job_isnt_currently_uploading")
		return
	}
	cancel := t.cancel
	j.mu.Unlock()
	cancel(errSendCancelled)
	writeJSON(w, 202, j.transferView())
}

var errSendCancelled = errors.New("cancelled")

func sendRefusal(row sendSlot, printer string) failure {
	switch {
	case row.State == "unassigned":
		return newFailure("slot_unassigned", row.Filament, printer)
	case row.State == "ambiguous":
		return newFailure("topology_unknown", printer)
	case row.tray == nil:
		return newFailure("slot_tray_missing", row.Filament, row.Tray, printer)
	case strings.TrimSpace(row.tray.Type) == "":
		return newFailure("slot_tray_empty", row.Filament, row.Tray, printer, row.Expected.Type)
	}
	return newFailure("slot_mismatch", row.Filament, row.Tray, row.tray.Type, printer, row.Expected.Type)
}

func (s *Server) getSendPlan(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	p := s.sends.printer(r.URL.Query().Get("printer"))
	if p == nil || p.Kind != "bambu-lan" {
		fail(w, 404, "printer_isnt_connected_over")
		return
	}
	if j.AwaitFinished(r.Context()) != StateDone || j.Request.Output != "gcode.3mf" {
		fail(w, 409, "wait_slicing_finish_before")
		return
	}
	sliced, err := readBambuJobMapping(j.OutputPath("gcode.3mf"))
	if err != nil {
		fail(w, 422, "couldnt_read_nozzle_assignment", err.Error())
		return
	}
	lib, err := s.libraryDocument()
	if err != nil {
		libraryFailure(w, 503, "library_unavailable", "Profiles could not be read.", "retry_storage", 0)
		return
	}
	status, ok := s.sends.hub.current(p.Printer)
	if !ok || !status.Online {
		status = PrinterStatus{}
	}
	plan, err := s.printerSendPlan(lib, sendPlanInput{Source: j.Request.Library, TargetID: p.Printer, Sliced: sliced, TargetLive: status})
	if err != nil {
		fail(w, 409, "slot_identity", err.Error())
		return
	}
	writeJSON(w, 200, plan)
}

type sendRequest struct {
	Printer       string          `json:"printer"`
	Start         *bool           `json:"start"`
	Filename      string          `json:"filename"`
	BedLeveling   *bool           `json:"bed_leveling"`
	FlowCali      bool            `json:"flow_cali"`
	FlowCaliMode  json.RawMessage `json:"flow_cali_mode"`
	Timelapse     bool            `json:"timelapse"`
	VibrationCali *bool           `json:"vibration_cali"`
	Override      []string        `json:"override"`
	ExpectedPlan  []string        `json:"expected_plan"`
}

type validatedSend struct {
	job        *Job
	printer    *PrinterConfig
	filename   string
	start      bool
	options    bambuOptions
	plan       *sendPlan
	status     *PrinterStatus
	checkStart func() error
	cleanup    func()
}

func (s *Server) sendJob(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	if s.sends.store == nil {
		fail(w, 403, "printer_network_connections_disabled")
		return
	}
	var body sendRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		fail(w, 413, "send_request_exceeds_kib")
		return
	}
	if json.Unmarshal(raw, &body) != nil {
		fail(w, 400, "expected_printer_printer_setup")
		return
	}
	if send := s.validateSend(w, r, j, body, "", true); send != nil {
		s.launchSend(w, r, send, nil)
	}
}

func (s *Server) validateSend(w http.ResponseWriter, r *http.Request, j *Job, body sendRequest, artifactPath string, wait bool) *validatedSend {
	if s.sends.store == nil {
		fail(w, 403, "printer_network_connections_disabled")
		return nil
	}
	if body.Printer == "" {
		fail(w, 400, "expected_printer_printer_setup")
		return nil
	}
	flowCali, err := parseFlowCalibrationMode(body.FlowCaliMode, body.FlowCali)
	if err != nil {
		fail(w, 400, "http_invalid", err.Error())
		return nil
	}
	p := s.sends.printer(body.Printer)
	if p == nil {
		fail(w, 404, "printer_isnt_connected_add")
		return nil
	}
	if p.Kind == "bambu-connect" {
		fail(w, 409, "bambu_connect_opens_files")
		return nil
	}
	start := body.Start == nil || *body.Start
	filename := j.DownloadName()
	if body.Filename != "" {
		if strings.ContainsAny(body.Filename, "/\\") || len(body.Filename) > 240 || strings.IndexFunc(body.Filename, unicode.IsControl) >= 0 || !strings.HasSuffix(strings.ToLower(body.Filename), "."+j.Request.Output) || strings.HasPrefix(body.Filename, ".") {
			fail(w, 400, "filename_must_basename_ending", j.Request.Output)
			return nil
		}
		filename = body.Filename
	}
	state := j.State()
	if wait && state != StateDone {
		state = j.AwaitFinished(r.Context())
	}
	if state != StateDone || j.Request.Orient || j.Request.Output == "3mf" {
		fail(w, 409, "wait_slicing_finish_before")
		return nil
	}
	if refusal := j.curveRefusal(); refusal != "" {
		writeJSON(w, 409, map[string]string{"error": refusal, "code": "could_not_curve"})
		return nil
	}
	if want := map[bool]string{true: "gcode.3mf", false: "gcode"}[p.Kind == "bambu-lan"]; j.Request.Output != want {
		fail(w, 400, "needs_export_job_produced", connectionKinds[p.Kind], want, j.Request.Output)
		return nil
	}
	send := &validatedSend{
		job: j, printer: p, filename: filename, start: start,
		options: bambuOptions{
			bedLeveling:   body.BedLeveling == nil && p.Kind != "elegoolink" || body.BedLeveling != nil && *body.BedLeveling,
			flowCali:      flowCali,
			timelapse:     body.Timelapse,
			vibrationCali: body.VibrationCali == nil || *body.VibrationCali,
		},
	}
	if p.Kind == "bambu-lan" && start {
		if artifactPath == "" {
			artifactPath = j.OutputPath("gcode.3mf")
		}
		sliced, err := readBambuJobMapping(artifactPath)
		if err != nil {
			fail(w, 422, "couldnt_read_nozzle_assignment", err.Error())
			return nil
		}
		status, ok := s.sends.hub.current(p.Printer)
		if !ok || !status.Online || status.Trays == nil {
			fail(w, 409, "topology_unknown", p.Name)
			return nil
		}
		lib, err := s.libraryDocument()
		if err != nil {
			libraryFailure(w, 503, "library_unavailable", "Profiles could not be read.", "retry_storage", 0)
			return nil
		}
		plan, err := s.printerSendPlan(lib, sendPlanInput{Source: j.Request.Library, TargetID: p.Printer, Sliced: sliced, TargetLive: status})
		if err != nil {
			fail(w, 409, "slot_identity", err.Error())
			return nil
		}
		if !plan.matchesProofs(body.ExpectedPlan) {
			refusal := newFailure("topology_mismatch", "The printer filament setup changed. Check the tray mapping and send again.")
			writeJSON(w, 409, map[string]any{"error": refusal.Error, "code": refusal.Code, "step": refusal.Step, "plan": plan})
			return nil
		}
		v0, v1, refused := plan.mapping(body.Override)
		if refused != nil {
			refusal := sendRefusal(*refused, p.Name)
			writeJSON(w, 409, map[string]any{"error": refusal.Error, "code": refusal.Code, "step": refusal.Step, "plan": plan})
			return nil
		}
		submitted := j.submittedFilamentMap()
		if err := checkSendTopology(sliced, submitted, status, plan, p.Name); err != nil {
			fail(w, 409, "topology_mismatch", err.Error())
			return nil
		}
		send.plan = &plan
		send.status = &status
		send.options.amsMapping, send.options.amsMapping2 = v0, v1
		send.options.useAMS = slices.ContainsFunc(v0, func(tray int) bool { return tray >= 0 })
		send.checkStart = func() error {
			if s.sends.printer(p.Printer) != p {
				return &connError{"refused", "The printer connection changed during the upload. Check the connection and send again."}
			}
			status, ok := s.sends.hub.current(p.Printer)
			if !ok || !status.Online || status.Trays == nil {
				return &connError{"refused", "The printer stopped reporting its trays during the upload. Check its status and send again."}
			}
			if status.State == "printing" || status.State == "paused" || status.State == "busy" {
				return &connError{"busy", "the printer is " + status.State + "; wait for its current job to finish"}
			}
			if err := plan.checkLiveTrays(status.Trays); err != nil {
				return &connError{"refused", err.Error()}
			}
			if slices.ContainsFunc(plan.Slots, func(row sendSlot) bool { return row.State == "remembered" }) {
				currentLibrary, err := s.libraryDocument()
				if err != nil {
					return &connError{"refused", "The saved external filament could not be checked during the upload. Check the slot and send again."}
				}
				if err := plan.checkRemembered(s.catalog, currentLibrary); err != nil {
					return &connError{"refused", err.Error()}
				}
			}
			if err := checkSendTopology(sliced, submitted, status, plan, p.Name); err != nil {
				return &connError{"refused", err.Error()}
			}
			return nil
		}
	}
	return send
}

func (s *Server) launchSend(w http.ResponseWriter, r *http.Request, send *validatedSend, artifact *os.File) {
	j, p, filename, start := send.job, send.printer, send.filename, send.start
	f := artifact
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	if r.Context().Err() != nil {
		fail(w, 408, "http_timeout", r.Context().Err().Error())
		return
	}
	client := s.clientIP(r)
	if ok, retry := s.sends.allow(client); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(min(retry, 86400)))
		fail(w, 429, "too_many_print_uploads")
		return
	}
	if f == nil {
		var err error
		f, err = os.Open(j.OutputPath(j.Request.Output))
		if err != nil {
			fail(w, 404, "download_link_has_expired")
			return
		}
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		fail(w, 500, "couldnt_read_exported_print")
		return
	}
	st, err := f.Stat()
	if err != nil {
		fail(w, 500, "couldnt_read_exported_print")
		return
	}
	m := s.sends
	m.mu.Lock()
	if m.printerLocked(p.Printer) != p {
		m.mu.Unlock()
		fail(w, 409, "printers_connection_settings_were")
		return
	}
	if m.busy[p.Printer] {
		m.mu.Unlock()
		fail(w, 409, "busy_receiving_another_file", p.Name)
		return
	}
	j.mu.Lock()
	if j.send != nil && j.send.active() {
		j.mu.Unlock()
		m.mu.Unlock()
		fail(w, 409, "print_job_already_being")
		return
	}
	card := p.Printer + "\x00" + filename
	resend := artifact == nil && m.cards[card] == j.ID
	delete(m.cards, card)
	base, stopTimeout := context.WithTimeoutCause(context.Background(), sendTimeout, errors.New("The upload took longer than 30 minutes and timed out."))
	ctx, cancel := context.WithCancelCause(base)
	t := &Transfer{ID: newSendID(), Job: j.ID, Printer: p.Printer, Name: p.Name, Kind: p.Kind, Start: start, Filename: filename, State: TransferUploading,
		Total: st.Size(), Started: time.Now().UTC(), sent: &atomic.Int64{}, cancel: cancel}
	t.Project, t.Version = s.workspace.versionOfJob(j.ID)
	j.send = t
	view := *t
	j.mu.Unlock()
	m.busy[p.Printer] = true
	m.live[p.Printer] = j
	m.mu.Unlock()
	m.history.record(view)
	m.hub.changed()
	audit(map[string]any{"event": "send_accepted", "job_id": j.ID, "printer": p.Printer, "kind": p.Kind, "client": client, "start_print": start, "bytes": st.Size()})
	go s.runSend(ctx, stopTimeout, j, t, p, f, client, send.options, resend, send.checkStart, send.cleanup)
	f = nil
	w.Header().Set("Location", "/api/jobs/"+j.ID+"/send")
	writeJSON(w, 202, &view)
}

func audit(fields map[string]any) {
	fields["audit"] = "printer_send"
	b, _ := json.Marshal(fields)
	log.Writer().Write(append(b, '\n'))
}

const sendTimeout = 30 * time.Minute
const sendStall = 30 * time.Second

type upload struct {
	printer         *PrinterConfig
	secret          string
	filename        string
	file            *os.File
	size            int64
	start           bool
	progress        *atomic.Int64
	starting        func()
	stored          func()
	options         bambuOptions
	resend, onCard  bool
	outcome         string
	internalStorage *bool
	checkStart      func() error
}

func (u *upload) placed() string {
	if u.onCard {
		return u.filename + " was already on " + u.printer.Name + "'s SD card"
	}
	return "Uploaded " + u.filename + " to " + u.printer.Name
}

func (u *upload) result(message string) (outcome, detail string) {
	switch {
	case !u.start:
		return "uploaded", message
	case u.outcome != "":
		return u.outcome, message
	default:
		return "unconfirmed", message + " Waiting for the printer to confirm the start."
	}
}

type bambuOptions struct {
	bedLeveling, timelapse, vibrationCali, useAMS bool
	flowCali                                      flowCalibrationMode
	amsMapping                                    []int
	amsMapping2                                   []bambuTrayAddress
}

func (s *Server) runSend(ctx context.Context, stopTimeout context.CancelFunc, j *Job, t *Transfer, p *PrinterConfig, f *os.File, client string, options bambuOptions, resend bool, checkStart func() error, cleanup func()) {
	defer func() {
		f.Close()
		if cleanup != nil {
			cleanup()
		}
	}()
	m := s.sends
	if checkStart != nil {
		defer m.hub.subscribe()()
	}
	began := time.Now()
	defer stopTimeout()
	defer t.cancel(nil)
	stalled := watchProgress(ctx, t.cancel, t.sent, t.Total)
	u := &upload{printer: p, filename: t.Filename, file: f, size: t.Total, start: t.Start, progress: t.sent,
		starting: func() { j.mu.Lock(); t.to(TransferStarting); j.mu.Unlock(); m.hub.changed() },
		stored: func() {
			m.mu.Lock()
			m.cards[p.Printer+"\x00"+t.Filename] = j.ID
			m.mu.Unlock()
		}, options: options, resend: resend, checkStart: checkStart}
	var message string
	var before PrinterStatus
	secret, err := p.secret()
	u.secret = secret
	if err == nil {
		if status, ok := m.hub.current(p.Printer); ok && status.Online {
			before = status
			u.internalStorage = status.InternalStorage
			if t.Start && (status.State == "printing" || status.State == "paused" || status.State == "busy") {
				err = &connError{"busy", "the printer is " + status.State + "; wait for its current job to finish"}
			}
		}
	}
	if err == nil {
		switch p.Kind {
		case "bambu-lan":
			message, err = sendBambu(ctx, u)
		case "duet":
			message, err = sendDuet(ctx, u)
		case "flashforge":
			message, err = sendFlashforge(ctx, u)
		case "elegoolink":
			message, err = sendElegoo(ctx, u)
		default:
			message, err = sendHTTP(ctx, u)
		}
	}
	cancelled := false
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
		if stalled.Load() {
			err = &connError{"timeout", "The printer stopped responding during the upload (timed out after 30 seconds)."}
		}
		cancelled = errors.Is(context.Cause(ctx), errSendCancelled)
	}
	view := m.finishTransfer(j, t, u, before, message, err, cancelled)
	m.mu.Lock()
	delete(m.busy, p.Printer)
	delete(m.live, p.Printer)
	m.mu.Unlock()
	m.hub.changed()
	audit(map[string]any{"event": "send_" + string(view.State), "job_id": j.ID, "printer": p.Printer, "kind": p.Kind, "client": client, "start_print": view.Start, "bytes": view.Sent, "on_card": u.onCard, "duration_ms": time.Since(began).Milliseconds(), "error": view.Error})
}

func (m *SendManager) finishTransfer(j *Job, t *Transfer, u *upload, before PrinterStatus, message string, err error, cancelled bool) Transfer {
	finished := time.Now().UTC()
	j.mu.Lock()
	defer j.mu.Unlock()
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	t.Finished = &finished
	switch {
	case cancelled:
		t.Outcome = "failed"
		t.Error = map[bool]string{true: "Cancelled after the file reached the printer; check the printer before printing it", false: "Cancelled; the file did not reach the printer"}[t.State == TransferStarting]
		t.to(TransferCancelled)
	case err != nil:
		t.Error = redact(err.Error(), u.secret)
		t.Outcome = "failed"
		t.Reason, t.Recovery = sendFailure(err)
		t.to(TransferFailed)
	default:
		t.Outcome, message = u.result(message)
		t.Message = redact(message, u.secret)
		t.to(TransferDone)
		if observed, ok := m.history.lookup(t.ID); ok && observed.active() {
			t.Running, t.JobEnd, t.JobEndAt, t.JobErrors = observed.Running, observed.JobEnd, observed.JobEndAt, observed.JobErrors
			if observed.Outcome == "unconfirmed" && !t.Running && t.JobEnd == "" {
				t.awaitStartConfirmation()
			}
		}
		t.applyStatus(before, m.hub.status[t.Printer])
		t.confirmObservedStart()
	}
	view := *t
	view.Sent = t.sent.Load()
	m.history.record(view)
	return view
}

func sendFailure(err error) (string, string) {
	reason := "connection"
	var conn *connError
	var ftp *ftpError
	switch {
	case errors.As(err, &conn):
		reason = map[string]string{"access_code": "credentials", "developer_mode": "developer_mode", "refused": "refused", "busy": "busy",
			"storage": "storage", "timeout": "timeout"}[conn.stage]
	case errors.As(err, &ftp) && (ftp.code == 452 || ftp.code == 552):
		reason = "storage"
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		reason = "timeout"
	}
	if reason == "" {
		reason = "connection"
	}
	return reason, failureTexts["send_"+reason].Step
}

func (m *SendManager) activeTransfers() []Transfer {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.live))
	for _, j := range m.live {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	out := []Transfer{}
	for _, j := range jobs {
		if t := j.transferView(); t != nil && t.active() {
			out = append(out, *t)
		}
	}
	return out
}

func watchProgress(ctx context.Context, cancel context.CancelCauseFunc, sent *atomic.Int64, total int64) *atomic.Bool {
	stalled := &atomic.Bool{}
	go func() {
		last, at := sent.Load(), time.Now()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if n := sent.Load(); n != last {
					last, at = n, now
				} else if now.Sub(at) > sendStall && last < total {
					stalled.Store(true)
					cancel(errors.New("stalled"))
					return
				}
			}
		}
	}()
	return stalled
}

func redact(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[redacted]")
}

type progressReader struct {
	r io.Reader
	n *atomic.Int64
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n.Add(int64(n))
	return n, err
}

func netError(err error, what string) error {
	var dns *net.DNSError
	var opErr *net.OpError
	var verify *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return &connError{"timeout", fmt.Sprintf("%s didn't respond in time—make sure the printer is powered on and reachable on your network.", what)}
	case errors.As(err, &dns):
		return fmt.Errorf("Couldn't find the network address for %s.", what)
	case errors.As(err, &verify) || errors.As(err, &unknownAuthority) || errors.As(err, &invalid) || errors.As(err, &hostname):
		return fmt.Errorf("%s's security certificate was rejected: %v", what, lastClause(err))
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return fmt.Errorf("Couldn't connect to %s—make sure the printer is powered on and on the same network.", what)
	}
	return fmt.Errorf("Connection to %s failed: %v", what, lastClause(err))
}

func lastClause(err error) string {
	text := err.Error()
	if i := strings.LastIndex(text, ": "); i >= 0 {
		return text[i+2:]
	}
	return text
}
