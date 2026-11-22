package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cameraSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Format   string `json:"format"`
	URL      string `json:"url"`
	upstream string
}

const cameraIdleGrace = 3 * time.Second

var errCameraConfigChanged = errors.New("printer connection changed")

type cameraFeed struct {
	camera         cameraSource
	ctx            context.Context
	jpegViewers    map[chan []byte]struct{}
	jpeg           []byte
	jpegAt         time.Time
	fragment       []byte
	decoder        *cameraJPEGDecoder
	decoderRetry   time.Time
	decoderBackoff time.Duration
	cancel         context.CancelFunc
	viewers        map[chan []byte]struct{}
	init           []byte
	idle           *time.Timer
	stopping       bool
	changed        chan struct{}
}
type cameraHub struct {
	mu         sync.Mutex
	feeds      map[string]*cameraFeed
	idleGrace  time.Duration
	run        func(context.Context, *PrinterConfig, cameraSource, func([]byte, bool)) error
	decodeJPEG func(context.Context, <-chan []byte, func([]byte)) error
}

func (m *SendManager) subscribeCamera(ctx context.Context, p *PrinterConfig, camera cameraSource) (<-chan []byte, func(), error) {
	return m.subscribeCameraOutput(ctx, p, camera, false)
}

func (m *SendManager) subscribeCameraOutput(ctx context.Context, p *PrinterConfig, camera cameraSource, jpeg bool) (<-chan []byte, func(), error) {
	for {
		m.mu.Lock()
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		if m.printerLocked(p.Printer) != p {
			m.mu.Unlock()
			return nil, nil, errCameraConfigChanged
		}
		frames, leave, changed := m.cameras.subscribe(p, camera, jpeg)
		m.mu.Unlock()
		if changed == nil {
			return frames, leave, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
}

func (h *cameraHub) subscribe(p *PrinterConfig, camera cameraSource, jpeg bool) (<-chan []byte, func(), <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	feed := h.feeds[p.Printer]
	if feed != nil && (feed.stopping || !feed.matches(camera)) {
		if len(feed.viewers)+len(feed.jpegViewers) == 0 {
			feed.stop()
		}
		return nil, nil, feed.changed
	}
	if feed == nil {
		ctx, cancel := context.WithCancel(context.Background())
		feed = &cameraFeed{camera: camera, ctx: ctx, jpegViewers: make(map[chan []byte]struct{}), cancel: cancel, viewers: make(map[chan []byte]struct{}), changed: make(chan struct{})}
		h.feeds[p.Printer] = feed
		go h.stream(ctx, p, feed)
	}
	if jpeg && len(feed.jpegViewers) >= 64 {
		return nil, nil, feed.changed
	}
	feed.stopIdle()
	capacity := 8
	viewers := feed.viewers
	if jpeg {
		capacity, viewers = 1, feed.jpegViewers
	}
	frames := make(chan []byte, capacity)
	viewers[frames] = struct{}{}
	if jpeg {
		if len(feed.jpeg) > 0 && time.Since(feed.jpegAt) < cameraSnapshotMaxAge {
			frames <- feed.jpeg
		}
		h.startJPEGDecoder(feed)
	} else if len(feed.init) > 0 {
		frames <- feed.init
	}
	return frames, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := viewers[frames]; ok {
			delete(viewers, frames)
			close(frames)
			close(feed.changed)
			feed.changed = make(chan struct{})
			if len(feed.jpegViewers) == 0 {
				feed.stopJPEGDecoder()
			}
			h.scheduleIdle(feed)
		}
	}, nil
}

func (h *cameraHub) stream(ctx context.Context, p *PrinterConfig, feed *cameraFeed) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		received := false
		h.run(ctx, p, feed.camera, func(data []byte, init bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if feed.stopping || len(data) > cameraChunkLimit {
				return
			}
			received = true
			if init {
				feed.init = data
			} else if feed.camera.Format == "mp4" {
				feed.fragment = data
			}
			if feed.camera.Format == "mjpeg" {
				h.publishJPEG(feed, data)
			} else if feed.decoder == nil {
				h.startJPEGDecoder(feed)
			} else if !feed.decoder.stopping {
				select {
				case feed.decoder.input <- data:
				default:
					feed.stopJPEGDecoder()
				}
			}
			for out := range feed.viewers {
				select {
				case out <- data:
				default:
					delete(feed.viewers, out)
					close(out)
				}
			}
			h.scheduleIdle(feed)
		})
		h.mu.Lock()
		feed.stopJPEGDecoder()
		feed.init, feed.fragment, feed.jpeg = nil, nil, nil
		for out := range feed.viewers {
			delete(feed.viewers, out)
			close(out)
		}
		h.scheduleIdle(feed)
		h.mu.Unlock()
		if received {
			backoff = 250 * time.Millisecond
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		backoff = min(2*backoff, 30*time.Second)
	}
	h.mu.Lock()
	feed.stop()
	var decoded <-chan struct{}
	if feed.decoder != nil {
		decoded = feed.decoder.done
	}
	h.mu.Unlock()
	if decoded != nil {
		<-decoded
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.feeds, p.Printer)
	close(feed.changed)
}

