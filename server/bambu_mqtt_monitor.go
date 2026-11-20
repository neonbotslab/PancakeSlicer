package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

const (
	bambuMQTTPingInterval = bambuMQTTKeepAlive / 2
	bambuMQTTPingTimeout  = 15 * time.Second
	bambuMQTTWriteTimeout = 10 * time.Second
	bambuStatusRetry      = 10 * time.Second
	bambuStatusTimeout    = 30 * time.Second
)

var bambuSessionLog = slog.New(slog.NewJSONHandler(os.Stdout, nil))

func bambuSessionReason(err error) string {
	var connErr *connError
	switch {
	case err == nil:
		return "closed"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.As(err, &connErr):
		switch connErr.stage {
		case "unreachable", "serial", "tls", "access_code", "mqtt_connack", "mqtt_suback",
			"eof", "mqtt_read", "mqtt_write", "mqtt_packet", "ping_timeout", "no_usable_status":
			return connErr.stage
		}
	}
	return "connection_error"
}

func monitorBambu(ctx context.Context, p *PrinterConfig, secret string, publish func(PrinterStatus), heard func(bambuHeard)) (err error) {
	began := time.Now()
	bambuSessionLog.Info("bambu_mqtt_session_start", "printer", p.Name)
	defer func() {
		reason := err
		if ctx.Err() != nil {
			reason = ctx.Err()
		}
		bambuSessionLog.Info("bambu_mqtt_session_end", "printer", p.Name, "reason", bambuSessionReason(reason), "duration_ms", time.Since(began).Milliseconds())
	}()
	conn, r, err := mqttSession(ctx, p, secret)
	if err != nil {
		return err
	}
	return monitorBambuConn(ctx, conn, r, p, bambuSessionLog, publish, heard)
}

type mqttReadResult struct {
	packet mqttPacket
	err    error
}

func monitorBambuConn(ctx context.Context, conn net.Conn, r *bufio.Reader, p *PrinterConfig, logger *slog.Logger, publish func(PrinterStatus), heard func(bambuHeard)) error {
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &connError{"mqtt_read", netError(err, "the printer (MQTT port 8883)").Error()}
	}

	incoming := make(chan mqttReadResult)
	stopReading := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			packet, err := mqttReadPacket(r)
			select {
			case incoming <- mqttReadResult{packet: packet, err: err}:
			case <-stopReading:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopReading)
		conn.Close()
		<-readerDone
	}()

	var lastWrite time.Time
	write := func(send func() error) error {
		conn.SetWriteDeadline(time.Now().Add(bambuMQTTWriteTimeout))
		if err := send(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &connError{"mqtt_write", netError(err, "the printer (MQTT port 8883)").Error()}
		}
		lastWrite = time.Now()
		return nil
	}
	pushAll := func() error { return requestBambuStatus(conn, p) }
	if err := write(pushAll); err != nil {
		return err
	}
	statusRetryAt := lastWrite.Add(bambuStatusRetry)
	statusDeadline := lastWrite.Add(bambuStatusTimeout)
	var pingDeadline time.Time
	timerAt := statusRetryAt
	timer := time.NewTimer(time.Until(timerAt))
	defer timer.Stop()

	state := map[string]any{}
	loggedIssues := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now := time.Now()
		if !pingDeadline.IsZero() && !now.Before(pingDeadline) {
			return &connError{"ping_timeout", "the printer did not send MQTT PINGRESP before the keepalive deadline"}
		}
		if !statusDeadline.IsZero() && !now.Before(statusDeadline) {
			return &connError{"no_usable_status", "the printer did not report a print state after MQTT status requests"}
		}
		if pingDeadline.IsZero() && !statusRetryAt.IsZero() && !now.Before(statusRetryAt) {
			if err := write(pushAll); err != nil {
				return err
			}
			statusRetryAt = time.Time{}
		}
		if pingDeadline.IsZero() && !now.Before(lastWrite.Add(bambuMQTTPingInterval)) {
			if err := write(func() error { return mqttWrite(conn, 0xC0, nil) }); err != nil {
				return err
			}
			pingDeadline = lastWrite.Add(bambuMQTTPingTimeout)
		}

		wakeAt := lastWrite.Add(bambuMQTTPingInterval)
		if !pingDeadline.IsZero() {
			wakeAt = pingDeadline
		}
		if !statusDeadline.IsZero() && statusDeadline.Before(wakeAt) {
			wakeAt = statusDeadline
		}
		if pingDeadline.IsZero() && !statusRetryAt.IsZero() && statusRetryAt.Before(wakeAt) {
			wakeAt = statusRetryAt
		}
		if wakeAt != timerAt {
			if !timerAt.IsZero() && !timer.Stop() {
				<-timer.C
			}
			timer.Reset(time.Until(wakeAt))
			timerAt = wakeAt
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			timerAt = time.Time{}
		case result := <-incoming:
			if result.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(result.err, io.EOF) {
					return &connError{"eof", "the printer closed the MQTT connection"}
				}
				return &connError{"mqtt_read", netError(result.err, "the printer (MQTT port 8883)").Error()}
			}
			packet := result.packet
			switch packet.header >> 4 {
			case 13:
				if packet.header != 0xD0 || len(packet.body) != 0 {
					return &connError{"mqtt_packet", "malformed MQTT keepalive reply"}
				}
				pingDeadline = time.Time{}
			case 3:
				if packet.header&0x06 != 0 {
					return &connError{"mqtt_packet", "unexpected MQTT report delivery level"}
				}
				payload := mqttPayload(3, packet.body)
				if payload == nil {
					return &connError{"mqtt_packet", "malformed MQTT report packet"}
				}
				report, event, issue := decodeBambuReportUpdate(state, payload)
				if issue != "" && !loggedIssues[issue] {
					logger.Warn("bambu_mqtt_decode_error", "printer", p.Name, "reason", issue)
					loggedIssues[issue] = true
				}
				if report == nil {
					continue
				}
				if report.State != "" {
					statusRetryAt, statusDeadline = time.Time{}, time.Time{}
					status := report.status()
					publish(status)
					event.status = &status
				} else if statusDeadline.IsZero() {
					now := time.Now()
					statusRetryAt = now.Add(bambuStatusRetry)
					statusDeadline = now.Add(bambuStatusTimeout)
				}
				if heard != nil {
					if event.status != nil {
						event.state = event.status.Trays
					} else {
						event.state = report.trays()
					}
					heard(event)
				}
			}
		}
	}
}
