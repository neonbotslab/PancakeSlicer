package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const bambuUser = "bblp"
const bambuMQTTKeepAlive = 60 * time.Second

func sendBambu(ctx context.Context, u *upload) (string, error) {
	p := u.printer
	if strings.ContainsAny(u.filename, "\r\n/") {
		return "", errors.New("That file name contains characters that can't be sent over FTP.")
	}
	plate, err := packagePlate(u.file)
	if err != nil {
		return "", err
	}
	digest := md5.New()
	if _, err := io.Copy(digest, u.file); err != nil {
		return "", err
	}
	if _, err := u.file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tlsConfig := p.tlsConfig(p.Serial)
	tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(4)
	host, err := p.resolvedHost(ctx)
	if err != nil {
		return "", err
	}
	md5sum := hex.EncodeToString(digest.Sum(nil))
	internalStorage := u.internalStorage
	if internalStorage == nil {
		internalStorage, err = bambuInternalStorage(ctx, p, u.secret)
		if err != nil {
			return "", err
		}
	}
	fileURL := ""
	err = errBambuTunnelUnsupported
	if *internalStorage {
		fileURL, err = bambuTunnelUpload(ctx, host, tlsConfig, u, md5sum)
	}
	if errors.Is(err, errBambuTunnelUnsupported) {
		fileURL = "file:///sdcard/" + u.filename
		err = ftpsUpload(ctx, host, tlsConfig, u)
		md5sum = strings.ToUpper(md5sum)
	} else if err == nil {
		md5sum = "from_sd_card"
	}
	if err != nil {
		return "", err
	}
	u.stored()
	if !u.start {
		return u.placed() + ".", nil
	}
	u.starting()
	return mqttProjectFile(ctx, u, plate, md5sum, fileURL)
}

var plateEntry = regexp.MustCompile(`^Metadata/plate_[0-9]+\.gcode$`)

func packagePlate(f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	z, err := zip.NewReader(f, st.Size())
	if err != nil {
		return "", errors.New("The exported file isn't a valid .gcode.3mf package.")
	}
	for _, entry := range z.File {
		if plateEntry.MatchString(entry.Name) {
			return entry.Name, nil
		}
	}
	return "", errors.New("The .gcode.3mf package doesn't contain any sliced plate G-code.")
}

type connError struct{ stage, msg string }

func (e *connError) Error() string { return e.msg }

type contextConn struct {
	net.Conn
	stop func() bool
}

func (c *contextConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func dialTLS(ctx context.Context, host string, port int, config *tls.Config) (*tls.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, dialError(err, host, port)
	}
	bound := &contextConn{Conn: raw, stop: context.AfterFunc(ctx, func() { raw.Close() })}
	conn := tls.Client(bound, config)
	if err := conn.HandshakeContext(dctx); err != nil {
		bound.Close()
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, handshakeError(err, port)
	}
	if cause := context.Cause(ctx); cause != nil {
		bound.Close()
		return nil, cause
	}
	return conn, nil
}

func dialError(err error, host string, port int) error {
	var dns *net.DNSError
	msg := fmt.Sprintf("cannot connect to %s on port %d: %s", host, port, lastClause(err))
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		msg = fmt.Sprintf("%s refused port %d: LAN-only mode is off, or this is not the printer's address", host, port)
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		msg = fmt.Sprintf("no answer from %s on port %d: check the IP address and that the printer is on the server's network", host, port)
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		msg = fmt.Sprintf("no route from the server to %s", host)
	case errors.As(err, &dns):
		msg = "couldn't find " + host
	}
	return &connError{"unreachable", msg}
}

func handshakeError(err error, port int) error {
	var serial *serialMismatch
	switch {
	case errors.As(err, &serial):
		return &connError{"serial", serial.Error() + "; check the serial number (Settings > Device on the printer)"}
	case errors.Is(err, errPinMismatch):
		return &connError{"tls", "the printer's certificate changed since it was first trusted; if the printer was replaced or reset, remove it and add it again"}
	}
	return &connError{"tls", fmt.Sprintf("TLS handshake on port %d failed: %s", port, lastClause(err))}
}

type ftpConn struct {
	conn *tls.Conn
	r    *bufio.Reader
}

func (c *ftpConn) reply(timeout time.Duration) (int, string, error) {
	c.conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := c.r.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	if len(line) < 4 {
		return 0, "", errors.New("malformed FTP reply")
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", errors.New("malformed FTP reply")
	}
	text := strings.TrimSpace(line[4:])
	if line[3] == '-' {
		for {
			next, err := c.r.ReadString('\n')
			if err != nil {
				return 0, "", err
			}
			if strings.HasPrefix(next, line[:3]+" ") {
				text = strings.TrimSpace(next[4:])
				break
			}
		}
	}
	return code, text, nil
}

func (c *ftpConn) cmd(want int, timeout time.Duration, format string, args ...any) (string, error) {
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(c.conn, format+"\r\n", args...); err != nil {
		return "", err
	}
	code, text, err := c.reply(timeout)
	if err != nil {
		return "", err
	}
	if code/100 != want/100 && code != want {
		return text, &ftpError{code, text}
	}
	return text, nil
}

type ftpError struct {
	code int
	text string
}

func (e *ftpError) Error() string { return fmt.Sprintf("FTP %d %s", e.code, e.text) }

var pasvReply = regexp.MustCompile(`(\d+),(\d+),(\d+),(\d+),(\d+),(\d+)`)

