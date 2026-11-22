package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (s *Server) apiPrinterCameras(w http.ResponseWriter, r *http.Request) {
	p := s.sends.printer(r.PathValue("printer"))
	if p == nil {
		fail(w, 404, "printer_doesnt_have_network")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	sources, err := cameraSources(ctx, p)
	if err != nil {
		fail(w, 502, "couldnt_read_camera_settings")
		return
	}
	type cameraView struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := []cameraView{}
	for _, source := range sources {
		out = append(out, cameraView{source.ID, source.Name})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"cameras": out})
}

func (s *Server) apiPrinterCamera(w http.ResponseWriter, r *http.Request) {
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
	id := r.URL.Query().Get("camera")
	if id == "" && len(sources) > 0 {
		id = sources[0].ID
	}
	var source *cameraSource
	for i := range sources {
		if sources[i].ID == id {
			source = &sources[i]
			break
		}
	}
	if source == nil {
		fail(w, 404, "no_camera_set_up")
		return
	}
	snapshot := r.PathValue("image") == "snapshot.jpg"
	contentType := "multipart/x-mixed-replace; boundary=orca-camera"
	if snapshot {
		contentType = "image/jpeg"
	}
	if r.Method == "HEAD" {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	joinCtx, cancelJoin := context.WithTimeout(r.Context(), 10*time.Second)
	frames, leave, err := s.sends.subscribeCameraOutput(joinCtx, p, *source, true)
	cancelJoin()
	if err != nil {
		if errors.Is(err, errCameraConfigChanged) {
			fail(w, 409, "printers_connection_settings_were")
		} else if r.Context().Err() == nil {
			fail(w, 503, "camera_stream_active")
		}
		return
	}
	defer leave()
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	first := true
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			if first {
				fail(w, 504, "didnt_receive_video_frame")
			}
			return
		case frame, ok := <-frames:
			if !ok {
				if first {
					fail(w, 502, "camera_unavailable_check_camera")
				}
				return
			}
			if first {
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Accel-Buffering", "no")
				if snapshot {
					w.Header().Set("Content-Length", fmt.Sprint(len(frame)))
				}
				first = false
			}
			if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if !snapshot {
				if _, err := fmt.Fprintf(w, "--orca-camera\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame)); err != nil {
					return
				}
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if snapshot {
				return
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil || controller.Flush() != nil {
				return
			}
			timer.Reset(30 * time.Second)
		}
	}
}
