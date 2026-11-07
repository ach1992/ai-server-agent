package executor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func reviewSyncServer(t *testing.T) *Server {
	t.Helper()
	s := newSyncOutputTestServer(t)
	settings := config.DefaultRuntimeSettings()
	settings.SyncOutputSpoolMiB = 8
	s.cfg.Runtime = &settings
	return s
}

func syncSpoolMarkerIDs(t *testing.T, jobsDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), syncSpoolMarkerSuffix) {
			ids = append(ids, strings.TrimSuffix(e.Name(), syncSpoolMarkerSuffix))
		}
	}
	return ids
}

func TestSyncOutputInterruptedExecutorProcessReclaimedAfterRestart(t *testing.T) {
	// The child writes a real, fallocated transcript then terminates without
	// Finish. The kernel releases its marker flock, not the orphan files.
	if path := os.Getenv("ASA230_SYNC_CRASH_DIR"); path != "" {
		settings := config.DefaultRuntimeSettings()
		settings.SyncOutputSpoolMiB = 8
		s := &Server{cfg: config.Config{StateDir: path, Runtime: &settings}, workerUID: uint32(os.Geteuid())}
		c := newSyncOutputCapture(s)
		if _, err := c.Write(bytes.Repeat([]byte("x"), maxSyncOutputBytes+8192)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(81)
		}
		os.Exit(73)
	}
	dir := t.TempDir()
	jobsDir := filepath.Join(dir, "jobs")
	if err := os.Mkdir(jobsDir, 0711); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSyncOutputInterruptedExecutorProcessReclaimedAfterRestart$")
	cmd.Env = append(os.Environ(), "ASA230_SYNC_CRASH_DIR="+dir)
	combined, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 73 {
		t.Fatalf("disposable Executor interruption did not occur: err=%v out=%s", err, combined)
	}
	ids := syncSpoolMarkerIDs(t, jobsDir)
	if len(ids) != 1 {
		t.Fatalf("crashed transcript marker missing: %v", ids)
	}
	s := reviewSyncServer(t)
	// Reuse the child's exact state to model root Executor restart.
	s.cfg.StateDir = dir
	if err := s.cleanupInterruptedSyncSpools(jobsDir); err != nil {
		t.Fatal(err)
	}
	if got := syncSpoolMarkerIDs(t, jobsDir); len(got) != 0 {
		t.Fatalf("stale marker remained after restart: %v", got)
	}
	for _, ext := range []string{".log", ".status", syncSpoolMarkerSuffix} {
		if _, err := os.Lstat(filepath.Join(jobsDir, ids[0]+ext)); !os.IsNotExist(err) {
			t.Fatalf("crash-left transcript artifact retained %s: %v", ext, err)
		}
	}
	if output := s.jobOutputBounded(Request{JobID: ids[0]}); output.OK || output.ErrorCode != "job_not_found" {
		t.Fatalf("partial output advertised as complete after restart: %+v", output)
	}
}

func TestSyncOutputCrashDuringStatusWriteCleansTornStatus(t *testing.T) {
	for _, badStatus := range []string{"non-numeric", strings.Repeat("x", 65), "unknown"} {
		t.Run(fmt.Sprintf("bytes_%d", len(badStatus)), func(t *testing.T) {
			s := reviewSyncServer(t)
			c := newSyncOutputCapture(s)
			if _, err := c.Write(bytes.Repeat([]byte("t"), maxSyncOutputBytes+129)); err != nil {
				t.Fatal(err)
			}
			paths := jobPathsFor(filepath.Join(s.cfg.StateDir, "jobs"), c.jobID)
			// The process disappears before Finish's numeric status fsync;
			// release only test-owned FDs and leave root-private artifacts.
			if err := c.file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.marker.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.status, []byte(badStatus), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.cleanupInterruptedSyncSpools(filepath.Dir(paths.status)); err != nil {
				t.Fatalf("torn status blocked startup reconciliation: %v", err)
			}
			for _, path := range []string{paths.status, paths.log, paths.status[:len(paths.status)-len(".status")] + syncSpoolMarkerSuffix} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("stale partial artifact remains: %s (%v)", path, err)
				}
			}
		})
	}
}

