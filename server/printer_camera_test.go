package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type cameraTestStream struct {
	camera   cameraSource
	emit     func([]byte, bool)
	stopping <-chan struct{}
	closed   chan struct{}
}

type cameraTestUpstream struct {
	mu     sync.Mutex
	active map[string]int
	peak   map[string]int
	starts map[string]int
	opened chan cameraTestStream
	drain  <-chan struct{}
}

func testCameraManager(t *testing.T, printers ...*PrinterConfig) (*SendManager, *cameraTestUpstream) {
	t.Helper()
	m := NewSendManager(nil)
	m.conns = printers
	u := &cameraTestUpstream{
		active: make(map[string]int),
		peak:   make(map[string]int),
		starts: make(map[string]int),
		opened: make(chan cameraTestStream, 16),
	}
	m.cameras.run = u.run
	t.Cleanup(func() {
		for _, p := range printers {
			m.cameras.disconnect(p.Printer)
		}
	})
	return m, u
}

func (u *cameraTestUpstream) run(ctx context.Context, p *PrinterConfig, c cameraSource, emit func([]byte, bool)) error {
	u.mu.Lock()
	u.active[p.Printer]++
	u.peak[p.Printer] = max(u.peak[p.Printer], u.active[p.Printer])
	u.starts[p.Printer]++
	u.mu.Unlock()
	closed := make(chan struct{})
	u.opened <- cameraTestStream{camera: c, emit: emit, stopping: ctx.Done(), closed: closed}
	<-ctx.Done()
	if u.drain != nil {
		<-u.drain
	}
	u.mu.Lock()
	u.active[p.Printer]--
	u.mu.Unlock()
	close(closed)
	return ctx.Err()
}

func (u *cameraTestUpstream) counts(printer string) (active, peak, starts int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.active[printer], u.peak[printer], u.starts[printer]
}

func cameraTestReceive[T any](t *testing.T, ctx context.Context, stream <-chan T) T {
	t.Helper()
	select {
	case value := <-stream:
		return value
	case <-ctx.Done():
		t.Fatalf("waiting for camera: %v", ctx.Err())
		var zero T
		return zero
	}
}

func TestCameraSerializesSourceChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "switching"}
	m, upstream := testCameraManager(t, p)
	m.cameras.idleGrace = time.Hour
	drain := make(chan struct{})
	upstream.drain = drain
	release := sync.OnceFunc(func() { close(drain) })
	defer release()
	first := cameraSource{ID: "0", Format: "mjpeg", upstream: "https://first.invalid/"}
	second := cameraSource{ID: "1", Format: "mjpeg", upstream: "https://second.invalid/"}
	_, leave, err := m.subscribeCamera(ctx, p, first)
	if err != nil {
		t.Fatal(err)
	}
	old := cameraTestReceive(t, ctx, upstream.opened)
	assertBlocked := func(camera cameraSource) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		_, leave, err := m.subscribeCamera(wait, p, camera)
		if leave != nil {
			leave()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting for source %s: got %v", camera.ID, err)
		}
	}
	assertBlocked(second)
	type result struct {
		leave func()
		err   error
	}
	joined := make(chan result, 1)
	go func() {
		_, leave, err := m.subscribeCamera(ctx, p, second)
		joined <- result{leave, err}
	}()
	leave()
	cameraTestReceive(t, ctx, old.stopping)
	assertBlocked(first)
	assertBlocked(second)
	release()
	cameraTestReceive(t, ctx, old.closed)
	current := cameraTestReceive(t, ctx, upstream.opened)
	if current.camera != second {
		t.Fatalf("source switch opened %+v", current.camera)
	}
	viewer := cameraTestReceive(t, ctx, joined)
	if viewer.err != nil {
		t.Fatal(viewer.err)
	}
	viewer.leave()
	m.cameras.disconnect(p.Printer)
	cameraTestReceive(t, ctx, current.closed)
	active, peak, starts := upstream.counts(p.Printer)
	t.Logf("printer %s: maximum concurrent upstream connections %d during source switch", p.Printer, peak)
	if active != 0 || peak != 1 || starts != 2 {
		t.Fatalf("source switch counts: active %d, peak %d, starts %d", active, peak, starts)
	}
}

func TestCameraDropsSlowViewerAndStopsUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "slow"}
	m, upstream := testCameraManager(t, p)
	m.cameras.idleGrace = 25 * time.Millisecond
	frames, leave, err := m.subscribeCamera(ctx, p, cameraSource{ID: "0"})
	if err != nil {
		t.Fatal(err)
	}
	stream := cameraTestReceive(t, ctx, upstream.opened)
	for i := 0; i < 32; i++ {
		stream.emit([]byte("frame"), false)
	}
	cameraTestReceive(t, ctx, stream.closed)
	buffered := 0
	for frame := range frames {
		if string(frame) != "frame" {
			t.Fatalf("unexpected buffered frame %q", frame)
		}
		buffered++
	}
	leave()
	if buffered == 0 || buffered >= 32 {
		t.Fatalf("slow viewer buffered %d frames", buffered)
	}
}

func TestCameraRejectsStaleConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	old := &PrinterConfig{Printer: "edited", Host: "old.invalid"}
	current := &PrinterConfig{Printer: old.Printer, Host: "new.invalid"}
	m, upstream := testCameraManager(t, old)
	camera := cameraSource{ID: "0"}
	_, leaveOld, err := m.subscribeCamera(ctx, old, camera)
	if err != nil {
		t.Fatal(err)
	}
	previous := cameraTestReceive(t, ctx, upstream.opened)
	m.mu.Lock()
	m.conns = []*PrinterConfig{current}
	m.cameras.disconnect(old.Printer)
	m.mu.Unlock()

	if _, _, err := m.subscribeCamera(ctx, old, camera); !errors.Is(err, errCameraConfigChanged) {
		t.Fatalf("stale config returned %v", err)
	}
	_, leaveNew, err := m.subscribeCamera(ctx, current, camera)
	if err != nil {
		t.Fatal(err)
	}
	cameraTestReceive(t, ctx, previous.closed)
	cameraTestReceive(t, ctx, upstream.opened)
	leaveOld()
	leaveNew()
	_, peak, starts := upstream.counts(current.Printer)
	if peak != 1 || starts != 2 {
		t.Fatalf("config replacement counts: peak %d, starts %d", peak, starts)
	}
}