func ftpsLogin(ctx context.Context, host string, config *tls.Config, secret string) (c *ftpConn, err error) {
	conn, err := dialTLS(ctx, host, 990, config)
	if err != nil {
		return nil, err
	}
	c = &ftpConn{conn: conn, r: bufio.NewReader(conn)}
	defer func() {
		if err != nil {
			conn.Close()
			err = ftpFailure(ctx, err, "login")
		}
	}()
	if code, text, rerr := c.reply(15 * time.Second); rerr != nil {
		return nil, rerr
	} else if code != 220 {
		return nil, &ftpError{code, text}
	}
	if _, err = c.cmd(331, 15*time.Second, "USER %s", bambuUser); err != nil {
		return nil, err
	}
	if _, err = c.cmd(230, 15*time.Second, "PASS %s", secret); err != nil {
		return nil, err
	}
	return c, nil
}

func ftpFailure(ctx context.Context, err error, step string) error {
	var ftpErr *ftpError
	var connErr *connError
	switch {
	case context.Cause(ctx) != nil:
		return context.Cause(ctx)
	case errors.As(err, &connErr):
		return err
	case errors.As(err, &ftpErr) && ftpErr.code == 530:
		return &connError{"access_code", "the printer rejected the access code (FTPS); check it on the printer's screen (Settings > Network)"}
	case errors.As(err, &ftpErr) && ftpErr.code == 522:
		return errors.New("The printer declined the FTP data transfer (TLS session reuse). Please report this along with your printer's firmware version.")
	case errors.As(err, &ftpErr):
		return fmt.Errorf("File transfer to the printer failed at %s: %v", step, ftpErr)
	}
	return netError(err, "the printer (FTPS port 990, "+step+")")
}

func ftpsUpload(ctx context.Context, host string, config *tls.Config, u *upload) (err error) {
	c, err := ftpsLogin(ctx, host, config, u.secret)
	if err != nil {
		return err
	}
	defer c.conn.Close()
	step := "setup"
	defer func() {
		if err != nil {
			err = ftpFailure(ctx, err, step)
		}
	}()
	for _, command := range []string{"PBSZ 0", "PROT P", "TYPE I"} {
		if _, err = c.cmd(200, 15*time.Second, "%s", command); err != nil {
			return err
		}
	}
	if u.resend {
		if text, err := c.cmd(213, 15*time.Second, "SIZE %s", u.filename); err == nil && strings.TrimSpace(text) == strconv.FormatInt(u.size, 10) {
			u.onCard = true
			u.progress.Store(u.size)
			c.cmd(221, 5*time.Second, "QUIT")
			return nil
		}
	}
	text, err := c.cmd(227, 15*time.Second, "PASV")
	if err != nil {
		return err
	}
	m := pasvReply.FindStringSubmatch(text)
	if m == nil {
		return errors.New("malformed PASV reply")
	}
	hi, _ := strconv.Atoi(m[5])
	lo, _ := strconv.Atoi(m[6])
	step = "data connection"
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(hi<<8|lo)))
	if err != nil {
		return err
	}
	defer raw.Close()
	limitSendBuffer(raw)
	step = "upload"
	if _, err = c.cmd(150, 15*time.Second, "STOR %s", u.filename); err != nil {
		return err
	}
	data := tls.Client(raw, config)
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	data.SetDeadline(time.Now().Add(15 * time.Second))
	if err = data.HandshakeContext(ctx); err != nil {
		return err
	}
	data.SetDeadline(time.Time{})
	buf := make([]byte, 64<<10)
	for {
		n, rerr := u.file.Read(buf)
		if n > 0 {
			data.SetWriteDeadline(time.Now().Add(sendStall))
			if _, err = data.Write(buf[:n]); err != nil {
				return err
			}
			u.progress.Add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err = data.CloseWrite(); err != nil {
		return err
	}
	data.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, data)
	data.Close()
	step = "completion"
	code, text, err := c.reply(2 * time.Minute)
	if err != nil {
		return err
	}
	if code != 226 && code != 250 {
		return &ftpError{code, text}
	}
	c.cmd(221, 5*time.Second, "QUIT")
	return nil
}

func mqttSession(ctx context.Context, p *PrinterConfig, secret string) (*tls.Conn, *bufio.Reader, error) {
	host, err := p.resolvedHost(ctx)
	if err != nil {
		return nil, nil, err
	}
	conn, err := dialTLS(ctx, host, 8883, p.tlsConfig(p.Serial))
	if err != nil {
		return nil, nil, err
	}
	r, err := mqttHandshake(conn, p.Serial, secret)
	if err != nil {
		conn.Close()
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		return nil, nil, err
	}
	return conn, r, nil
}

