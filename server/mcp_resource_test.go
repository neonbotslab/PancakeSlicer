package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func assertNoMCPPrintSnapshots(t *testing.T, job *Job) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(job.Dir, "tmp", ".mcp-print-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("temporary print files remain: %v", paths)
	}
}

func TestMCPPrintSnapshotLifetime(t *testing.T) {
	fixture := newMCPPrintFixture(t)
	session := fixture.connect(t, nil)
	printer := fixture.readyPrinter(t, session)
	job := fixture.slice(t, session, printer)
	prepared := mcpTestPrepare(t, session, job.ID, printer)
	assertNoMCPPrintSnapshots(t, job)
	fixture.assertUnsent(t, job.ID)

	if err := os.WriteFile(job.OutputPath("gcode"), []byte("G28\nG1 X6 Y5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mcpTestRefusal(t, mcpTestCall(t, session, "confirm_print", prepared.confirm(), nil), "artifact mismatch")
	assertNoMCPPrintSnapshots(t, job)
	fixture.assertUnsent(t, job.ID)
}

func TestMCPPreparationRespectsDiskQuota(t *testing.T) {
	fixture := newMCPPrintFixture(t)
	session := fixture.connect(t, nil)
	printer := fixture.readyPrinter(t, session)
	job := fixture.slice(t, session, printer)
	used, err := directoryBytes(fixture.server.cfg.JobRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.cfg.DiskQuota = used + int64(len(mcpFixtureGCode)) - 1
	result := mcpTestCall(t, session, "prepare_print", mcpPrepareInput{
		JobID: job.ID, Printer: printer, Filename: mcpFixtureFilename, FlowCalibration: "off",
	}, nil)
	mcpTestRefusal(t, result, "disk quota")
	assertNoMCPPrintSnapshots(t, job)
	fixture.assertUnsent(t, job.ID)
}

func TestMCPActiveUploadRetainsSnapshot(t *testing.T) {
	gate := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(gate) })
	defer unblock()
	fixture := newMCPPrintFixtureWithUploadGate(t, gate)
	session := fixture.connect(t, nil)
	printer := fixture.readyPrinter(t, session)
	job := fixture.slice(t, session, printer)
	prepared := mcpTestPrepare(t, session, job.ID, printer)
	used, err := directoryBytes(fixture.server.cfg.JobRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.cfg.DiskQuota = used + int64(len(mcpFixtureGCode))

	var transfer Transfer
	mcpTestCall(t, session, "confirm_print", prepared.confirm(), &transfer)
	select {
	case got := <-fixture.uploads:
		if want := (mcpTestUpload{mcpFixtureFilename, mcpFixtureGCode, "true", mcpFixtureKey}); got != want {
			t.Fatalf("printer received %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mock printer did not receive the upload")
	}
	paths, err := filepath.Glob(filepath.Join(job.Dir, "tmp", ".mcp-print-*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("active upload snapshots = %v, error %v", paths, err)
	}
	if got, err := directoryBytes(fixture.server.cfg.JobRoot); got != fixture.server.cfg.DiskQuota || err != nil {
		t.Fatalf("active upload uses %d bytes, want %d including snapshot, error %v", got, fixture.server.cfg.DiskQuota, err)
	}
	deleteJob := func() (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, fixture.http.URL+"/api/jobs/"+job.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := fixture.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(body)
	}
	if code, body := deleteJob(); code < 400 || !strings.Contains(body, "artifact is currently in use") {
		t.Fatalf("delete job during upload returned HTTP %d, %s", code, body)
	}

	unblock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, current := fixture.transfer(t, job.ID)
		_, statErr := os.Stat(paths[0])
		if code == http.StatusOK && current.ID == transfer.ID && current.State == TransferDone && os.IsNotExist(statErr) {
			break
		}
		if current.State == TransferFailed || time.Now().After(deadline) {
			t.Fatalf("upload did not finish and clean up: HTTP %d, transfer %+v, snapshot error %v", code, current, statErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertNoMCPPrintSnapshots(t, job)
	if got, err := directoryBytes(fixture.server.cfg.JobRoot); got != used || err != nil {
		t.Fatalf("completed upload uses %d bytes, want %d after cleanup, error %v", got, used, err)
	}
	for {
		code, body := deleteJob()
		if code == http.StatusNoContent {
			break
		}
		if !strings.Contains(body, "artifact is currently in use") || time.Now().After(deadline) {
			t.Fatalf("delete job after upload returned HTTP %d, %s", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMCPPrintSnapshotCleanupOnRestart(t *testing.T) {
	fixture := newMCPPrintFixture(t)
	session := fixture.connect(t, nil)
	printer := fixture.readyPrinter(t, session)
	job := fixture.slice(t, session, printer)
	fixture.server.jobs.Shutdown()

	tmp := filepath.Join(job.Dir, "tmp")
	for _, name := range []string{".mcp-print-abandoned", "keep"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(mcpFixtureGCode), 0600); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := NewJobManager(fixture.server.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jobs.Shutdown)
	if restored := jobs.Get(job.ID); restored == nil || restored.State() != StateDone {
		t.Fatalf("completed slice did not survive restart: %v", restored)
	}
	assertNoMCPPrintSnapshots(t, job)
	for _, path := range []string{filepath.Join(tmp, "keep"), job.OutputPath("gcode")} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != mcpFixtureGCode {
			t.Fatalf("restart changed %s: %q, error %v", path, got, err)
		}
	}
}
