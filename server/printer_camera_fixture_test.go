package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func cameraFixtureAddress(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("local test addresses unavailable: %v", err)
	}
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil && prefix.Addr().IsPrivate() && denied(prefix.Addr()) == "" {
			return prefix.Addr()
		}
	}
	t.Skip("camera fixture requires a private local test address allowed by the printer network policy")
	return netip.Addr{}
}

func cameraFixtureServer(t *testing.T, addr netip.Addr, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewUnstartedServer(handler)
	fixture.Listener.Close()
	fixture.Listener = listener
	t.Cleanup(fixture.Close)
	return fixture
}

func TestCameraSharesUpstreamUntilIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	addr := cameraFixtureAddress(t)
	policy, err := parseHostPolicy(addr.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	m := NewSendManager(nil)
	type counts struct{ active, peak, accepted int }
	type fixture struct {
		printer string
		sockets func() counts
		idle    <-chan struct{}
	}
	makeFixture := func(printer string) fixture {
		var mu sync.Mutex
		var sockets counts
		idle := make(chan struct{}, 1)
		camera := cameraFixtureServer(t, addr, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=fixture")
			controller := http.NewResponseController(w)
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				_, err := fmt.Fprintf(w, "--fixture\r\nContent-Type: image/jpeg\r\n\r\n\xff\xd8%s\xff\xd9\r\n", printer)
				if err != nil || controller.Flush() != nil {
					return
				}
				select {
				case <-r.Context().Done():
					return
				case <-tick.C:
				}
			}
		})
		camera.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			mu.Lock()
			defer mu.Unlock()
			switch state {
			case http.StateNew:
				sockets.active++
				sockets.accepted++
				sockets.peak = max(sockets.peak, sockets.active)
			case http.StateClosed:
				sockets.active--
				if sockets.active == 0 {
					select {
					case idle <- struct{}{}:
					default:
					}
				}
			}
		}
		camera.StartTLS()
		metadata := cameraFixtureServer(t, addr, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/server/webcams/list" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"result":{"webcams":[{"name":"fixture","enabled":true,"stream_url":%q}]}}`, camera.URL)
		})
		metadata.Start()
		fingerprint := sha256.Sum256(camera.Certificate().Raw)
		m.conns = append(m.conns, &PrinterConfig{
			Printer: printer, Kind: "moonraker", URL: metadata.URL, TLS: "pin", fingerprint: fingerprint[:], hosts: policy,
		})
		t.Cleanup(func() { m.cameras.disconnect(printer) })
		return fixture{printer: printer, idle: idle, sockets: func() counts {
			mu.Lock()
			defer mu.Unlock()
			return sockets
		}}
	}
	fixtures := []fixture{makeFixture("first"), makeFixture("second")}
	mux := http.NewServeMux()
	server := &Server{sends: m}
	mux.HandleFunc("GET /api/printer-connections/{printer}/camera/{camera}", server.printerCamera)
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	type viewer struct {
		body io.ReadCloser
		err  error
	}
	join := func(printer string) viewer {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/api/printer-connections/"+printer+"/camera/0", nil)
		if err != nil {
			return viewer{err: err}
		}
		resp, err := proxy.Client().Do(req)
		if err != nil {
			return viewer{err: err}
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return viewer{err: fmt.Errorf("printer %s camera returned %d", printer, resp.StatusCode)}
		}
		want := fmt.Sprintf("--orca-camera\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n\xff\xd8%s\xff\xd9\r\n", len(printer)+4, printer)
		got := make([]byte, len(want))
		if _, err = io.ReadFull(resp.Body, got); err == nil && !bytes.Equal(got, []byte(want)) {
			err = fmt.Errorf("printer %s received %q", printer, got)
		}
		if err != nil {
			resp.Body.Close()
			return viewer{err: err}
		}
		return viewer{body: resp.Body}
	}
	const viewersPerPrinter = 5
	joined := make(chan viewer, viewersPerPrinter*len(fixtures))
	for _, f := range fixtures {
		for i := 0; i < viewersPerPrinter; i++ {
			go func(printer string) { joined <- join(printer) }(f.printer)
		}
	}
	var viewing []io.ReadCloser
	defer func() {
		for _, body := range viewing {
			body.Close()
		}
	}()
	for i := 0; i < cap(joined); i++ {
		v := cameraTestReceive(t, ctx, joined)
		if v.err != nil {
			t.Fatal(v.err)
		}
		viewing = append(viewing, v.body)
	}
	for _, f := range fixtures {
		if got := f.sockets(); got != (counts{active: 1, peak: 1, accepted: 1}) {
			t.Fatalf("printer %s socket counts with %d viewers: %+v", f.printer, viewersPerPrinter, got)
		}
	}
	for _, body := range viewing {
		body.Close()
	}
	for _, f := range fixtures {
		v := join(f.printer)
		if v.err != nil {
			t.Fatal(v.err)
		}
		viewing = append(viewing, v.body)
	}
	leftAt := time.Now()
	for _, body := range viewing {
		body.Close()
	}
	for _, f := range fixtures {
		cameraTestReceive(t, ctx, f.idle)
		got := f.sockets()
		t.Logf("printer %s: %d simultaneous viewers, maximum concurrent upstream sockets %d, accepted sockets %d, active after idle %d", f.printer, viewersPerPrinter, got.peak, got.accepted, got.active)
		if got != (counts{peak: 1, accepted: 1}) {
			t.Fatalf("printer %s socket counts after idle: %+v", f.printer, got)
		}
	}
	if elapsed := time.Since(leftAt); elapsed < cameraIdleGrace {
		t.Fatalf("camera sockets closed after %s, want at least %s", elapsed, cameraIdleGrace)
	}
}
