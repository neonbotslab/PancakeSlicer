//go:build go1.25

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type fakeBambuOptions struct {
	noPingResponse bool
	noStatus       bool
	fragment       bool
	badFields      bool
}

type fakeBambuResult struct {
	pings, pushalls int
	keepalive       time.Duration
	maxWriteGap     time.Duration
	elapsed         time.Duration
	err             error
}

func fakeBambuBroker(ctx context.Context, conn net.Conn, options fakeBambuOptions) <-chan fakeBambuResult {
	result := make(chan fakeBambuResult, 1)
	go func() {
		began := time.Now()
		out := fakeBambuResult{}
		defer func() {
			conn.Close()
			out.elapsed = time.Since(began)
			result <- out
		}()
		r := bufio.NewReader(conn)
		packet, err := mqttReadPacket(r)
		if err != nil || packet.header != 0x10 || len(packet.body) < 10 {
			out.err = fmt.Errorf("CONNECT packet: %v", err)
			return
		}
		out.keepalive = time.Duration(binary.BigEndian.Uint16(packet.body[8:10])) * time.Second
		if out.keepalive <= 0 {
			out.err = errors.New("client disabled keepalive")
			return
		}
		if err := mqttWrite(conn, 0x20, []byte{0, 0}); err != nil {
			out.err = err
			return
		}
		packet, err = mqttReadPacket(r)
		if err != nil || packet.header != 0x82 || len(packet.body) < 2 {
			out.err = fmt.Errorf("SUBSCRIBE packet: %v", err)
			return
		}
		if err := mqttWrite(conn, 0x90, []byte{packet.body[0], packet.body[1], 0}); err != nil {
			out.err = err
			return
		}
		readerCtx, stopReader := context.WithCancel(ctx)
		defer stopReader()
		packets := make(chan mqttPacket)
		readErrors := make(chan error, 1)
		go func() {
			for {
				packet, err := mqttReadPacket(r)
				if err != nil {
					readErrors <- err
					return
				}
				select {
				case packets <- packet:
				case <-readerCtx.Done():
					return
				}
			}
		}()
		deadline := time.NewTimer(out.keepalive * 3 / 2)
		defer deadline.Stop()
		lastPacket := time.Now()
		reports := time.NewTicker(time.Second)
		defer reports.Stop()
		var fragmentRest []byte
		var finishFragment <-chan time.Time
		fragmentSent := false
		pingPending := false
		payload := []byte(`{"print":{"command":"push_status","msg":1,"gcode_state":"RUNNING","mc_percent":42}}`)
		if options.badFields {
			payload = []byte(`{"print":{"command":"push_status","msg":1,"gcode_state":"RUNNING","nozzle_temper":"synthetic-secret","device":{"extruder":{"info":[{"id":0,"temp":"synthetic"}]}},"unexpected":"device/synthetic/report"}}`)
		}
		reportBody := append(mqttString("device/synthetic/report"), payload...)
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-readErrors:
				if ctx.Err() == nil {
					out.err = err
				}
				return
			case <-deadline.C:
				out.err = errors.New("broker keepalive expired while reports were streaming")
				return
			case packet := <-packets:
				out.maxWriteGap = max(out.maxWriteGap, time.Since(lastPacket))
				lastPacket = time.Now()
				deadline.Reset(out.keepalive * 3 / 2)
				switch packet.header {
				case 0xC0:
					out.pings++
					if !options.noPingResponse {
						if fragmentRest != nil {
							pingPending = true
							continue
						}
						if err := mqttWrite(conn, 0xD0, nil); err != nil {
							return
						}
					}
				case 0x30:
					out.pushalls++
				}
			case <-reports.C:
				if options.noStatus || fragmentRest != nil {
					continue
				}
				if options.fragment && !fragmentSent && time.Since(began) >= 5*time.Second {
					var encoded bytes.Buffer
					mqttWrite(&encoded, 0x30, reportBody)
					wire := encoded.Bytes()
					fragmentRest = wire[len(wire)-4:]
					if _, err := conn.Write(wire[:len(wire)-4]); err != nil {
						return
					}
					finishFragment = time.After(35 * time.Second)
					fragmentSent = true
				} else if err := mqttWrite(conn, 0x30, reportBody); err != nil {
					return
				}
			case <-finishFragment:
				if _, err := conn.Write(fragmentRest); err != nil {
					return
				}
				fragmentRest, finishFragment = nil, nil
				if pingPending {
					if err := mqttWrite(conn, 0xD0, nil); err != nil {
						return
					}
					pingPending = false
				}
			}
		}
	}()
	return result
}