func mqttHandshake(conn net.Conn, serial, secret string) (*bufio.Reader, error) {
	r := bufio.NewReader(conn)
	id := make([]byte, 6)
	rand.Read(id)
	connect := []byte{0, 4, 'M', 'Q', 'T', 'T', 4, 0xC2}
	connect = binary.BigEndian.AppendUint16(connect, uint16(bambuMQTTKeepAlive/time.Second))
	connect = append(connect, mqttString("pancakeslicer-"+hex.EncodeToString(id))...)
	connect = append(connect, mqttString(bambuUser)...)
	connect = append(connect, mqttString(secret)...)
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := mqttWrite(conn, 0x10, connect); err != nil {
		return nil, netError(err, "the printer (MQTT port 8883)")
	}
	packet, err := mqttReadPacket(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &connError{"access_code", "the printer closed the MQTT connection at login; check the access code"}
		}
		return nil, netError(err, "the printer (MQTT port 8883)")
	}
	if packet.header != 0x20 || len(packet.body) != 2 {
		return nil, &connError{"mqtt_connack", "unexpected MQTT connection reply"}
	}
	switch packet.body[1] {
	case 0:
	case 4, 5:
		return nil, &connError{"access_code", "the printer rejected the access code (MQTT); check it on the printer's screen (Settings > Network)"}
	default:
		return nil, &connError{"mqtt_connack", fmt.Sprintf("The printer declined the MQTT connection (code %d).", packet.body[1])}
	}
	const subscriptionID = 1
	subscribe := append(binary.BigEndian.AppendUint16(nil, subscriptionID), mqttString("device/"+serial+"/report")...)
	subscribe = append(subscribe, 0)
	if err := mqttWrite(conn, 0x82, subscribe); err != nil {
		return nil, err
	}
	var earlyReports bytes.Buffer
	for {
		packet, err := mqttReadPacket(r)
		if err != nil {
			return nil, &connError{"mqtt_suback", "could not read the printer's MQTT subscription reply"}
		}
		if packet.header>>4 == 3 && earlyReports.Len()+len(packet.body)+5 <= maxMQTTPacketSize {
			mqttWrite(&earlyReports, packet.header, packet.body)
			continue
		}
		if packet.header != 0x90 || len(packet.body) != 3 || binary.BigEndian.Uint16(packet.body) != subscriptionID {
			return nil, &connError{"mqtt_suback", "unexpected MQTT subscription reply"}
		}
		if packet.body[2] != 0 {
			return nil, &connError{"mqtt_suback", "the printer rejected the MQTT report subscription"}
		}
		if earlyReports.Len() > 0 {
			r = bufio.NewReader(io.MultiReader(&earlyReports, r))
		}
		return r, nil
	}
}

func mqttPublish(conn io.Writer, p *PrinterConfig, message any) error {
	command, _ := json.Marshal(message)
	return mqttWrite(conn, 0x30, append(mqttString("device/"+p.Serial+"/request"), command...))
}

func mqttPayload(kind byte, body []byte) []byte {
	if kind != 3 || len(body) < 2 {
		return nil
	}
	topicLength := int(binary.BigEndian.Uint16(body))
	if topicLength == 0 || 2+topicLength > len(body) {
		return nil
	}
	return body[2+topicLength:]
}

func refusedForAuth(result, reason string, code int) bool {
	return !strings.EqualFold(result, "success") && (strings.Contains(strings.ToLower(reason), "verif") || code == 84033543)
}

func (u *upload) bambuProjectFileMessage(sequence, plate, md5sum, fileURL string) (map[string]any, error) {
	legacyMapping, addresses := u.options.amsMapping, u.options.amsMapping2
	if len(legacyMapping) != len(addresses) {
		return nil, errors.New("The AMS mapping is incomplete. Pick the trays and send again.")
	}
	if legacyMapping == nil {
		legacyMapping, addresses = []int{}, []bambuTrayAddress{}
	}
	flowCali := u.options.flowCali
	if flowCali == flowCalibrationAuto && !u.printer.supportsAutoFlowCalibration() {
		flowCali = flowCalibrationOn
	}
	return map[string]any{"print": map[string]any{
		"sequence_id": sequence, "command": "project_file", "param": plate,
		"project_id": "0", "profile_id": "0", "task_id": "0", "subtask_id": "0", "subtask_name": strings.TrimSuffix(u.filename, ".gcode.3mf"),
		"file": u.filename, "url": fileURL, "md5": md5sum,
		"timelapse": u.options.timelapse, "bed_type": "auto", "bed_leveling": u.options.bedLeveling,
		"flow_cali": flowCali == flowCalibrationOn, "extrude_cali_flag": flowCali,
		"vibration_cali": u.options.vibrationCali, "layer_inspect": false, "use_ams": u.options.useAMS, "ams_mapping": legacyMapping, "ams_mapping2": addresses,
	}}, nil
}

func mqttProjectFile(ctx context.Context, u *upload, plate, md5sum, fileURL string) (string, error) {
	fail := func(err error) (string, error) {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		return "", fmt.Errorf("%s, but the printer couldn't start the print automatically: %w", u.placed(), err)
	}
	p := u.printer
	conn, r, err := mqttSession(ctx, p, u.secret)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	sequence := strconv.FormatInt(time.Now().UnixMilli()%1_000_000_000, 10)
	message, err := u.bambuProjectFileMessage(sequence, plate, md5sum, fileURL)
	if err != nil {
		return fail(err)
	}
	if u.checkStart != nil {
		if err := u.checkStart(); err != nil {
			return fail(err)
		}
	}
	if err := mqttPublish(conn, p, message); err != nil {
		return fail(err)
	}
	name := strings.TrimSuffix(u.filename, ".gcode.3mf")
	start := &bambuStart{state: StartSending}
	conn.SetDeadline(time.Now().Add(bambuStartWindow))
	for start.state == StartSending {
		kind, body, err := mqttRead(r)
		if err != nil {
			if os.IsTimeout(err) && ctx.Err() == nil {
				start.to(StartUnconfirmed, nil)
			} else {
				start.to(StartFailed, err)
			}
			break
		}
		payload := mqttPayload(kind, body)
		if payload == nil {
			continue
		}
		var message struct {
			Print struct {
				bambuEcho
				State     string `json:"gcode_state"`
				Subtask   string `json:"subtask_name"`
				GcodeFile string `json:"gcode_file"`
			} `json:"print"`
		}
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		m := message.Print
		switch {
		case m.Command == "project_file" && m.Sequence == sequence && m.ErrCode == 0 && strings.EqualFold(m.Result, "success"):
			start.to(StartAccepted, nil)
		case m.Command == "project_file" && m.Sequence == sequence:
			reason := cmpOr(m.Reason, m.Result)
			if refusedForAuth(m.Result, m.Reason, m.ErrCode) {
				start.to(StartFailed, &connError{"developer_mode", fmt.Sprintf("the printer declined the start command (%s); turn on LAN Only mode and Developer Mode on the printer", reason)})
			} else {
				start.to(StartFailed, &connError{"refused", fmt.Sprintf("the printer declined the print job: %s (code %d)", reason, m.ErrCode)})
			}
		case (m.State == "PREPARE" || m.State == "RUNNING") && (m.Subtask == name || m.GcodeFile == u.filename):
			start.to(StartAccepted, nil)
		}
	}
	mqttWrite(conn, 0xE0, nil)
	switch start.state {
	case StartAccepted:
		u.outcome = "printing"
		return u.placed() + " and started printing.", nil
	case StartUnconfirmed:
		u.outcome = "unconfirmed"
		return u.placed() + ", but " + p.Name + " hasn't confirmed that the print started. Check its screen before sending again.", nil
	}
	return fail(start.err)
}

