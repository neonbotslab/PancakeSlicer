package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var errBambuTunnelUnsupported = errors.New("the printer does not support native internal-storage uploads")

type bambuTunnel struct {
	conn     *tls.Conn
	stop     func() bool
	sequence uint32
	payload  []byte
}

type bambuTunnelReply struct {
	Type     int             `json:"mtype"`
	Command  int             `json:"cmdtype"`
	Sequence uint32          `json:"sequence"`
	Result   *int            `json:"result"`
	Reply    json.RawMessage `json:"reply"`
}

func bambuInternalStorageFlag(flags string) *bool {
	flags = strings.TrimSpace(flags)
	if flags == "" {
		return ptr(false)
	}
	n, err := strconv.ParseUint(flags[len(flags)-1:], 16, 4)
	if err != nil {
		return nil
	}
	return ptr(n&1 != 0)
}

func bambuInternalStorage(ctx context.Context, p *PrinterConfig, secret string) (*bool, error) {
	conn, reader, err := mqttSession(ctx, p, secret)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := mqttPublish(conn, p, map[string]any{"pushing": map[string]any{
		"sequence_id": strconv.FormatInt(time.Now().UnixMilli()%1_000_000_000, 10), "command": "pushall",
	}}); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		kind, body, err := mqttRead(reader)
		if err != nil {
			return nil, fmt.Errorf("Couldn't read the printer's internal-storage capability: %w", err)
		}
		var message struct {
			Print struct {
				Fun     *string `json:"fun"`
				Fun2    *string `json:"fun2"`
				Command string  `json:"command"`
				Msg     *int    `json:"msg"`
			} `json:"print"`
		}
		if json.Unmarshal(mqttPayload(kind, body), &message) != nil {
			continue
		}
		if message.Print.Fun2 != nil {
			if supported := bambuInternalStorageFlag(*message.Print.Fun2); supported != nil {
				return supported, nil
			}
			return nil, errors.New("the printer returned an invalid internal-storage capability")
		}
		if message.Print.Fun != nil || message.Print.Command == "push_status" && message.Print.Msg != nil && *message.Print.Msg == 0 {
			return ptr(false), nil
		}
	}
}

func openBambuTunnel(ctx context.Context, host string, config *tls.Config, secret string) (*bambuTunnel, error) {
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, "6000"))
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errBambuTunnelUnsupported
		}
		return nil, dialError(err, host, 6000)
	}
	conn := tls.Client(raw, config)
	limitSendBuffer(raw)
	t := &bambuTunnel{conn: conn, stop: context.AfterFunc(ctx, func() { raw.Close() })}
	ok := false
	defer func() {
		if !ok {
			t.close()
		}
	}()
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := conn.HandshakeContext(hctx); err != nil {
		return nil, handshakeError(err, 6000)
	}
	if len(secret) > 8 {
		return nil, &connError{"access_code", "the native upload service requires the printer's 8-character LAN access code"}
	}
	var login [16]byte
	copy(login[:8], bambuUser)
	copy(login[8:], secret)
	if err := t.write(0x0101013f, login[:], nil); err != nil {
		return nil, err
	}
	magic, payload, err := t.read()
	if err != nil {
		return nil, err
	}
	if magic == 0x0003013f {
		return nil, errBambuTunnelUnsupported
	}
	if magic != 0x0001013f || len(payload) != 4 {
		return nil, errors.New("the printer returned an invalid native upload login response")
	}
	if binary.LittleEndian.Uint32(payload) != 0 {
		return nil, &connError{"access_code", "the printer rejected the access code (native upload)"}
	}
	setup := []byte(`{"sequence":0,"mtype":12291,"req":{"t_av":0,"mtype":12289,"peer_t":3,"pid":"","ver":""}}`)
	if err := t.write(0x0102013f, setup, nil); err != nil {
		return nil, err
	}
	reply, err := t.reply(12291, 0, 0)
	if err != nil {
		return nil, err
	}
	if *reply.Result != 0 {
		return nil, fmt.Errorf("the printer declined native upload setup (result %d)", *reply.Result)
	}
	ok = true
	return t, nil
}