func TestPrinterCameraHTTP(t *testing.T) {
	for _, tt := range []struct {
		model, format, mime string
		frame, wire         []byte
	}{
		{"P1S", "mjpeg", "multipart/x-mixed-replace; boundary=orca-camera", []byte{255, 216, 255, 217}, []byte("--orca-camera\r\nContent-Type: image/jpeg\r\nContent-Length: 4\r\n\r\n\xff\xd8\xff\xd9\r\n")},
		{"H2D", "mp4", "video/mp4", []byte("test init"), []byte("test init")},
	} {
		t.Run(tt.model, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			p := &PrinterConfig{Printer: "fixture", Kind: "bambu-lan", model: tt.model}
			m, upstream := testCameraManager(t, p)
			server := &Server{sends: m}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/printer-connections/{printer}/camera/{camera}", server.printerCamera)
			listener := httptest.NewServer(mux)
			defer listener.Close()
			url := listener.URL + "/api/printer-connections/fixture/camera/0"
			head, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := listener.Client().Do(head)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if r.StatusCode != 200 || r.Header.Get("Content-Type") != tt.mime {
				t.Fatalf("head response %d, %v", r.StatusCode, r.Header)
			}
			if _, _, starts := upstream.counts(p.Printer); starts != 0 {
				t.Fatal("HEAD opened upstream")
			}
			result := make(chan error, 1)
			go func() {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if err != nil {
					result <- err
					return
				}
				resp, err := listener.Client().Do(req)
				if err != nil {
					result <- err
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != tt.mime {
					result <- fmt.Errorf("get response %d, %v", resp.StatusCode, resp.Header)
					return
				}
				got := make([]byte, len(tt.wire))
				if _, err := io.ReadFull(resp.Body, got); err != nil {
					result <- err
					return
				}
				if !bytes.Equal(got, tt.wire) {
					result <- fmt.Errorf("wire frame %q, want %q", got, tt.wire)
					return
				}
				result <- nil
			}()
			stream := cameraTestReceive(t, ctx, upstream.opened)
			if stream.camera.Format != tt.format {
				t.Fatalf("opened camera format %q, want %q", stream.camera.Format, tt.format)
			}
			stream.emit(tt.frame, tt.format == "mp4")
			if err := cameraTestReceive(t, ctx, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCameraIntermediateRecovery(t *testing.T) {
	const serial = "H2D-example"
	root := issueCameraTestCert(t, "Bambu root", true, nil)
	intermediate := issueCameraTestCert(t, "Bambu intermediate", true, &root)
	mqtt := issueCameraTestCert(t, serial, false, &intermediate)
	camera := issueCameraTestCert(t, serial, false, &intermediate)
	wrongSerial := issueCameraTestCert(t, "different-serial", false, &intermediate)
	untrusted := issueCameraTestCert(t, serial, false, nil)
	roots := x509.NewCertPool()
	roots.AddCert(root.Leaf)
	p := &PrinterConfig{TLS: "bambu-ca", Serial: serial, bambuRoots: roots}

	for _, tc := range []struct {
		name        string
		cameraChain []*x509.Certificate
		mqtt        tls.Certificate
		wantMQTT    bool
		wantErr     string
	}{
		{name: "complete camera chain", cameraChain: []*x509.Certificate{camera.Leaf, intermediate.Leaf}},
		{name: "recover camera intermediate", cameraChain: []*x509.Certificate{camera.Leaf}, mqtt: mqtt, wantMQTT: true},
		{name: "check recovered camera serial", cameraChain: []*x509.Certificate{wrongSerial.Leaf}, mqtt: mqtt, wantMQTT: true, wantErr: "different-serial"},
		{name: "require trusted camera leaf", cameraChain: []*x509.Certificate{untrusted.Leaf}, mqtt: mqtt, wantMQTT: true, wantErr: "Bambu printer CA"},
		{name: "check MQTT serial", cameraChain: []*x509.Certificate{camera.Leaf}, mqtt: wrongSerial, wantMQTT: true, wantErr: "different-serial"},
		{name: "require trusted MQTT chain", cameraChain: []*x509.Certificate{camera.Leaf}, mqtt: untrusted, wantMQTT: true, wantErr: "Bambu printer CA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := p.tlsConfig(p.Serial)
			mqttCalled := false
			recoverCameraIntermediates(config, func(mqttConfig *tls.Config) (*tls.Conn, error) {
				mqttCalled = true
				if !tc.wantMQTT {
					t.Fatal("MQTT dial with a complete camera chain")
				}
				broker := httptest.NewUnstartedServer(http.NotFoundHandler())
				broker.TLS = &tls.Config{Certificates: []tls.Certificate{tc.mqtt}}
				broker.StartTLS()
				t.Cleanup(broker.Close)
				return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", broker.Listener.Addr().String(), mqttConfig)
			})
			err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: tc.cameraChain})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("verification error = %v, want %q", err, tc.wantErr)
			}
			if mqttCalled != tc.wantMQTT {
				t.Fatalf("MQTT dialed = %t, want %t", mqttCalled, tc.wantMQTT)
			}
		})
	}
}

func issueCameraTestCert(t *testing.T, name string, ca bool, parent *tls.Certificate) tls.Certificate {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  ca,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if ca {
		cert.KeyUsage |= x509.KeyUsageCertSign
	}
	issuer, signer := cert, any(key)
	if parent != nil {
		issuer, signer = parent.Leaf, parent.PrivateKey
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, issuer, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{der}
	if parent != nil {
		chain = append(chain, parent.Certificate...)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}
}

func startCameraTestRelay(t *testing.T, authority string) (context.Context, string, net.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	config, err := cameraTunnelTLS()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	upstream, printer := net.Pipe()
	t.Cleanup(func() { printer.Close() })
	deadline, _ := ctx.Deadline()
	printer.SetDeadline(deadline)
	go relayCameraRTSP(ctx, listener, upstream, authority, config)
	return ctx, listener.Addr().String(), printer
}

func TestCameraRelayPreservesRTSPRequests(t *testing.T) {
	const authority = "[fd00::42]:322"
	for _, scheme := range []string{"rtsp", "rtsps"} {
		t.Run(scheme, func(t *testing.T) {
			ctx, proxy, printer := startCameraTestRelay(t, authority)
			local, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy)
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			deadline, _ := ctx.Deadline()
			local.SetDeadline(deadline)
			if _, err := fmt.Fprintf(local, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, proxy); err != nil {
				t.Fatal(err)
			}
			res, err := http.ReadResponse(bufio.NewReader(local), &http.Request{Method: http.MethodConnect})
			if err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT response = %v, %v", res, err)
			}
			client := tls.Client(local, &tls.Config{InsecureSkipVerify: true})
			uri := scheme + "://" + authority + "/streaming/live/1?track=%2F"
			request := fmt.Sprintf("SET_PARAMETER %s RTSP/1.0\r\nAuthorization: Digest uri=\"%s\", response=\"abc\"\r\nContent-Length: 4\r\n\r\nbody", uri, uri)
			wire := request + "$\x01\x00\x04rtcp"
			if _, err := io.WriteString(client, wire); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(wire))
			if _, err := io.ReadFull(printer, got); err != nil || string(got) != wire {
				t.Fatalf("relayed request = %q, %v, want %q", got, err, wire)
			}
		})
	}
}