type StartState string

const (
	StartSending     StartState = "sending"
	StartAccepted    StartState = "accepted"
	StartFailed      StartState = "failed"
	StartUnconfirmed StartState = "unconfirmed"
)

var startTransitions = map[StartState]map[StartState]bool{
	StartSending:     {StartAccepted: true, StartFailed: true, StartUnconfirmed: true},
	StartAccepted:    {},
	StartFailed:      {},
	StartUnconfirmed: {},
}

const bambuStartWindow = 20 * time.Second

type bambuStart struct {
	state StartState
	err   error
}

func (s *bambuStart) to(next StartState, err error) {
	if !startTransitions[s.state][next] {
		log.Printf("bambu start: illegal transition %s -> %s", s.state, next)
		return
	}
	s.state, s.err = next, err
}

func mqttString(s string) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(s))), s...)
}

func mqttWrite(w io.Writer, header byte, body []byte) error {
	packet := []byte{header}
	n := len(body)
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		packet = append(packet, b)
		if n == 0 {
			break
		}
	}
	packet = append(packet, body...)
	n, err := w.Write(packet)
	if err == nil && n != len(packet) {
		err = io.ErrShortWrite
	}
	return err
}

func mqttRead(r *bufio.Reader) (byte, []byte, error) {
	packet, err := mqttReadPacket(r)
	return packet.header >> 4, packet.body, err
}

const maxMQTTPacketSize = 4 << 20

type mqttPacket struct {
	header byte
	body   []byte
}

func mqttReadPacket(r *bufio.Reader) (mqttPacket, error) {
	header, err := r.ReadByte()
	if err != nil {
		return mqttPacket{}, err
	}
	length, shift := 0, 0
	for i := 0; ; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return mqttPacket{}, err
		}
		length |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
		if i == 3 {
			return mqttPacket{}, errors.New("malformed MQTT length")
		}
	}
	if length > maxMQTTPacketSize {
		return mqttPacket{}, errors.New("MQTT packet too large")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return mqttPacket{}, err
	}
	return mqttPacket{header: header, body: body}, nil
}