func TestSyncOutputLiveWriterAndUnrelatedJobProtectedFromCleanup(t *testing.T) {
	s := reviewSyncServer(t)
	jobsDir := filepath.Join(s.cfg.StateDir, "jobs")
	c := newSyncOutputCapture(s)
	raw := bytes.Repeat([]byte("Q"), maxSyncOutputBytes+512)
	if _, err := c.Write(raw); err != nil {
		t.Fatal(err)
	}
	if got := syncSpoolMarkerIDs(t, jobsDir); len(got) != 1 {
		t.Fatalf("active marker missing: %v", got)
	}
	other := jobPathsFor(jobsDir, "987654321")
	if err := os.WriteFile(other.log, []byte("unrelated output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other.status, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.cleanupInterruptedSyncSpools(jobsDir); err != nil {
		t.Fatal(err)
	}
	if got := syncSpoolMarkerIDs(t, jobsDir); len(got) != 1 {
		t.Fatalf("active writer retired: %v", got)
	}
	if part := s.jobOutputBounded(Request{JobID: "987654321"}); !part.OK || part.Output != "unrelated output" {
		t.Fatalf("unrelated completed Job damaged: %+v", part)
	}
	var resp Response
	applyOutputResult(&resp, c.preview.Result())
	if err := c.Finish(&resp, 32768); err != nil {
		t.Fatal(err)
	}
	if resp.JobID == "" {
		t.Fatal("complete transcript not advertised")
	}
	if markers := syncSpoolMarkerIDs(t, jobsDir); len(markers) != 0 {
		t.Fatalf("committed marker leaked: %v", markers)
	}
	if part := s.jobOutputBounded(Request{JobID: resp.JobID, Offset: 0, Limit: 4096}); !part.OK || part.Output != string(raw[:4096]) {
		t.Fatalf("committed transcript unreadable: %+v", part)
	}
	if got, err := os.ReadFile(other.status); err != nil || string(got) != "0\n" {
		t.Fatalf("unrelated status changed: %q %v", got, err)
	}
}

func TestSyncOutputCommitBeforeMarkerCleanupRetainsCompleteJob(t *testing.T) {
	s := reviewSyncServer(t)
	c := newSyncOutputCapture(s)
	payload := bytes.Repeat([]byte("C"), maxSyncOutputBytes+256)
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after numeric commit but before removing the marker.
	if err := c.file.Truncate(c.seen); err != nil {
		t.Fatal(err)
	}
	if err := c.file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := c.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeJobStatus(c.statusPath, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.marker.Close(); err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Join(s.cfg.StateDir, "jobs")
	if err := s.cleanupInterruptedSyncSpools(jobsDir); err != nil {
		t.Fatal(err)
	}
	if got := syncSpoolMarkerIDs(t, jobsDir); len(got) != 0 {
		t.Fatalf("committed stale marker not removed: %v", got)
	}
	part := s.jobOutputBounded(Request{JobID: c.jobID, Limit: 4096})
	if !part.OK || part.Output != string(payload[:4096]) {
		t.Fatalf("durably complete transcript discarded: %+v", part)
	}
}

func TestSyncOutputConcurrentDiskReservationsAndRelease(t *testing.T) {
	s := reviewSyncServer(t)
	dir := filepath.Join(s.cfg.StateDir, "jobs")
	base := jobAuditSafetyReserveAtStartup(s.cfg)
	const room = int64(12 << 20)
	// A controlled fake free-space envelope counts real fallocated file
	// sizes. Two 8 MiB spools must not each pass a stale 12 MiB budget.
	s.syncSpoolReserveCheckTestHook = func(_ string, need int64) (bool, int64, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false, 0, err
		}
		free := base + room
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				return false, 0, err
			}
			free -= fi.Size()
		}
		return free >= need, free, nil
	}
	const count = 2
	var wg sync.WaitGroup
	captures := make([]*syncOutputCapture, count)
	errs := make([]error, count)
	begin := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-begin
			c := newSyncOutputCapture(s)
			_, err := c.Write(bytes.Repeat([]byte("A"), maxSyncOutputBytes+16))
			captures[index], errs[index] = c, err
		}(i)
	}
	close(begin)
	wg.Wait()
	var success, denied int
	var admitted *syncOutputCapture
	for i := 0; i < count; i++ {
		if errs[i] == nil {
			success++
			admitted = captures[i]
		} else if strings.Contains(errs[i].Error(), "output spool disk capacity is insufficient") {
			denied++
		} else {
			t.Fatalf("unexpected disk admission error: %v", errs[i])
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("capacity double-booked: success=%d denied=%d", success, denied)
	}
	if got := syncSpoolMarkerIDs(t, dir); len(got) != 1 {
		t.Fatalf("denied spool leaked marker: %v", got)
	}
	// Completed transcript shrinks to actual bytes, making residual
	// capacity available without global command serialization.
	var resp Response
	applyOutputResult(&resp, admitted.preview.Result())
	if err := admitted.Finish(&resp, 32768); err != nil {
		t.Fatal(err)
	}
	next := newSyncOutputCapture(s)
	if _, err := next.Write(bytes.Repeat([]byte("B"), maxSyncOutputBytes+32)); err != nil {
		t.Fatalf("capacity was not released: %v", err)
	}
	var nextResp Response
	applyOutputResult(&nextResp, next.preview.Result())
	if err := next.Finish(&nextResp, 32768); err != nil {
		t.Fatal(err)
	}
	// The existing persistent-Job admission remains independently usable
	// when its configured safety reserve still fits the filesystem.
	// This fixture has no systemd Job inventory; exercise the shared disk
	// reserve calculation itself without pretending an installed service.
	s.syncSpoolMu.Lock()
	ok, available, checkErr := s.syncSpoolReserveCheckTestHook(dir, jobStateSafetyReserveBytesForLimits(6, 3))
	s.syncSpoolMu.Unlock()
	if checkErr != nil || !ok {
		t.Fatalf("persistent Job safety reserve not preserved: %d %v", available, checkErr)
	}
}