func startFakeBambuMonitor(ctx context.Context, conn net.Conn, publish func(PrinterStatus), logger *slog.Logger) <-chan error {
	ended := make(chan error, 1)
	go func() {
		r, err := mqttHandshake(conn, "synthetic", "synthetic-secret")
		if err == nil {
			err = monitorBambuConn(ctx, conn, r, &PrinterConfig{Name: "Test printer", Serial: "synthetic"}, logger, publish, nil, false)
		}
		conn.Close()
		ended <- err
	}()
	return ended
}

func TestBambuMQTTKeepaliveWhileStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		broker := fakeBambuBroker(ctx, peer, fakeBambuOptions{})
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
		updates := 0
		ended := startFakeBambuMonitor(ctx, client, func(st PrinterStatus) {
			if st.Online && st.State == "printing" {
				updates++
			}
		}, logger)
		select {
		case err := <-ended:
			t.Fatalf("streaming session ended before three keepalive periods: %v", err)
		case <-time.After(181 * time.Second):
		}
		cancel()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel returned %v", err)
		}
		out := <-broker
		if out.err != nil || out.keepalive != 60*time.Second || out.pings < 5 || updates < 100 || out.maxWriteGap > 30*time.Second {
			t.Fatalf("streaming session did not stay alive: broker=%+v updates=%d", out, updates)
		}
	})
}

func TestBambuMQTTMissedPingWhileStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		broker := fakeBambuBroker(ctx, peer, fakeBambuOptions{noPingResponse: true})
		ended := startFakeBambuMonitor(ctx, client, func(PrinterStatus) {}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		began := time.Now()
		if err := <-ended; bambuSessionReason(err) != "ping_timeout" || !strings.Contains(err.Error(), "PINGRESP") {
			t.Fatalf("missing ping response returned %v", err)
		}
		if elapsed := time.Since(began); elapsed < 45*time.Second || elapsed > 46*time.Second {
			t.Fatalf("ping response timeout took %s", elapsed)
		}
		cancel()
		if out := <-broker; out.pings != 1 {
			t.Fatalf("wanted one pending ping, got %+v", out)
		}
	})
}

func TestBambuMQTTBoundedStatusRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		broker := fakeBambuBroker(ctx, peer, fakeBambuOptions{noStatus: true})
		ended := startFakeBambuMonitor(ctx, client, func(PrinterStatus) { t.Error("published missing status") }, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		began := time.Now()
		if err := <-ended; bambuSessionReason(err) != "no_usable_status" {
			t.Fatalf("missing status returned %v", err)
		}
		if elapsed := time.Since(began); elapsed != 30*time.Second {
			t.Fatalf("missing status waited %s", elapsed)
		}
		cancel()
		if out := <-broker; out.pushalls != 2 {
			t.Fatalf("wanted initial pushall and one retry, got %+v", out)
		}
	})
}

func TestBambuMQTTFragmentAcrossPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		broker := fakeBambuBroker(ctx, peer, fakeBambuOptions{fragment: true})
		updates := 0
		ended := startFakeBambuMonitor(ctx, client, func(st PrinterStatus) { updates++ }, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		select {
		case err := <-ended:
			t.Fatalf("fragmented packet ended session: %v", err)
		case <-time.After(65 * time.Second):
		}
		cancel()
		<-ended
		if out := <-broker; out.pings < 2 || updates < 20 {
			t.Fatalf("fragmented stream did not recover: %+v updates=%d", out, updates)
		}
	})
}

