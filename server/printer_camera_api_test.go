package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCameraJPEGFanoutDropsFramesAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "example"}
	m, upstream := testCameraManager(t, p)
	m.cameras.idleGrace = 20 * time.Millisecond
	camera := cameraSource{ID: "0", Format: "mjpeg"}
	first, leaveFirst, err := m.subscribeCameraOutput(ctx, p, camera, true)
	if err != nil {
		t.Fatal(err)
	}
	second, leaveSecond, err := m.subscribeCameraOutput(ctx, p, camera, true)
	if err != nil {
		t.Fatal(err)
	}
	stream := cameraTestReceive(t, ctx, upstream.opened)
	for i := byte(0); i < 100; i++ {
		stream.emit([]byte{255, 216, i, 255, 217}, false)
	}
	for _, ch := range []<-chan []byte{first, second} {
		if got := cameraTestReceive(t, ctx, ch); len(got) != 5 || got[2] != 99 {
			t.Fatalf("slow client did not get latest frame: %v", got)
		}
	}
	if active, peak, starts := upstream.counts(p.Printer); active != 1 || peak != 1 || starts != 1 {
		t.Fatalf("not shared: %d %d %d", active, peak, starts)
	}
	leaveFirst()
	leaveFirst()
	stream.emit([]byte{255, 216, 100, 255, 217}, false)
	if frame := cameraTestReceive(t, ctx, second); frame[2] != 100 {
		t.Fatal(frame)
	}
	leaveSecond()
	cameraTestReceive(t, ctx, stream.closed)
	m.cameras.mu.Lock()
	viewers := len(m.cameras.feeds)
	m.cameras.mu.Unlock()
	if viewers != 0 {
		select {
		case <-ctx.Done():
			t.Fatal("upstream not retired")
		case <-stream.stopping:
		}
	}
}

func TestCameraJPEGReconnects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "example"}
	m := NewSendManager(nil)
	m.conns = []*PrinterConfig{p}
	m.cameras.idleGrace = 10 * time.Millisecond
	var starts atomic.Int32
	m.cameras.run = func(ctx context.Context, _ *PrinterConfig, _ cameraSource, emit func([]byte, bool)) error {
		if starts.Add(1) < 3 {
			return errors.New("fixture disconnect")
		}
		emit([]byte{255, 216, 255, 217}, false)
		<-ctx.Done()
		return ctx.Err()
	}
	frames, leave, err := m.subscribeCameraOutput(ctx, p, cameraSource{ID: "0", Format: "mjpeg"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	defer m.cameras.disconnect(p.Printer)
	if frame := cameraTestReceive(t, ctx, frames); len(frame) != 4 || starts.Load() != 3 {
		t.Fatalf("reconnect failed: %v starts %d", frame, starts.Load())
	}
}

func TestCameraAPIHTTPMixedViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "example", Kind: "bambu-lan", model: "P1S"}
	m := NewSendManager(nil)
	m.conns = []*PrinterConfig{p}
	m.cameras.idleGrace = 20 * time.Millisecond
	var starts atomic.Int32
	closed := make(chan struct{})
	var encoded bytes.Buffer
	jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil)
	frame := encoded.Bytes()
	m.cameras.run = func(ctx context.Context, _ *PrinterConfig, _ cameraSource, emit func([]byte, bool)) error {
		starts.Add(1)
		defer close(closed)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			emit(frame, false)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
	}
	s := &Server{cfg: Config{BearerToken: "fixture-token"}, sends: m}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/printers/{printer}/camera/{image}", s.apiPrinterCamera)
	mux.HandleFunc("GET /api/printers/{printer}/cameras", s.apiPrinterCameras)
	mux.HandleFunc("GET /api/printer-connections/{printer}/camera/{camera}", s.printerCamera)
	server := httptest.NewServer(s.middleware(mux))
	defer server.Close()
	request := func(method, path string) *http.Response {
		t.Helper()
		r, _ := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
		r.Header.Set("Authorization", "Bearer fixture-token")
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, suffix := range []string{"stream.mjpeg", "snapshot.jpg"} {
		response := request("HEAD", "/api/printers/example/camera/"+suffix)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
	}
	if starts.Load() != 0 {
		t.Fatal("HEAD opened camera")
	}
	legacy := request("GET", "/api/printer-connections/example/camera/0")
	defer legacy.Body.Close()
	stream := request("GET", "/api/printers/example/camera/stream.mjpeg?camera=0")
	defer stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "multipart/x-mixed-replace; boundary=orca-camera" {
		t.Fatal(stream.StatusCode, stream.Header)
	}
	part, err := multipart.NewReader(stream.Body, "orca-camera").NextPart()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(part)
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatal("incorrect multipart JPEG")
	}
	snapshot := request("GET", "/api/printers/example/camera/snapshot.jpg")
	got, err = io.ReadAll(snapshot.Body)
	snapshot.Body.Close()
	if err != nil || snapshot.StatusCode != 200 || snapshot.Header.Get("Content-Type") != "image/jpeg" || !bytes.Equal(got, frame) {
		t.Fatal("incorrect JPEG snapshot")
	}
	missing := request("GET", "/api/printers/example/camera/snapshot.jpg?camera=missing")
	missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Fatal("unknown camera accepted")
	}
	if starts.Load() != 1 {
		t.Fatalf("opened %d upstreams", starts.Load())
	}
	legacy.Body.Close()
	stream.Body.Close()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("HTTP clients did not release upstream")
	}
}

func TestCameraJPEGDecoderFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=160x90:rate=10", "-t", "2", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-g", "5", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	movie, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan []byte, 1)
	input <- movie
	close(input)
	count := 0
	err = decodeCameraJPEG(ctx, input, func(frame []byte) {
		img, err := jpeg.Decode(bytes.NewReader(frame))
		if err != nil || img.Bounds().Dx() != 160 || img.Bounds().Dy() != 90 {
			t.Error("decoder did not produce a valid JPEG")
		}
		count++
	})
	if count < 2 {
		t.Fatalf("decoded %d frames: %v", count, err)
	}
}

func TestCameraMP4AndJPEGShareUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "example"}
	m, upstream := testCameraManager(t, p)
	camera := cameraSource{ID: "0", Format: "mp4"}
	var decoders atomic.Int32
	m.cameras.decodeJPEG = func(ctx context.Context, input <-chan []byte, emit func([]byte)) error {
		decoders.Add(1)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-input:
				emit([]byte{255, 216, 255, 217})
			}
		}
	}
	_, leaveNative, err := m.subscribeCamera(ctx, p, camera)
	if err != nil {
		t.Fatal(err)
	}
	defer leaveNative()
	stream := cameraTestReceive(t, ctx, upstream.opened)
	stream.emit([]byte("init"), true)
	first, leaveFirst, err := m.subscribeCameraOutput(ctx, p, camera, true)
	if err != nil {
		t.Fatal(err)
	}
	defer leaveFirst()
	second, leaveSecond, err := m.subscribeCameraOutput(ctx, p, camera, true)
	if err != nil {
		t.Fatal(err)
	}
	defer leaveSecond()
	cameraTestReceive(t, ctx, first)
	cameraTestReceive(t, ctx, second)
	if active, peak, starts := upstream.counts(p.Printer); active != 1 || peak != 1 || starts != 1 || decoders.Load() != 1 {
		t.Fatalf("upstream %d %d %d decoders %d", active, peak, starts, decoders.Load())
	}
}

func TestCameraSelectorDoesNotInterruptJPEGViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "example"}
	m, upstream := testCameraManager(t, p)
	frames, leave, err := m.subscribeCameraOutput(ctx, p, cameraSource{ID: "0", Format: "mjpeg"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	stream := cameraTestReceive(t, ctx, upstream.opened)
	busy, cancelBusy := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancelBusy()
	_, _, err = m.subscribeCameraOutput(busy, p, cameraSource{ID: "1", Format: "mjpeg"}, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("second source interrupted active viewer", err)
	}
	stream.emit([]byte{255, 216, 255, 217}, false)
	if frame := cameraTestReceive(t, ctx, frames); len(frame) != 4 {
		t.Fatal(frame)
	}
	if active, peak, starts := upstream.counts(p.Printer); active != 1 || peak != 1 || starts != 1 {
		t.Fatal(active, peak, starts)
	}
}

func TestCameraDecoderHandoffWaitsForExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &PrinterConfig{Printer: "fixture"}
	m := NewSendManager(nil)
	m.conns = []*PrinterConfig{p}
	m.cameras.run = func(ctx context.Context, _ *PrinterConfig, _ cameraSource, emit func([]byte, bool)) error {
		emit([]byte("init"), true)
		<-ctx.Done()
		return ctx.Err()
	}
	var starts atomic.Int32
	firstStarted := make(chan struct{})
	firstStopping := make(chan struct{})
	allowFirstReap := make(chan struct{})
	release := sync.OnceFunc(func() { close(allowFirstReap) })
	defer release()
	secondStarted := make(chan struct{})
	m.cameras.decodeJPEG = func(ctx context.Context, _ <-chan []byte, _ func([]byte)) error {
		if starts.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			close(firstStopping)
			<-allowFirstReap
		} else {
			close(secondStarted)
			<-ctx.Done()
		}
		return ctx.Err()
	}
	defer m.cameras.disconnect(p.Printer)
	source := cameraSource{ID: "0", Format: "mp4"}
	_, leave, err := m.subscribeCameraOutput(ctx, p, source, true)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	cameraTestReceive(t, ctx, firstStarted)
	m.cameras.disconnect(p.Printer)
	cameraTestReceive(t, ctx, firstStopping)
	go func() {
		_, leave, err := m.subscribeCameraOutput(ctx, p, source, true)
		if err == nil {
			defer leave()
			<-ctx.Done()
		}
	}()
	select {
	case <-secondStarted:
		t.Fatal("replacement decoder started before previous decoder returned")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	cameraTestReceive(t, ctx, secondStarted)
}