func (t *bambuTunnel) close() {
	t.stop()
	t.conn.Close()
}

func (t *bambuTunnel) write(magic uint32, payload, data []byte) error {
	var header [16]byte
	size := len(payload)
	if data != nil {
		size += 2 + len(data)
	}
	binary.LittleEndian.PutUint32(header[:4], uint32(size))
	binary.LittleEndian.PutUint32(header[4:8], magic)
	binary.LittleEndian.PutUint32(header[8:12], t.sequence)
	t.sequence++
	t.conn.SetWriteDeadline(time.Now().Add(sendStall))
	if _, err := t.conn.Write(header[:]); err != nil {
		return err
	}
	if _, err := t.conn.Write(payload); err != nil {
		return err
	}
	if data != nil {
		if _, err := t.conn.Write([]byte{'\n', '\n'}); err != nil {
			return err
		}
		if _, err := t.conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func (t *bambuTunnel) read() (uint32, []byte, error) {
	var header [16]byte
	if _, err := io.ReadFull(t.conn, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.LittleEndian.Uint32(header[:4])
	if size > 1<<20 {
		return 0, nil, errors.New("the printer returned an oversized native upload response")
	}
	if cap(t.payload) < int(size) {
		t.payload = make([]byte, size)
	}
	t.payload = t.payload[:size]
	if _, err := io.ReadFull(t.conn, t.payload); err != nil {
		return 0, nil, err
	}
	return binary.LittleEndian.Uint32(header[4:8]), t.payload, nil
}

func (t *bambuTunnel) reply(kind, command int, sequence uint32) (bambuTunnelReply, error) {
	for range 32 {
		magic, payload, err := t.read()
		if err != nil {
			return bambuTunnelReply{}, err
		}
		if magic != 0x0002013f {
			return bambuTunnelReply{}, errors.New("the printer returned an invalid native upload frame")
		}
		var reply bambuTunnelReply
		if err := json.Unmarshal(payload, &reply); err != nil {
			return reply, errors.New("the printer returned invalid native upload JSON")
		}
		if reply.Type != kind || reply.Command != command || reply.Sequence != sequence {
			continue
		}
		if reply.Result == nil {
			return reply, errors.New("the printer omitted the native upload result")
		}
		return reply, nil
	}
	return bambuTunnelReply{}, errors.New("the printer did not acknowledge the native upload request")
}

func (t *bambuTunnel) request(command int, sequence uint32, req any) error {
	payload, err := json.Marshal(struct {
		Type     int    `json:"mtype"`
		Command  int    `json:"cmdtype"`
		Sequence uint32 `json:"sequence"`
		Request  any    `json:"req"`
	}{12289, command, sequence, req})
	if err != nil {
		return err
	}
	return t.write(0x0102013f, payload, nil)
}

func bambuTunnelTLSConfig(ctx context.Context, host string, config *tls.Config) (*tls.Config, error) {
	if config.VerifyConnection == nil {
		return config, nil
	}
	conn, err := dialTLS(ctx, host, 8883, config)
	if err != nil {
		return nil, err
	}
	chain := conn.ConnectionState().PeerCertificates
	conn.Close()
	verify := config.VerifyConnection
	config = config.Clone()
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) > 0 && len(chain) > 0 && bytes.Equal(state.PeerCertificates[0].Raw, chain[0].Raw) {
			state.PeerCertificates = chain
		}
		return verify(state)
	}
	return config, nil
}

func bambuTunnelUpload(ctx context.Context, host string, config *tls.Config, u *upload, md5sum string) (url string, err error) {
	config, err = bambuTunnelTLSConfig(ctx, host, config)
	if err != nil {
		return "", err
	}
	t, err := openBambuTunnel(ctx, host, config, u.secret)
	if err != nil {
		return "", err
	}
	defer t.close()
	defer func() {
		if err != nil && !errors.Is(err, errBambuTunnelUnsupported) {
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			}
			err = fmt.Errorf("Native upload to the printer failed: %w", err)
		}
	}()
	t.conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if err = t.request(7, 1, map[string]any{"peer": "studio", "peer_t": 3, "api_version": 3}); err != nil {
		return "", err
	}
	ability, err := t.reply(12289, 7, 1)
	if err != nil {
		return "", err
	}
	if *ability.Result != 0 {
		return "", &connError{"storage", fmt.Sprintf("the printer declined the storage query (result %d)", *ability.Result)}
	}
	var media struct {
		Storage []string `json:"storage"`
		Upload  []string `json:"upload_storage"`
	}
	if err := json.Unmarshal(ability.Reply, &media); err != nil {
		if err := json.Unmarshal(ability.Reply, &media.Storage); err != nil {
			return "", &connError{"storage", "the printer returned invalid storage capabilities"}
		}
	}
	storage := media.Upload
	if storage == nil {
		storage = media.Storage
	}
	if !slices.Contains(storage, "emmc") {
		return "", errBambuTunnelUnsupported
	}
	if err = t.request(5, 2, map[string]any{"type": "model", "storage": "emmc", "path": u.filename, "total": u.size}); err != nil {
		return "", err
	}
	init, err := t.reply(12289, 5, 2)
	if err != nil {
		return "", err
	}
	if *init.Result != 1 && *init.Result != 19 {
		return "", &connError{"storage", fmt.Sprintf("the printer could not create the file in internal storage (result %d)", *init.Result)}
	}
	var chunk struct {
		Size   int   `json:"chunk_size"`
		Offset int64 `json:"offset"`
	}
	if json.Unmarshal(init.Reply, &chunk) != nil || chunk.Size <= 0 || chunk.Size > 1024 || chunk.Offset < 0 || chunk.Offset > u.size {
		return "", errors.New("the printer returned invalid native upload chunk parameters")
	}
	if _, err = u.file.Seek(chunk.Offset, io.SeekStart); err != nil {
		return "", err
	}
	u.progress.Store(chunk.Offset)
	t.conn.SetDeadline(time.Time{})
	buffer := make([]byte, min(int64(chunk.Size)*1024, u.size-chunk.Offset))
	meta := make([]byte, 0, 256)
	for fragment := int64(0); chunk.Offset < u.size; fragment++ {
		if cause := context.Cause(ctx); cause != nil {
			return "", cause
		}
		n := min(int64(len(buffer)), u.size-chunk.Offset)
		if _, err = io.ReadFull(u.file, buffer[:n]); err != nil {
			return "", err
		}
		meta = append(meta[:0], `{"mtype":12289,"cmdtype":5,"sequence":2,"req":{"frag_id":`...)
		meta = strconv.AppendInt(meta, fragment, 10)
		meta = append(meta, `,"offset":`...)
		meta = strconv.AppendInt(meta, chunk.Offset, 10)
		meta = append(meta, `,"size":`...)
		meta = strconv.AppendInt(meta, n, 10)
		if chunk.Offset+n == u.size {
			meta = append(meta, `,"file_md5":"`...)
			meta = append(meta, strings.ToLower(md5sum)...)
			meta = append(meta, '"')
		}
		meta = append(meta, '}', '}')
		if err = t.write(0x0102013f, meta, buffer[:n]); err != nil {
			return "", err
		}
		chunk.Offset += n
		u.progress.Store(chunk.Offset)
	}
	t.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	for range 32 {
		complete, err := t.reply(12289, 5, 2)
		if err != nil {
			return "", err
		}
		if *complete.Result == 1 {
			continue
		}
		if *complete.Result != 0 {
			return "", &connError{"storage", fmt.Sprintf("the printer could not finish writing to internal storage (result %d)", *complete.Result)}
		}
		return "brtc://emmc/" + u.filename, nil
	}
	return "", errors.New("the printer did not confirm the native upload completed")
}