func TestBambuMQTTRejectsInvalidSuback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header byte
		body   []byte
	}{
		{"rejected", 0x90, []byte{0, 1, 0x80}},
		{"wrong id", 0x90, []byte{0, 2, 0}},
		{"wrong length", 0x90, []byte{0, 1}},
		{"wrong flags", 0x91, []byte{0, 1, 0}},
		{"wrong qos", 0x90, []byte{0, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				r := bufio.NewReader(peer)
				mqttRead(r)
				mqttWrite(peer, 0x20, []byte{0, 0})
				mqttRead(r)
				mqttWrite(peer, tc.header, tc.body)
			}()
			if _, err := mqttHandshake(client, "synthetic", "synthetic-secret"); bambuSessionReason(err) != "mqtt_suback" {
				t.Fatalf("invalid subscription reply returned %v", err)
			}
			<-done
		})
	}
}

func TestBambuMQTTReportBeforeSuback(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReader(peer)
		mqttRead(r)
		mqttWrite(peer, 0x20, []byte{0, 0})
		mqttRead(r)
		mqttWrite(peer, 0x30, append(mqttString("device/synthetic/report"), []byte(`{"print":{"gcode_state":"IDLE"}}`)...))
		mqttWrite(peer, 0x90, []byte{0, 1, 0})
	}()
	r, err := mqttHandshake(client, "synthetic", "synthetic-secret")
	if err != nil {
		t.Fatal(err)
	}
	kind, body, err := mqttRead(r)
	if err != nil || !bytes.Contains(mqttPayload(kind, body), []byte(`"gcode_state":"IDLE"`)) {
		t.Fatalf("report before SUBACK was lost: kind=%d body=%q error=%v", kind, body, err)
	}
	<-done
}

func TestBambuMQTTDecodeLogsAreBoundedAndPrivate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		broker := fakeBambuBroker(ctx, peer, fakeBambuOptions{badFields: true})
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, nil))
		updates := 0
		ended := startFakeBambuMonitor(ctx, client, func(PrinterStatus) { updates++ }, logger)
		time.Sleep(20 * time.Second)
		cancel()
		<-ended
		<-broker
		decodeErrors := 0
		for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("session log is not one JSON object per line: %v", err)
			}
			if event["printer"] != "Test printer" {
				t.Fatalf("session log lost display name: %s", line)
			}
			if event["msg"] == "bambu_mqtt_decode_error" {
				decodeErrors++
			}
		}
		if decodeErrors != 1 || updates < 10 {
			t.Fatalf("bad fields should log once while valid status continues: errors=%d updates=%d", decodeErrors, updates)
		}
		for _, forbidden := range []string{"synthetic", "device/", "nozzle_temper", "access_code", "serial", "host", "payload"} {
			if strings.Contains(output.String(), forbidden) {
				t.Fatalf("session logs contain private data %q: %s", forbidden, output.String())
			}
		}
	})
}

func TestBambuMQTTSubackTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			r := bufio.NewReader(peer)
			mqttRead(r)
			mqttWrite(peer, 0x20, []byte{0, 0})
			mqttRead(r)
		}()
		began := time.Now()
		if _, err := mqttHandshake(client, "synthetic", "synthetic-secret"); bambuSessionReason(err) != "mqtt_suback" {
			t.Fatalf("missing SUBACK returned %v", err)
		}
		if elapsed := time.Since(began); elapsed != 15*time.Second {
			t.Fatalf("missing SUBACK waited %s", elapsed)
		}
		<-done
	})
}

func TestBambuMQTTCancelDuringPartialPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		broker := fakeBambuBroker(ctx, peer, fakeBambuOptions{fragment: true})
		ended := startFakeBambuMonitor(ctx, client, func(PrinterStatus) {}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		time.Sleep(6 * time.Second)
		cancel()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel during partial read returned %v", err)
		}
		<-broker
	})
}

func TestDialTLSCancellationClosesSocket(t *testing.T) {
	certificate, err := cameraTLSListener()
	if err != nil {
		t.Fatal(err)
	}
	defer certificate.Close()
	closed := make(chan error, 1)
	go func() {
		conn, err := certificate.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(io.Discard, conn)
		closed <- err
	}()
	address := certificate.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := dialTLS(ctx, address.IP.String(), address.Port, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Hour))
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close TLS socket after deadlines changed")
	}
}
