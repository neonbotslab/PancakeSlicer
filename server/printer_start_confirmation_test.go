package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPUploadStartOutcome(t *testing.T) {
	for _, tt := range []struct {
		name    string
		start   bool
		status  int
		outcome string
	}{
		{"start accepted", true, http.StatusCreated, "unconfirmed"},
		{"upload only", false, http.StatusCreated, "uploaded"},
		{"start rejected", true, http.StatusConflict, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			printer := &PrinterConfig{Kind: "octoprint", Name: "Fixture"}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			mux.HandleFunc("POST /api/files/local", func(w http.ResponseWriter, r *http.Request) {
				if got := r.FormValue("print"); got != strconv.FormatBool(tt.start) {
					t.Errorf("print form field = %q, want %t", got, tt.start)
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer file.Close()
				body, err := io.ReadAll(file)
				if err != nil || header.Filename != "fixture.gcode" || string(body) != "G28\n" {
					t.Errorf("upload = %q %q, error = %v", header.Filename, body, err)
				}
				w.WriteHeader(tt.status)
			})
			upstream := httptest.NewServer(mux)
			defer upstream.Close()
			base, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "job.gcode")
			if err := os.WriteFile(path, []byte("G28\n"), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			u := &upload{printer: printer, filename: "fixture.gcode", file: file, size: 4, start: tt.start, progress: &atomic.Int64{}}
			host := &hostSession{client: upstream.Client(), printer: printer}
			message, err := host.upload(context.Background(), base, u)
			if tt.status == http.StatusConflict {
				if reason, _ := sendFailure(err); err == nil || reason != "busy" || u.outcome != "" {
					t.Fatalf("rejected upload gave outcome %q and error %v (%s)", u.outcome, err, reason)
				}
				return
			}
			outcome, detail := u.result(message)
			if err != nil || outcome != tt.outcome || strings.Contains(detail, "started printing") {
				t.Fatalf("upload gave outcome %q, message %q and error %v", outcome, detail, err)
			}
		})
	}
}

func TestFlashforgeConsoleStartOutcome(t *testing.T) {
	for _, tt := range []struct {
		name, reply, rejection string
	}{
		{"bare acknowledgement", "ok\r\n", ""},
		{"error before acknowledgement", "Error: file missing\r\nok\r\n", "file missing"},
		{"file open failed", "open failed, File: fixture.gcode\r\n", "open failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "fixture-*.gcode")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString("G28\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			local, peer := net.Pipe()
			defer local.Close()
			defer peer.Close()
			if err := peer.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				defer peer.Close()
				r := bufio.NewReader(peer)
				for _, step := range [][2]string{{"~M28 4 0:/user/fixture.gcode\r\n", "ok\r\n"}, {"G28\n", ""}, {"~M29\r\n", "ok\r\n"}, {"~M23 0:/user/fixture.gcode\r\n", tt.reply}} {
					line, err := r.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					if line != step[0] {
						t.Errorf("command = %q, want %q", line, step[0])
					}
					if step[1] != "" {
						if _, err := io.WriteString(peer, step[1]); err != nil {
							done <- err
							return
						}
					}
				}
				done <- nil
			}()
			u := &upload{printer: &PrinterConfig{Name: "Fixture"}, filename: "fixture.gcode", file: file, size: 4,
				start: true, progress: &atomic.Int64{}, starting: func() {}}
			c := &flashforgeConsole{conn: local, r: bufio.NewReader(local)}
			message, err := c.upload(u, u.filename)
			if fakeErr := <-done; fakeErr != nil {
				t.Fatal(fakeErr)
			}
			if tt.rejection == "" {
				if outcome, detail := u.result(message); err != nil || outcome != "unconfirmed" || strings.Contains(detail, "started printing") {
					t.Fatalf("bare ok gave outcome %q, message %q and error %v", outcome, detail, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.rejection) || u.outcome != "" {
				t.Fatalf("rejection gave outcome %q and error %v", u.outcome, err)
			}
		})
	}
}

