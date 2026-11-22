package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"os/exec"
	"time"
)

const cameraChunkLimit = 32 << 20
const cameraJPEGLimit = 4 << 20
const cameraSnapshotMaxAge = 2 * time.Second

type cameraJPEGDecoder struct {
	input    chan []byte
	cancel   context.CancelFunc
	stopping bool
	done     chan struct{}
}

func (f *cameraFeed) stopJPEGDecoder() {
	if f.decoder != nil {
		f.decoder.stopping = true
		f.decoder.cancel()
	}
}

func (h *cameraHub) publishJPEG(feed *cameraFeed, frame []byte) {
	if len(frame) > cameraJPEGLimit || !bytes.HasPrefix(frame, []byte{255, 216}) || !bytes.HasSuffix(frame, []byte{255, 217}) {
		return
	}
	feed.jpeg, feed.jpegAt = frame, time.Now()
	feed.decoderBackoff = 0
	for out := range feed.jpegViewers {
		select {
		case <-out:
		default:
		}
		out <- frame
	}
}

func (h *cameraHub) startJPEGDecoder(feed *cameraFeed) {
	if feed.stopping || feed.camera.Format != "mp4" || len(feed.jpegViewers) == 0 || feed.decoder != nil || len(feed.init) == 0 || time.Now().Before(feed.decoderRetry) {
		return
	}
	ctx, cancel := context.WithCancel(feed.ctx)
	decoder := &cameraJPEGDecoder{input: make(chan []byte, 2), cancel: cancel, done: make(chan struct{})}
	feed.decoder = decoder
	decoder.input <- feed.init
	if len(feed.fragment) > 0 {
		decoder.input <- feed.fragment
	}
	go func() {
		defer close(decoder.done)
		defer cancel()
		h.decodeJPEG(ctx, decoder.input, func(frame []byte) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if feed.decoder == decoder && !decoder.stopping && !feed.stopping {
				h.publishJPEG(feed, frame)
			}
		})
		h.mu.Lock()
		defer h.mu.Unlock()
		if feed.decoder == decoder {
			feed.decoder = nil
			feed.jpeg = nil
			feed.decoderBackoff = min(max(time.Second, 2*feed.decoderBackoff), 30*time.Second)
			feed.decoderRetry = time.Now().Add(feed.decoderBackoff)
		}
	}()
}

func decodeCameraJPEG(ctx context.Context, input <-chan []byte, emit func([]byte)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "1", "-filter_threads", "1", "-f", "mov", "-i", "pipe:0", "-map", "0:v:0", "-an", "-vf", "fps=5,scale=w='min(1280,iw)':h=-2", "-c:v", "mjpeg", "-q:v", "5", "-threads", "1", "-f", "mpjpeg", "-boundary_tag", "orca-jpeg", "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		defer stdin.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-input:
				if !ok {
					return
				}
				if _, err := stdin.Write(chunk); err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		stdin.Close()
		cmd.Wait()
		<-pumped
	}()
	reader := multipart.NewReader(stdout, "orca-jpeg")
	for {
		part, err := reader.NextPart()
		if err != nil {
			return err
		}
		frame, err := io.ReadAll(io.LimitReader(part, cameraJPEGLimit+1))
		if err != nil || len(frame) > cameraJPEGLimit || !bytes.HasPrefix(frame, []byte{255, 216}) || !bytes.HasSuffix(frame, []byte{255, 217}) {
			return errors.New("invalid camera JPEG")
		}
		emit(frame)
	}
}
