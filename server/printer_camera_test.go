package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