func TestHTTPPrinterCancellationStatus(t *testing.T) {
	for _, tt := range []struct {
		kind, state string
		replies     map[string]string
	}{
		{"octoprint", "busy", map[string]string{"/api/job": `{"state":"Cancelling","job":{"file":{"name":"fixture.gcode"}}}`}},
		{"prusalink", "idle", map[string]string{"/api/v1/status": `{"printer":{"state":"STOPPED"},"job":{"id":1}}`, "/api/v1/job": `{"file":{"display_name":"fixture.gcode"}}`}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reply, ok := tt.replies[r.URL.Path]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				io.WriteString(w, reply)
			}))
			defer upstream.Close()
			base, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			host := &hostSession{client: upstream.Client(), printer: &PrinterConfig{Kind: tt.kind}}
			st, err := statusHTTP(context.Background(), host, base)
			if err != nil || st.File != "fixture.gcode" || st.State != tt.state || st.jobEnd() != "cancelled" {
				t.Fatalf("cancelled status = %+v, error = %v", st, err)
			}
		})
	}
}

func TestSendSnapshotIncludesElegooFailure(t *testing.T) {
	var report sdcpStatus
	if err := json.Unmarshal([]byte(`{"CurrentStatus":[0],"PrintInfo":{"Status":14,"Filename":"/local/fixture.gcode","TotalLayer":10}}`), &report); err != nil {
		t.Fatal(err)
	}
	failure := elegooStatus(&report)
	if failure.State != "failed" || failure.File != "fixture.gcode" || failure.Layers != nil {
		t.Fatalf("unexpected Elegoo failure status %+v", failure)
	}
	for _, beforeAck := range []bool{true, false} {
		name := map[bool]string{true: "already known at completion", false: "first poll after acknowledgement"}[beforeAck]
		t.Run(name, func(t *testing.T) {
			m := NewSendManager(nil)
			m.history = loadSendHistory(t.TempDir())
			printer := &PrinterConfig{Printer: "elegoo", Kind: "elegoolink"}
			transfer := &Transfer{ID: "send", Job: "job", Printer: printer.Printer, Kind: printer.Kind,
				Filename: "fixture.gcode", Start: true, State: TransferStarting, Started: time.Now().Add(-time.Second), sent: &atomic.Int64{}}
			job := &Job{ID: transfer.Job, send: transfer}
			m.live[printer.Printer] = job
			m.history.record(*transfer)
			s := &Server{sends: m, jobs: &JobManager{jobs: map[string]*Job{job.ID: job}}}
			mon := &monitor{cfg: printer, link: LinkConnecting}
			m.hub.monitors[printer.Printer] = mon
			m.hub.set(mon, PrinterStatus{Online: true, State: "idle"})
			baseline, _ := m.hub.current(printer.Printer)
			if beforeAck {
				m.hub.set(mon, failure)
			}
			readStarted, completed, readDone := make(chan struct{}), make(chan struct{}), make(chan Transfer, 1)
			if beforeAck {
				go func() {
					close(readStarted)
					for {
						for _, tr := range s.snapshot().Transfers {
							if tr.ID == "send" && tr.State == TransferDone && tr.Outcome == "printing" && tr.JobEnd == "" {
								readDone <- tr
								return
							}
						}
						select {
						case <-completed:
							readDone <- Transfer{}
							return
						default:
						}
					}
				}()
				<-readStarted
			}
			u := &upload{start: true, outcome: "printing"}
			m.finishTransfer(job, transfer, u, baseline, "Start acknowledged.", nil, false)
			if beforeAck {
				close(completed)
				if tr := <-readDone; tr.ID != "" {
					t.Fatalf("snapshot reported printing without the known failure %+v", tr)
				}
			} else {
				m.hub.set(mon, failure)
			}
			snapshot := s.snapshot().Transfers
			if len(snapshot) != 1 || snapshot[0].ID != "send" || snapshot[0].State != TransferDone || snapshot[0].Outcome != "printing" || snapshot[0].JobEnd != "failed" {
				t.Fatalf("snapshot after failed print = %+v", snapshot)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/jobs/{id}/send", s.getSend)
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/jobs/job/send", nil))
			var got Transfer
			if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || res.Code != http.StatusOK || got.JobEnd != "failed" {
				t.Fatalf("GET send after failed print = %d %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestSendLatchesStatusBeforeCompletion(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reports []PrinterStatus
		ack     string
		outcome string
		jobEnd  string
	}{
		{"filename-less failure", []PrinterStatus{{State: "printing", File: "fixture.gcode"}, {State: "failed"}}, "printing", "printing", "failed"},
		{"filename-less cancellation followed by idle", []PrinterStatus{{State: "printing", File: "fixture.gcode"}, {State: "idle", Detail: "cancelled"}, {State: "idle"}}, "printing", "printing", "cancelled"},
		{"uncorrelated failure after offline", []PrinterStatus{{State: "failed", File: "fixture.gcode"}}, "printing", "unconfirmed", ""},
		{"printing observed before upload completion", []PrinterStatus{{State: "printing", File: "fixture.gcode"}, {State: "idle"}}, "", "printing", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewSendManager(nil)
			m.history = loadSendHistory(t.TempDir())
			printer := &PrinterConfig{Printer: "target", Kind: "elegoolink"}
			transfer := &Transfer{ID: "send", Job: "job", Printer: printer.Printer, Name: "Fixture", Kind: printer.Kind,
				Filename: "fixture.gcode", Start: true, State: TransferStarting, Started: time.Now().Add(-time.Second), sent: &atomic.Int64{}}
			job := &Job{ID: transfer.Job, send: transfer}
			m.live[printer.Printer] = job
			m.history.record(*transfer)
			s := &Server{sends: m, jobs: &JobManager{jobs: map[string]*Job{job.ID: job}}}
			mon := &monitor{cfg: printer, link: LinkConnecting}
			m.hub.monitors[printer.Printer] = mon
			m.hub.set(mon, PrinterStatus{State: "offline"})
			baseline, _ := m.hub.current(printer.Printer)
			for _, status := range tt.reports {
				status.Online = true
				m.hub.set(mon, status)
				snapshot := s.snapshot().Transfers
				if len(snapshot) != 1 || snapshot[0].State != TransferStarting || snapshot[0].Outcome == "printing" {
					t.Fatalf("transfer published completion early %+v", snapshot)
				}
			}
			u := &upload{start: true, outcome: tt.ack}
			m.finishTransfer(job, transfer, u, baseline, "Uploaded fixture.gcode to Fixture.", nil, false)
			snapshot := s.snapshot().Transfers
			if len(snapshot) != 1 || snapshot[0].State != TransferDone || snapshot[0].Outcome != tt.outcome || snapshot[0].JobEnd != tt.jobEnd {
				t.Fatalf("first completed snapshot = %+v, want outcome %q and job end %q", snapshot, tt.outcome, tt.jobEnd)
			}
		})
	}
}

func TestStartConfirmationInSendResponses(t *testing.T) {
	for _, tt := range []struct {
		name, kind, filename, file, state, detail, outcome, jobEnd                                      string
		wrongPrinter, stale, priorStart, priorPrint, repeatedEnd, failed, uploadOnly, accepted, offline bool
	}{
		{name: "same file and printer", outcome: "printing"},
		{name: "wrong printer", wrongPrinter: true, outcome: "unconfirmed"},
		{name: "wrong file", file: "other.gcode", outcome: "unconfirmed"},
		{name: "stale report", stale: true, outcome: "unconfirmed"},
		{name: "fresh status for prior job", kind: "moonraker", priorStart: true, outcome: "unconfirmed"},
		{name: "same basename in another folder", kind: "moonraker", file: "other/fixture.gcode", outcome: "unconfirmed"},
		{name: "same basename in Duet macros", kind: "duet", file: "0:/macros/fixture.gcode", outcome: "unconfirmed"},
		{name: "Duet simulation", kind: "duet", detail: "simulating", outcome: "unconfirmed"},
		{name: "idle report", state: "idle", outcome: "unconfirmed"},
		{name: "busy report", state: "busy", outcome: "unconfirmed"},
		{name: "failed job report before confirmation", state: "failed", outcome: "unconfirmed", jobEnd: "failed"},
		{name: "failed job report after acknowledgement", kind: "bambu-lan", state: "failed", accepted: true, outcome: "printing", jobEnd: "failed"},
		{name: "ambiguous failure revokes unobserved acknowledgement", state: "failed", accepted: true, offline: true, outcome: "unconfirmed"},
		{name: "cancelled job report after acknowledgement", state: "idle", detail: "cancelled", accepted: true, outcome: "printing", jobEnd: "cancelled"},
		{name: "OctoPrint cancelling report after acknowledgement", state: "busy", detail: "cancelled", accepted: true, outcome: "printing", jobEnd: "cancelled"},
		{name: "failed job report for another file", state: "failed", file: "other.gcode", outcome: "unconfirmed"},
		{name: "stale failed job report", state: "failed", stale: true, outcome: "unconfirmed"},
		{name: "fresh failure status for prior job", state: "failed", priorStart: true, outcome: "unconfirmed"},
		{name: "failure from known earlier print", kind: "moonraker", state: "failed", priorPrint: true, outcome: "unconfirmed"},
		{name: "temperature update for previous failure", state: "failed", repeatedEnd: true, outcome: "unconfirmed"},
		{name: "OctoPrint SD transfer", detail: "Sending file to SD", outcome: "unconfirmed"},
		{name: "OctoPrint starting", detail: "Starting", outcome: "unconfirmed"},
		{name: "failed send", failed: true, outcome: "failed"},
		{name: "upload only", uploadOnly: true, outcome: "uploaded"},
		{name: "Bambu preparation", kind: "bambu-lan", state: "busy", detail: "PREPARE", outcome: "printing"},
		{name: "Flashforge sanitized name", kind: "flashforge", filename: "fixture part.gcode", file: "0:/user/fixture_part.gcode", outcome: "printing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			transfer := &Transfer{ID: "send", Job: "job", Printer: "target", Kind: cmpOr(tt.kind, "octoprint"),
				Filename: cmpOr(tt.filename, "fixture.gcode"), Start: !tt.uploadOnly, State: TransferDone, Outcome: "unconfirmed",
				Started: now.Add(-time.Second), Finished: &now, sent: &atomic.Int64{}}
			if tt.failed {
				transfer.State, transfer.Outcome = TransferFailed, "failed"
			}
			if tt.uploadOnly {
				transfer.Outcome = "uploaded"
			}
			if tt.accepted {
				transfer.Outcome = "printing"
			}
			historyDir := t.TempDir()
			sends := NewSendManager(nil)
			sends.history = loadSendHistory(historyDir)
			sends.history.record(*transfer)
			status := PrinterStatus{Printer: "target", Online: true, State: cmpOr(tt.state, "printing"), Detail: tt.detail,
				File: cmpOr(tt.file, "fixture.gcode"), At: now}
			if tt.wrongPrinter {
				status.Printer = "other"
			}
			if tt.priorStart {
				status.StartedAt = ptr(transfer.Started.Add(-time.Minute))
			}
			previous := PrinterStatus{Printer: status.Printer, Online: true, State: "idle"}
			if tt.offline {
				previous.Online, previous.State = false, "offline"
			}
			if tt.priorPrint {
				previous.State, previous.File = "printing", status.File
				previous.StartedAt = ptr(transfer.Started.Add(-time.Minute))
			}
			if tt.repeatedEnd {
				previous = status
				status.Nozzle = &Temp{Actual: 26}
			}
			if tt.stale {
				status.At = transfer.Started.Add(-time.Second)
				sends.history.observe(previous, status)
			} else {
				mon := &monitor{cfg: &PrinterConfig{Printer: status.Printer}, link: LinkConnecting}
				sends.hub.monitors[status.Printer] = mon
				sends.hub.status[status.Printer] = previous
				sends.hub.set(mon, status)
				if tt.jobEnd != "" {
					status.State, status.Detail = "printing", ""
					sends.hub.set(mon, status)
				}
			}
			job := &Job{ID: transfer.Job, send: transfer}
			s := &Server{sends: sends, jobs: &JobManager{jobs: map[string]*Job{job.ID: job}}}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/jobs/{id}/send", s.getSend)
			mux.HandleFunc("GET /api/printer-sends", s.listSends)
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/jobs/job/send", nil))
			var got Transfer
			if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || res.Code != 200 || got.Outcome != tt.outcome || got.JobEnd != tt.jobEnd {
				t.Fatalf("current send = %d %s, want outcome %q and job end %q", res.Code, res.Body, tt.outcome, tt.jobEnd)
			}
			sends.history = loadSendHistory(historyDir)
			res = httptest.NewRecorder()
			mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/printer-sends", nil))
			var listed struct{ Sends []Transfer }
			if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil || res.Code != 200 || len(listed.Sends) != 1 || listed.Sends[0].Outcome != tt.outcome || listed.Sends[0].JobEnd != tt.jobEnd {
				t.Fatalf("persisted send = %d %s, want outcome %q and job end %q", res.Code, res.Body, tt.outcome, tt.jobEnd)
			}
		})
	}
}