type PrinterTest struct {
	OK      bool   `json:"ok"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
}

func testBambu(ctx context.Context, p *PrinterConfig, secret string) PrinterTest {
	failed := func(err error) PrinterTest {
		var c *connError
		if errors.As(err, &c) {
			return PrinterTest{Stage: c.stage, Message: c.msg}
		}
		return PrinterTest{Stage: "unreachable", Message: err.Error()}
	}
	conn, r, err := mqttSession(ctx, p, secret)
	if err != nil {
		return failed(err)
	}
	defer conn.Close()
	sequence := strconv.FormatInt(time.Now().UnixMilli()%1_000_000_000, 10)
	mqttPublish(conn, p, map[string]any{"info": map[string]any{"sequence_id": sequence, "command": "get_version"}})
	mqttPublish(conn, p, map[string]any{"print": map[string]any{"sequence_id": sequence, "command": "gcode_line", "param": "; PancakeSlicer connection test\n"}})
	model, answered := "", false
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	for model == "" || !answered {
		kind, body, err := mqttRead(r)
		if err != nil {
			break
		}
		var message struct {
			Print struct {
				Command  string `json:"command"`
				Sequence string `json:"sequence_id"`
				Result   string `json:"result"`
				Reason   string `json:"reason"`
				ErrCode  int    `json:"err_code"`
			} `json:"print"`
			Info struct {
				Command string `json:"command"`
				Module  []struct {
					Product string `json:"product_name"`
					Project string `json:"project_name"`
				} `json:"module"`
			} `json:"info"`
		}
		if json.Unmarshal(mqttPayload(kind, body), &message) != nil {
			continue
		}
		if m := message.Print; m.Command == "gcode_line" && m.Sequence == sequence {
			if refusedForAuth(m.Result, m.Reason, m.ErrCode) {
				mqttWrite(conn, 0xE0, nil)
				return PrinterTest{Stage: "developer_mode", Message: "the printer turned down a LAN command (" + m.Reason + ")"}
			}
			answered = true
		}
		if message.Info.Command == "get_version" {
			for _, m := range message.Info.Module {
				if model == "" {
					model = bambuModel(m.Product, m.Project)
				}
			}
			if model == "" {
				model = "?"
			}
		}
	}
	mqttWrite(conn, 0xE0, nil)
	if model == "" || model == "?" {
		model = bambuSerialModel(p.Serial)
	}
	host, err := p.resolvedHost(ctx)
	if err != nil {
		return failed(err)
	}
	c, err := ftpsLogin(ctx, host, p.tlsConfig(p.Serial), secret)
	if err != nil {
		result := failed(err)
		if result.Stage == "unreachable" {
			result.Message = "MQTT works, but " + result.Message
		}
		return result
	}
	c.cmd(221, 5*time.Second, "QUIT")
	c.conn.Close()
	message := "Connected"
	if model != "" {
		message += " to " + model
	}
	if !answered {
		message += "; the printer didn't answer the test command, so Developer Mode isn't confirmed yet"
	}
	return PrinterTest{OK: true, Stage: "ok", Message: message, Model: model}
}

func bambuModel(product, code string) string {
	if product != "" {
		return strings.TrimPrefix(product, "Bambu Lab ")
	}
	if model := bambuAutoFlowModels[strings.ToLower(code)]; model != "" {
		return model
	}
	if model := bambuLegacyProjectModels[code]; model != "" {
		return model
	}
	return bambuSerialModels[code]
}

var bambuLegacyProjectModels = map[string]string{
	"N1": "A1 mini", "N2S": "A1", "C11": "P1P", "C12": "P1S", "C13": "X1E", "BL-P001": "X1 Carbon", "BL-P002": "X1",
}

var bambuSerialModels = map[string]string{
	"030": "A1 mini", "039": "A1", "01S": "P1P", "01P": "P1S", "00M": "X1 Carbon", "00W": "X1", "03W": "X1E", "094": "H2D",
}

func bambuSerialModel(serial string) string {
	if len(serial) < 3 {
		return ""
	}
	return bambuSerialModels[strings.ToUpper(serial[:3])]
}

type AMSTray struct {
	Kind       string `json:"kind,omitempty"`
	Extruder   *int   `json:"extruder,omitempty"`
	ID         int    `json:"id"`
	AMS        int    `json:"ams"`
	Slot       int    `json:"slot"`
	Type       string `json:"type"`
	Color      string `json:"color"`
	Name       string `json:"name,omitempty"`
	FilamentID string `json:"filament_id,omitempty"`
	Spool      string `json:"spool,omitempty"`
	Remaining  *int   `json:"remaining,omitempty"`
	Active     bool   `json:"active,omitempty"`
}

type bambuExternalTray struct {
	ID        string `json:"id"`
	Type      string `json:"tray_type"`
	Color     string `json:"tray_color"`
	Idx       string `json:"tray_info_idx"`
	UUID      string `json:"tray_uuid"`
	Remaining *int   `json:"remain"`
}

func spoolID(uuid string) string {
	if strings.Trim(uuid, "0") == "" {
		return ""
	}
	return uuid
}

func canonicalFilamentType(material, filamentID string) string {
	if material == "" {
		return ""
	}
	switch filamentID {
	case "GFS00":
		return "PLA-S"
	case "GFS01":
		return "PA-S"
	default:
		return material
	}
}

func bambuExistenceMask(text *string) *uint64 {
	if text == nil {
		return nil
	}
	value := strings.TrimPrefix(strings.TrimPrefix(*text, "0x"), "0X")
	mask, err := strconv.ParseUint(value, 16, 64)
	if err != nil {
		return nil
	}
	return &mask
}

func absentBambuSlot(mask *uint64, bit int) bool {
	return mask != nil && bit >= 0 && bit < 64 && *mask&(uint64(1)<<bit) == 0
}

type bambuAMSLayout struct {
	unit, firstID, unitBit, firstTrayBit, capacity int
	kind                                           string
	extruder                                       *int
}

func bambuAMSUnitLayout(id, info string) (bambuAMSLayout, bool) {
	unit, err := strconv.Atoi(id)
	if err != nil || unit < 0 || unit > 255 {
		return bambuAMSLayout{}, false
	}
	layout := bambuAMSLayout{unit: unit, firstID: unit * 4, unitBit: unit, firstTrayBit: unit * 4, capacity: 4, kind: "ams"}
	if bits, err := strconv.ParseUint(strings.TrimPrefix(info, "0x"), 16, 64); err == nil {
		layout.kind = map[uint64]string{1: "ams", 2: "ams-lite", 3: "ams-2-pro", 4: "ams-ht", 5: "ams-lite-mixed"}[bits&15]
		if binding := (bits >> 8) & 15; binding < 2 {
			layout.extruder = ptr(1 - int(binding))
		}
	}
	switch {
	case layout.kind == "ams-lite-mixed":
		layout.firstID, layout.unitBit, layout.firstTrayBit = 24, 12, 24
	case layout.kind == "ams-ht" || unit >= 128:
		layout.kind = "ams-ht"
		layout.firstID, layout.unitBit, layout.firstTrayBit, layout.capacity = unit, 4+(unit-128), 16+(unit-128), 1
	}
	return layout, true
}

func (layout bambuAMSLayout) traySlot(id string) (int, bool) {
	slot, err := strconv.Atoi(id)
	return slot, err == nil && slot >= 0 && slot < layout.capacity
}

type bambuReport struct {
	Fun       *string         `json:"fun"`
	Fun2      *string         `json:"fun2"`
	Command   string          `json:"command"`
	Msg       *int            `json:"msg"`
	SpeedMode *int            `json:"spd_lvl"`
	StartTime json.RawMessage `json:"start_time"`
	Lights    []struct {
		Node string `json:"node"`
		Mode string `json:"mode"`
	} `json:"lights_report"`
	HMS []struct {
		Attr    uint32 `json:"attr"`
		Code    uint32 `json:"code"`
		Message string `json:"message"`
	} `json:"hms"`
	State               string          `json:"gcode_state"`
	Percent             *float64        `json:"mc_percent"`
	Remaining           *int            `json:"mc_remaining_time"`
	Layer               *int            `json:"layer_num"`
	Layers              *int            `json:"total_layer_num"`
	Nozzle              *float64        `json:"nozzle_temper"`
	NozzleSet           *float64        `json:"nozzle_target_temper"`
	Bed                 *float64        `json:"bed_temper"`
	BedSet              *float64        `json:"bed_target_temper"`
	Chamber             *float64        `json:"chamber_temper"`
	ChamberSet          *float64        `json:"ctt"`
	ChamberEditable     bool            `json:"support_chamber_temp_edit"`
	ChamberRange        []int           `json:"support_chamber_temp_edit_range"`
	AuxSupported        *bool           `json:"support_aux_fan"`
	ChamberFanSupported *bool           `json:"support_chamber_fan"`
	Device              *bambuDevice    `json:"device"`
	NozzleDiameter      json.RawMessage `json:"nozzle_diameter"`
	NozzleType          string          `json:"nozzle_type"`
	Flag3               *uint32         `json:"flag3"`
	FanGear             *uint32         `json:"fan_gear"`
	PartFan             json.RawMessage `json:"cooling_fan_speed"`
	AuxFan              json.RawMessage `json:"big_fan1_speed"`
	ChamberFan          json.RawMessage `json:"big_fan2_speed"`
	Subtask             string          `json:"subtask_name"`
	Error               int64           `json:"print_error"`
	AMS                 *struct {
		TrayNow       json.RawMessage `json:"tray_now"`
		UnitExistBits *string         `json:"ams_exist_bits"`
		TrayExistBits *string         `json:"tray_exist_bits"`
		AMS           []struct {
			ID          string          `json:"id"`
			Info        string          `json:"info"`
			Humidity    json.RawMessage `json:"humidity"`
			HumidityRaw json.RawMessage `json:"humidity_raw"`
			Tray        []struct {
				Remaining *int   `json:"remain"`
				ID        string `json:"id"`
				Type      string `json:"tray_type"`
				Color     string `json:"tray_color"`
				Name      string `json:"tray_sub_brands"`
				Idx       string `json:"tray_info_idx"`
				UUID      string `json:"tray_uuid"`
			} `json:"tray"`
		} `json:"ams"`
	} `json:"ams"`
	ExternalSlots []bambuExternalTray `json:"vir_slot"`
}

func (b *bambuReport) trays() []AMSTray {
	if b.AMS == nil && b.ExternalSlots == nil {
		return nil
	}
	color := func(c string) string {
		if len(c) >= 6 {
			return "#" + strings.ToUpper(c[:6])
		}
		return ""
	}
	trays := []AMSTray{}
	if b.AMS != nil {
		unitMask, trayMask := bambuExistenceMask(b.AMS.UnitExistBits), bambuExistenceMask(b.AMS.TrayExistBits)
		for _, unit := range b.AMS.AMS {
			layout, ok := bambuAMSUnitLayout(unit.ID, unit.Info)
			if !ok || absentBambuSlot(unitMask, layout.unitBit) {
				continue
			}
			first := len(trays)
			for _, t := range unit.Tray {
				slot, ok := layout.traySlot(t.ID)
				if !ok {
					continue
				}
				tray := AMSTray{ID: layout.firstID + slot, AMS: layout.unit, Slot: slot, Kind: layout.kind, Extruder: layout.extruder}
				if !absentBambuSlot(trayMask, layout.firstTrayBit+slot) {
					tray.Type = canonicalFilamentType(t.Type, t.Idx)
					tray.Color, tray.Name = color(t.Color), t.Name
					tray.FilamentID, tray.Spool = t.Idx, spoolID(t.UUID)
					tray.Remaining, tray.Active = t.Remaining, b.activeTray(layout.unit, slot, false)
				}
				trays = append(trays, tray)
			}
			if len(trays) == first {
				trays = append(trays, AMSTray{ID: layout.firstID, AMS: layout.unit, Kind: layout.kind, Extruder: layout.extruder})
			}
		}
	}
	dual := b.dualNozzle()
	for _, e := range b.ExternalSlots {
		id, err := strconv.Atoi(e.ID)
		if err != nil || !isExternalTray(id) {
			continue
		}
		var extruder *int
		if dual {
			extruder = ptr(id - 254)
		}
		trays = append(trays, AMSTray{ID: id, AMS: id, Slot: 0, Kind: "external", Active: b.activeTray(id, 0, true), Extruder: extruder, Remaining: e.Remaining, Type: canonicalFilamentType(e.Type, e.Idx), Color: color(e.Color), FilamentID: e.Idx, Spool: spoolID(e.UUID)})
	}
	return trays
}

func (b *bambuReport) amsUnits() []AMSUnitStatus {
	if b.AMS == nil {
		return nil
	}
	out := []AMSUnitStatus{}
	mask := bambuExistenceMask(b.AMS.UnitExistBits)
	for _, unit := range b.AMS.AMS {
		layout, ok := bambuAMSUnitLayout(unit.ID, unit.Info)
		if !ok || absentBambuSlot(mask, layout.unitBit) {
			continue
		}
		row := AMSUnitStatus{ID: layout.unit, Kind: layout.kind, Extruder: layout.extruder}
		if v := bambuNumber(unit.HumidityRaw); v != nil && *v >= 0 && *v <= 100 {
			row.HumidityPercent = v
		}
		if v := bambuNumber(unit.Humidity); v != nil && *v >= 1 && *v <= 5 && *v == float64(int(*v)) {
			row.HumidityLevel = ptr(int(*v))
		}
		out = append(out, row)
	}
	return out
}

func (b *bambuReport) status() PrinterStatus {
	st := PrinterStatus{Online: true, Detail: b.State, File: b.Subtask, Layer: b.Layer, Layers: b.Layers, Trays: b.trays(), Nozzles: b.nozzles(), Fans: b.fans(), AMSUnits: b.amsUnits()}
	if b.Fun2 != nil {
		st.InternalStorage = bambuInternalStorageFlag(*b.Fun2)
	} else if b.Fun != nil || b.Command == "push_status" && b.Msg != nil && *b.Msg == 0 {
		st.InternalStorage = ptr(false)
	}
	st.Controls = b.controls()
	if b.Device != nil && b.Device.Extruder != nil {
		for _, extruder := range b.Device.Extruder.Info {
			if extruder.Temp != nil {
				st.NozzleTemps = append(st.NozzleTemps, NozzleTemp{ID: extruder.ID, Actual: float64(*extruder.Temp & 65535), Target: float64(*extruder.Temp >> 16)})
			}
		}
	}
	st.SpeedMode = b.SpeedMode
	if started := bambuNumber(b.StartTime); started != nil && *started > 0 {
		t := time.Unix(int64(*started), 0).UTC()
		st.StartedAt = &t
	}
	for _, light := range b.Lights {
		if light.Node == "chamber_light" {
			st.Light = ptr(light.Mode == "on")
		}
	}
	for _, h := range b.HMS {
		st.Messages = append(st.Messages, PrinterMessage{Code: fmt.Sprintf("%08X-%08X", h.Attr, h.Code), Message: h.Message})
	}
	if b.Error != 0 && b.Error != bambuCancelled {
		st.Messages = append(st.Messages, PrinterMessage{Code: fmt.Sprintf("%08X", b.Error)})
	}
	st.Messages = describeHMS(st.Messages)
	st.State = map[string]string{"IDLE": "idle", "PREPARE": "busy", "SLICING": "busy", "RUNNING": "printing", "PAUSE": "paused", "FINISH": "finished", "FAILED": "failed"}[b.State]
	if st.State == "" {
		st.State = "busy"
	}
	if b.Percent != nil && (st.State == "printing" || st.State == "paused" || st.State == "busy") {
		st.Progress = ptr(*b.Percent / 100)
	}
	if b.Remaining != nil && (st.State == "printing" || st.State == "paused") {
		st.Remaining = ptr(*b.Remaining * 60)
	}
	if b.Nozzle != nil {
		st.Nozzle = &Temp{*b.Nozzle, deref(b.NozzleSet)}
	}
	if b.Bed != nil {
		st.Bed = &Temp{*b.Bed, deref(b.BedSet)}
	}
	if b.Chamber != nil {
		st.Chamber = &Temp{*b.Chamber, deref(b.ChamberSet)}
	}
	switch {
	case b.State == "FAILED" && b.Error == bambuCancelled:
		st.State, st.Detail = "idle", "cancelled"
	case b.Error != 0:
		st.Detail = fmt.Sprintf("%s, error %08X", b.State, b.Error)
	}
	return st
}

const bambuCancelled = 0x0300400C

func pruneAbsentBambuInventory(state map[string]any) {
	ams, _ := state["ams"].(map[string]any)
	units, _ := ams["ams"].([]any)
	if len(units) == 0 {
		return
	}
	unitBits, _ := ams["ams_exist_bits"].(string)
	trayBits, _ := ams["tray_exist_bits"].(string)
	unitMask, trayMask := bambuExistenceMask(&unitBits), bambuExistenceMask(&trayBits)
	if unitMask == nil && trayMask == nil {
		return
	}
	retained := make([]any, 0, len(units))
	changed := false
	for _, raw := range units {
		unit, _ := raw.(map[string]any)
		id, _ := unit["id"].(string)
		info, _ := unit["info"].(string)
		if layout, ok := bambuAMSUnitLayout(id, info); ok {
			if absentBambuSlot(unitMask, layout.unitBit) {
				changed = true
				continue
			}
			if trayMask != nil {
				if cleaned := clearAbsentBambuTrays(unit, layout, trayMask); cleaned != nil {
					raw = cleaned
					changed = true
				}
			}
		}
		retained = append(retained, raw)
	}
	if changed {
		ams = maps.Clone(ams)
		ams["ams"] = retained
		state["ams"] = ams
	}
}

func clearAbsentBambuTrays(unit map[string]any, layout bambuAMSLayout, mask *uint64) map[string]any {
	trays, _ := unit["tray"].([]any)
	var cleaned []any
	for i, raw := range trays {
		tray, ok := raw.(map[string]any)
		if !ok || len(tray) <= 1 {
			continue
		}
		id, _ := tray["id"].(string)
		if slot, ok := layout.traySlot(id); ok && absentBambuSlot(mask, layout.firstTrayBit+slot) {
			if cleaned == nil {
				cleaned = slices.Clone(trays)
			}
			cleaned[i] = map[string]any{"id": id}
		}
	}
	if cleaned == nil {
		return nil
	}
	unit = maps.Clone(unit)
	unit["tray"] = cleaned
	return unit
}

func mergeReport(into, delta map[string]any) {
	msg, _ := delta["msg"].(json.Number)
	if delta["command"] == "push_status" && (msg == "0" || msg == "-0") {
		clear(into)
	}
	mergeReportFields(into, normalizeExternalTray(delta), "")
}

func normalizeExternalTray(delta map[string]any) map[string]any {
	legacy, hasLegacy := delta["vt_tray"].(map[string]any)
	slots, hasSlots := delta["vir_slot"].([]any)
	if !hasLegacy && len(slots) == 0 {
		return delta
	}
	normalized := make([]any, 0, len(slots))
	hasMain := false
	for _, item := range slots {
		if item == nil {
			continue
		}
		if row, ok := item.(map[string]any); ok {
			switch id := row["id"].(type) {
			case nil:
				continue
			case string:
				if id != "254" && id != "255" {
					continue
				}
				hasMain = hasMain || id == "255"
			}
		}
		normalized = append(normalized, item)
	}
	if hasLegacy && !hasMain && (!hasSlots || len(slots) > 0) {
		main := maps.Clone(legacy)
		main["id"] = "255"
		normalized = append(normalized, main)
	}
	if !hasLegacy && len(normalized) == len(slots) {
		return delta
	}
	delta = maps.Clone(delta)
	delete(delta, "vt_tray")
	if len(normalized) > 0 {
		delta["vir_slot"] = normalized
	} else if len(slots) > 0 {
		delete(delta, "vir_slot")
	}
	return delta
}

func bambuKeyedReportRows(path string) bool {
	switch path {
	case "vir_slot", "ams.ams", "ams.ams.tray", "device.extruder.info", "device.nozzle.info":
		return true
	default:
		return false
	}
}

func mergeReportFields(into, delta map[string]any, prefix string) {
	for key, update := range delta {
		path := prefix + key
		switch path {
		case "ams.ams_exist_bits", "ams.tray_exist_bits":
			value, ok := update.(string)
			if !ok || bambuExistenceMask(&value) == nil {
				continue
			}
		}
		if bambuKeyedReportRows(path) {
			if rows, ok := update.([]any); ok {
				if previous, ok := into[key].([]any); ok {
					into[key] = mergeReportRows(previous, rows, path+".")
					continue
				}
			}
		}
		if fields, ok := update.(map[string]any); ok {
			previous, _ := into[key].(map[string]any)
			merged := maps.Clone(previous)
			if merged == nil {
				merged = make(map[string]any, len(fields))
			}
			mergeReportFields(merged, fields, path+".")
			into[key] = merged
			continue
		}
		into[key] = update
	}
}

func mergeReportRows(into, delta []any, prefix string) []any {
	if len(delta) == 0 || len(into) == 0 {
		return delta
	}
	byID := indexReportRows(into)
	if byID == nil || indexReportRows(delta) == nil {
		return delta
	}
	into = slices.Clone(into)
	for _, item := range delta {
		row := item.(map[string]any)
		id := row["id"]
		if index, ok := byID[id]; ok {
			previous := maps.Clone(into[index].(map[string]any))
			mergeReportFields(previous, row, prefix)
			into[index] = previous
		} else {
			into = append(into, row)
		}
	}
	return into
}

func indexReportRows(rows []any) map[any]int {
	byID := make(map[any]int, len(rows))
	for index, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		id := row["id"]
		switch id.(type) {
		case string, json.Number:
		default:
			return nil
		}
		if _, duplicate := byID[id]; duplicate {
			return nil
		}
		byID[id] = index
	}
	return byID
}

func amsBambu(ctx context.Context, p *PrinterConfig, secret string) ([]AMSTray, error) {
	conn, r, err := mqttSession(ctx, p, secret)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := requestBambuStatus(conn, p); err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	state := map[string]any{}
	for {
		kind, body, err := mqttRead(r)
		if err != nil {
			return nil, fmt.Errorf("Couldn't read AMS status from the printer: %w", err)
		}
		report := decodeBambuReport(state, mqttPayload(kind, body))
		if report == nil {
			continue
		}
		if trays := report.trays(); trays != nil {
			return trays, nil
		}
	}
}

func requestBambuStatus(conn io.Writer, p *PrinterConfig) error {
	sequence := strconv.FormatInt(time.Now().UnixMilli()%1_000_000_000, 10)
	return mqttPublish(conn, p, map[string]any{"pushing": map[string]any{"sequence_id": sequence, "command": "pushall"}})
}

func bambuPublish(ctx context.Context, p *PrinterConfig, secret, scope string, command map[string]any) (string, <-chan bambuEcho, func(), error) {
	conn, r, err := mqttSession(ctx, p, secret)
	if err != nil {
		return "", nil, nil, err
	}
	sequence := strconv.FormatInt(time.Now().UnixMilli()%1_000_000_000, 10)
	command["sequence_id"] = sequence
	action, _ := command["command"].(string)
	if err := mqttPublish(conn, p, map[string]any{scope: command}); err != nil {
		conn.Close()
		return "", nil, nil, netError(err, "the printer (MQTT port 8883)")
	}
	echoes := make(chan bambuEcho, 4)
	go func() {
		defer close(echoes)
		for {
			kind, body, err := mqttRead(r)
			if err != nil {
				return
			}
			var message map[string]bambuEcho
			if json.Unmarshal(mqttPayload(kind, body), &message) != nil {
				continue
			}
			if e := message[scope]; e.Command == action && e.Sequence == sequence {
				select {
				case echoes <- e:
				default:
				}
			}
		}
	}()
	return sequence, echoes, func() { mqttWrite(conn, 0xE0, nil); conn.Close() }, nil
}