func TestSyncOutputExceededCapacityDeletesPreallocatedArtifacts(t *testing.T) {
	s := reviewSyncServer(t)
	c := newSyncOutputCapture(s)
	if _, err := c.Write(bytes.Repeat([]byte("x"), 2<<20)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.cfg.StateDir, "jobs", c.jobID+".log")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 8<<20 {
		t.Fatalf("fallocate did not reserve configured transcript space: %d", fi.Size())
	}
	if _, err := c.Write(bytes.Repeat([]byte("y"), 7<<20)); !errors.Is(err, errSyncOutputLimit) {
		t.Fatalf("missing output limit: %v", err)
	}
	var resp Response
	applyOutputResult(&resp, c.preview.Result())
	if err := c.Finish(&resp, 32768); !errors.Is(err, errSyncOutputLimit) || resp.JobID != "" {
		t.Fatalf("truncated result advertised as complete: %+v %v", resp, err)
	}
	if got := syncSpoolMarkerIDs(t, filepath.Join(s.cfg.StateDir, "jobs")); len(got) != 0 {
		t.Fatalf("failure left marker: %v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fallocate reservation not released: %v", err)
	}
	// A failed command cannot monopolize the capacity of later commands.
	next := newSyncOutputCapture(s)
	if _, err := next.Write(bytes.Repeat([]byte("z"), maxSyncOutputBytes+100)); err != nil {
		t.Fatalf("reservation remained after failure: %v", err)
	}
	var ok Response
	applyOutputResult(&ok, next.preview.Result())
	if err := next.Finish(&ok, 32768); err != nil || ok.JobID == "" {
		t.Fatalf("next command not recoverable: %+v %v", ok, err)
	}
}

func TestSyncSpoolStatusUsesNumericExitEvidence(t *testing.T) {
	s := reviewSyncServer(t)
	c := newSyncOutputCapture(s)
	if _, err := c.Write(bytes.Repeat([]byte("w"), maxSyncOutputBytes+10)); err != nil {
		t.Fatal(err)
	}
	var resp Response
	resp.ExitCode = 23
	applyOutputResult(&resp, c.preview.Result())
	if err := c.Finish(&resp, 32768); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "jobs", resp.JobID+".status"))
	if err != nil || string(b) != "23\n" {
		t.Fatalf("invalid status commit %q %v", b, err)
	}
	if markers := syncSpoolMarkerIDs(t, filepath.Join(s.cfg.StateDir, "jobs")); len(markers) != 0 {
		t.Fatalf("completed numeric status still has active marker: %v", markers)
	}
}