func (h *cameraHub) scheduleIdle(feed *cameraFeed) {
	if len(feed.viewers)+len(feed.jpegViewers) != 0 || feed.idle != nil || feed.stopping {
		return
	}
	close(feed.changed)
	feed.changed = make(chan struct{})
	var timer *time.Timer
	timer = time.AfterFunc(h.idleGrace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if feed.idle == timer {
			feed.stop()
		}
	})
	feed.idle = timer
}

func (f *cameraFeed) matches(camera cameraSource) bool {
	return f.camera.ID == camera.ID && f.camera.Format == camera.Format && f.camera.upstream == camera.upstream
}

func (f *cameraFeed) stopIdle() {
	if f.idle != nil {
		f.idle.Stop()
		f.idle = nil
	}
}

func (f *cameraFeed) stop() {
	if f.stopping {
		return
	}
	f.stopping = true
	f.stopIdle()
	f.cancel()
	f.stopJPEGDecoder()
	f.init, f.fragment, f.jpeg = nil, nil, nil
	for out := range f.jpegViewers {
		delete(f.jpegViewers, out)
		close(out)
	}
	for out := range f.viewers {
		delete(f.viewers, out)
		close(out)
	}
}

func (s *Server) printerCameras(w http.ResponseWriter, r *http.Request) {
	p := s.sends.printer(r.PathValue("printer"))
	if p == nil {
		fail(w, 404, "printer_doesnt_have_network")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cameras, err := cameraSources(ctx, p)
	if err != nil {
		fail(w, 502, "couldnt_read_camera_settings")
		return
	}
	writeJSON(w, 200, map[string]any{"cameras": cameras})
}
func (s *Server) printerCamera(w http.ResponseWriter, r *http.Request) {
	p := s.sends.printer(r.PathValue("printer"))
	if p == nil {
		fail(w, 404, "printer_doesnt_have_network")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	sources, err := cameraSources(ctx, p)
	cancel()
	if err != nil {
		fail(w, 502, "couldnt_read_camera_settings")
		return
	}
	var camera *cameraSource
	for i := range sources {
		if sources[i].ID == r.PathValue("camera") {
			camera = &sources[i]
		}
	}
	if camera == nil {
		fail(w, 404, "no_camera_set_up")
		return
	}
	if r.Method == "HEAD" {
		content := "video/mp4"
		if camera.Format == "mjpeg" {
			content = "multipart/x-mixed-replace; boundary=orca-camera"
		}
		w.Header().Set("Content-Type", content)
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	joinCtx, cancelJoin := context.WithTimeout(r.Context(), 10*time.Second)
	frames, leave, err := s.sends.subscribeCamera(joinCtx, p, *camera)
	cancelJoin()
	if err != nil {
		switch {
		case r.Context().Err() != nil:
		case errors.Is(err, errCameraConfigChanged):
			fail(w, 409, "printers_connection_settings_were")
		default:
			fail(w, 503, "camera_stream_active")
		}
		return
	}
	defer leave()
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	first := true
	for {
		var frame []byte
		var ok bool
		select {
		case <-r.Context().Done():
			return
		case frame, ok = <-frames:
		case <-time.After(30 * time.Second):
			if first {
				fail(w, 504, "didnt_receive_video_frame")
			}
			return
		}
		if !ok {
			if first {
				fail(w, 502, "camera_unavailable_check_camera")
			}
			return
		}
		if first {
			content := "video/mp4"
			if camera.Format == "mjpeg" {
				content = "multipart/x-mixed-replace; boundary=orca-camera"
			}
			w.Header().Set("Content-Type", content)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			first = false
		}
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		if camera.Format == "mjpeg" {
			if _, err := fmt.Fprintf(w, "--orca-camera\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame)); err != nil {
				return
			}
		}
		if _, err := w.Write(frame); err != nil {
			return
		}
		if camera.Format == "mjpeg" {
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return
			}
		}
		if err := controller.Flush(); err != nil {
			return
		}
	}
}

func cameraSources(ctx context.Context, p *PrinterConfig) ([]cameraSource, error) {
	out := []cameraSource{}
	add := func(name, format, upstream string) {
		id := strconv.Itoa(len(out))
		out = append(out, cameraSource{ID: id, Name: name, Format: format, URL: "/api/printer-connections/" + url.PathEscape(p.Printer) + "/camera/" + id, upstream: upstream})
	}
	if p.Kind == "bambu-lan" {
		format := "mjpeg"
		model := strings.ToUpper(p.model)
		if model == "" {
			model = strings.ToUpper(bambuSerialModel(p.Serial))
		}
		switch {
		case strings.Contains(model, "A1"), strings.Contains(model, "P1"):
		case strings.Contains(model, "X1"), strings.Contains(model, "H2"), strings.Contains(model, "P2"):
			format = "mp4"
		default:
			return out, nil
		}
		add("Chamber camera", format, "")
		return out, nil
	}
	if p.Kind != "moonraker" && p.Kind != "octoprint" {
		return out, nil
	}
	secret, err := p.secret()
	if err != nil {
		return nil, err
	}
	h, base, err := httpSession(ctx, p, secret)
	if err != nil {
		return nil, err
	}
	addHTTP := func(name, typed string) {
		u, err := url.Parse(typed)
		if err != nil || typed == "" {
			return
		}
		u = base.ResolveReference(u)
		if u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.Fragment != "" {
			return
		}
		if _, err = p.hosts.resolve(ctx, u); err != nil {
			return
		}
		add(name, "mjpeg", u.String())
	}
	if p.Kind == "moonraker" {
		var response struct {
			Result struct {
				Webcams []struct {
					Name     string `json:"name"`
					Enabled  bool   `json:"enabled"`
					Stream   string `json:"stream_url"`
					Snapshot string `json:"snapshot_url"`
					Service  string `json:"service"`
				} `json:"webcams"`
			} `json:"result"`
		}
		_, err = h.call(ctx, "GET", base.JoinPath("server/webcams/list").String(), nil, &response)
		if err != nil {
			return nil, err
		}
		for _, cam := range response.Result.Webcams {
			if cam.Enabled {
				stream := cam.Stream
				if strings.Contains(strings.ToLower(cam.Service), "webrtc") || strings.Contains(stream, "webrtc") {
					stream = cam.Snapshot
				}
				addHTTP(cam.Name, stream)
			}
		}
	} else {
		var response struct {
			Webcam struct {
				Stream   string `json:"streamUrl"`
				Snapshot string `json:"snapshotUrl"`
			} `json:"webcam"`
		}
		_, err = h.call(ctx, "GET", base.JoinPath("api/settings").String(), nil, &response)
		if err != nil {
			return nil, err
		}
		addHTTP("Webcam", cmpOr(response.Webcam.Stream, response.Webcam.Snapshot))
	}
	return out, nil
}
func runCamera(ctx context.Context, p *PrinterConfig, camera cameraSource, emit func([]byte, bool)) error {
	if p.Kind == "bambu-lan" {
		secret, err := p.secret()
		if err != nil {
			return err
		}
		if camera.Format == "mp4" {
			return rtspCamera(ctx, p, secret, emit)
		}
		return jpegCamera(ctx, p, secret, emit)
	}
	return httpCamera(ctx, p, camera.upstream, emit)
}

func cameraTLS(ctx context.Context, p *PrinterConfig, port int) (net.Conn, error) {
	u := &url.URL{Scheme: "tcp", Host: net.JoinHostPort(p.Host, strconv.Itoa(port))}
	target, err := p.hosts.resolve(ctx, u)
	if err != nil {
		return nil, err
	}
	config := p.tlsConfig(p.Serial)
	if port == 322 && p.TLS == "bambu-ca" {
		verify := config.VerifyConnection
		config.VerifyConnection = func(cs tls.ConnectionState) error {
			err := verify(cs)
			var unknown x509.UnknownAuthorityError
			if !errors.As(err, &unknown) {
				return err
			}
			mqtt, err := dialTLS(ctx, target.Addr().String(), 8883, p.tlsConfig(p.Serial))
			if err != nil {
				return err
			}
			defer mqtt.Close()
			cs.PeerCertificates = append(cs.PeerCertificates, mqtt.ConnectionState().PeerCertificates[1:]...)
			return verify(cs)
		}
	}
	conn, err := dialTLS(ctx, target.Addr().String(), port, config)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	return &cameraConn{Conn: conn, stop: stop}, nil
}

type cameraConn struct {
	net.Conn
	stop func() bool
}

func (c *cameraConn) Close() error { c.stop(); return c.Conn.Close() }
func jpegCamera(ctx context.Context, p *PrinterConfig, secret string, emit func([]byte, bool)) error {
	if len(secret) > 32 {
		return errors.New("Invalid camera access code.")
	}
	conn, err := cameraTLS(ctx, p, 6000)
	if err != nil {
		return err
	}
	defer conn.Close()
	auth := make([]byte, 80)
	binary.LittleEndian.PutUint32(auth, 0x40)
	binary.LittleEndian.PutUint32(auth[4:], 0x3000)
	copy(auth[16:], "bblp")
	copy(auth[48:], secret)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err = conn.Write(auth); err != nil {
		return err
	}
	for {
		conn.SetReadDeadline(time.Now().Add(25 * time.Second))
		header := make([]byte, 16)
		if _, err = io.ReadFull(conn, header); err != nil {
			return err
		}
		size := binary.LittleEndian.Uint32(header)
		if size < 4 || size > 16<<20 {
			return errors.New("invalid camera frame size")
		}
		frame := make([]byte, size)
		if _, err = io.ReadFull(conn, frame); err != nil {
			return err
		}
		if !bytes.HasPrefix(frame, []byte{255, 216}) || !bytes.HasSuffix(frame, []byte{255, 217}) {
			return errors.New("invalid JPEG camera frame")
		}
		emit(frame, false)
	}
}
func httpCamera(ctx context.Context, p *PrinterConfig, typed string, emit func([]byte, bool)) error {
	u, err := url.Parse(typed)
	if err != nil {
		return err
	}
	client, err := hostClient(ctx, p, u)
	if err != nil {
		return err
	}
	secret, err := p.secret()
	if err != nil {
		return err
	}
	base, _ := url.Parse(p.URL)
	h := &hostSession{client: client, printer: p, secret: secret}
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if u.Host == base.Host && u.Scheme == base.Scheme {
			h.authorize(req)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return errors.New("The printer's camera refused the connection.")
		}
		kind, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if kind == "multipart/x-mixed-replace" {
			reader := multipart.NewReader(resp.Body, params["boundary"])
			for {
				part, err := reader.NextPart()
				if err != nil {
					resp.Body.Close()
					return err
				}
				frame, err := io.ReadAll(io.LimitReader(part, (16<<20)+1))
				if err != nil || len(frame) > 16<<20 {
					resp.Body.Close()
					return errors.New("invalid webcam frame")
				}
				part.Close()
				if len(frame) > 3 && bytes.HasPrefix(frame, []byte{255, 216}) {
					emit(frame, false)
				}
			}
		}
		frame, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(frame) > 16<<20 || !bytes.HasPrefix(frame, []byte{255, 216}) {
			return errors.New("The webcam URL didn't return a supported JPEG or MJPEG stream.")
		}
		emit(frame, false)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

func cameraTLSListener() (net.Listener, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
}

func rtspCamera(ctx context.Context, p *PrinterConfig, secret string, emit func([]byte, bool)) error {
	executable, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("Streaming RTSPS camera video requires ffmpeg to be installed on the server.")
	}
	conn, err := cameraTLS(ctx, p, 322)
	if err != nil {
		return err
	}
	defer conn.Close()
	listener, err := cameraTLSListener()
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	go func() {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		defer local.Close()
		listener.Close()
		go func() { io.Copy(conn, local); conn.Close() }()
		io.Copy(local, conn)
	}()
	target := url.URL{Scheme: "rtsps", Host: listener.Addr().String(), User: url.UserPassword("bblp", secret), Path: "/streaming/live/1"}
	cmd := exec.CommandContext(ctx, executable, "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp", "-analyzeduration", "500000", "-probesize", "32768", "-timeout", "15000000", "-i", target.String(), "-map", "0:v:0", "-an", "-c:v", "copy", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-flush_packets", "1", "-f", "mp4", "pipe:1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var init, fragment []byte
	for {
		header := make([]byte, 8)
		if _, err = io.ReadFull(pipe, header); err != nil {
			return err
		}
		size := uint64(binary.BigEndian.Uint32(header))
		kind := string(header[4:])
		if size == 1 {
			extra := make([]byte, 8)
			if _, err = io.ReadFull(pipe, extra); err != nil {
				return err
			}
			size = binary.BigEndian.Uint64(extra)
			header = append(header, extra...)
		}
		if size < uint64(len(header)) || size > 32<<20 {
			return errors.New("invalid MP4 box")
		}
		box := make([]byte, int(size))
		copy(box, header)
		if _, err = io.ReadFull(pipe, box[len(header):]); err != nil {
			return err
		}
		switch kind {
		case "ftyp", "moov":
			if len(init)+len(box) > cameraChunkLimit {
				return errors.New("camera initialization too large")
			}
			init = append(init, box...)
			if kind == "moov" {
				emit(init, true)
			}
		case "moof":
			fragment = box
		case "mdat":
			if len(fragment) > 0 {
				if len(fragment)+len(box) > cameraChunkLimit {
					return errors.New("camera fragment too large")
				}
				fragment = append(fragment, box...)
				emit(fragment, false)
				fragment = nil
			}
		}
	}
}

func (h *cameraHub) disconnect(printer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if feed := h.feeds[printer]; feed != nil {
		feed.stop()
	}
}