func TestCameraFFmpegDigest(t *testing.T) {
	executable, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	const secret = "test:/+code"
	const realm, nonce = "printer-camera", "printer-nonce"
	target := url.URL{Scheme: "rtsps", Host: "camera.invalid:322", User: url.UserPassword(bambuUser, secret), Path: "/streaming/live/1"}
	ctx, proxy, printer := startCameraTestRelay(t, target.Host)
	t.Setenv("http_proxy", "http://unused.invalid:12345")
	t.Setenv("no_proxy", target.Hostname())
	cmd := cameraRTSPCommand(ctx, executable, target, proxy)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		if t.Failed() {
			t.Log(stderr.String())
		}
	}()
	target.User = nil
	reader := textproto.NewReader(bufio.NewReader(printer))
	// End with an RTSP error after checking the authenticated retry.
	for _, step := range []struct{ method, status, headers string }{
		{"OPTIONS", "200 OK", "Public: OPTIONS, DESCRIBE, SETUP, PLAY\r\n"},
		{"DESCRIBE", "401 Unauthorized", fmt.Sprintf("WWW-Authenticate: Digest realm=\"%s\", nonce=\"%s\", qop=\"auth\"\r\n", realm, nonce)},
		{"DESCRIBE", "404 Not Found", ""},
	} {
		line, err := reader.ReadLine()
		if err != nil || line != step.method+" "+target.String()+" RTSP/1.0" {
			t.Fatalf("%s request line = %q, %v", step.status, line, err)
		}
		headers, err := reader.ReadMIMEHeader()
		if err != nil {
			t.Fatal(err)
		}
		if step.status == "404 Not Found" {
			auth, ok := strings.CutPrefix(headers.Get("Authorization"), "Digest ")
			if !ok {
				t.Fatal("missing Digest authorization on the retry")
			}
			params := map[string]string{}
			for _, param := range strings.Split(auth, ",") {
				key, value, _ := strings.Cut(strings.TrimSpace(param), "=")
				params[key] = strings.Trim(value, `"`)
			}
			a1 := md5.Sum([]byte(bambuUser + ":" + realm + ":" + secret))
			a2 := md5.Sum([]byte(step.method + ":" + target.String()))
			digest := md5.Sum([]byte(fmt.Sprintf("%x:%s:%s:%s:auth:%x", a1, nonce, params["nc"], params["cnonce"], a2)))
			if params["username"] != bambuUser || params["uri"] != target.String() || params["qop"] != "auth" || params["response"] != fmt.Sprintf("%x", digest) {
				t.Fatalf("invalid Digest authorization for %s", line)
			}
		}
		if _, err := fmt.Fprintf(printer, "RTSP/1.0 %s\r\nCSeq: %s\r\n%s\r\n", step.status, headers.Get("CSeq"), step.headers); err != nil {
			t.Fatal(err)
		}
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || !strings.Contains(stderr.String(), "404 Not Found") {
		t.Fatalf("expected ffmpeg to report the fake camera's 404, got %v: %s", err, stderr.String())
	}
}
