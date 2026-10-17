package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForJobLivenessMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake systemd operation never reached %s", path)
}

// A hanging status probe previously held jobsMu forever, preventing an
// unrelated verified stop before its own 25s deadline had even started.
func TestStalledSystemctlStatusCannotPermanentlyBlockJobStop(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	const id = "991"
	verifiedStopJob(t, jobs, claims, id, jobStatusUnknown+"\n")
	bin := t.TempDir()
	probeEntered := filepath.Join(t.TempDir(), "probe-entered")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		" list-units)\n" +
		"   if [ ! -e " + shellQuote(probeEntered) + " ]; then\n" +
		"     : > " + shellQuote(probeEntered) + "\n" +
		"     exec sleep 60\n" +
		"   fi\n" +
		"   exit 0 ;;\n" +
		" stop) exit 0 ;;\n" +
		" show) printf 'not-found\\n'; exit 0 ;;\n" +
		" *) exit 88 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	statusDone := make(chan Response, 1)
	go func() { statusDone <- s.jobStatus(Request{JobID: id}) }()
	waitForJobLivenessMarker(t, probeEntered)

	stopStarted := time.Now()
	stopDone := make(chan Response, 1)
	go func() { stopDone <- s.jobStop(Request{JobID: id}) }()
	select {
	case status := <-statusDone:
		if status.OK || status.ErrorCode != "job_status_unavailable" ||
			!strings.Contains(status.Error, "deadline") {
			t.Fatalf("stalled status probe reported a terminal state: %+v", status)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("status blocked indefinitely while systemctl was stalled")
	}
	select {
	case stop := <-stopDone:
		assertSerializedStopExit(t, stop, -1)
	case <-time.After(3 * time.Second):
		t.Fatal("job_stop remained blocked after failed status probe")
	}
	if time.Since(stopStarted) > 8*time.Second {
		t.Fatalf("verified stop was not bounded: %s", time.Since(stopStarted))
	}
	restarted := &Server{cfg: s.cfg, workerUID: s.workerUID}
	result := restarted.jobStatus(Request{JobID: id})
	if !result.OK || result.Status != "stopped" || result.ExitCode != -1 {
		t.Fatalf("protected stop proof lost after timeout and restart: %+v", result)
	}
	// Later natural numeric evidence is still authoritative.
	if err := writeJobStatus(jobPathsFor(jobs, id).status, 19); err != nil {
		t.Fatal(err)
	}
	numeric := restarted.jobStatus(Request{JobID: id})
	if !numeric.OK || numeric.Status != "completed" || numeric.ExitCode != 19 {
		t.Fatalf("numeric runner exit not retained: %+v", numeric)
	}
}

// A concurrent stalled start must return a retryable busy stop, not retain
// jobsMu forever or invent privileged proof before a real stop was attempted.
func TestStalledJobStartCannotBlockStopOrForgeProof(t *testing.T) {
	s, state, _ := newSharedRuntimeTestServer(t)
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	const targetID = "992"
	verifiedStopJob(t, jobs, claims, targetID, jobStatusUnknown+"\n")

	bin, signals := t.TempDir(), t.TempDir()
	gate := filepath.Join(signals, "hold")
	startEntered := filepath.Join(signals, "start-entered")
	stopCalled := filepath.Join(signals, "stop-called")
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctl := "#!/bin/sh\ncase \"$1\" in\n" +
		" list-units) exit 0 ;;\n" +
		" stop) : > " + shellQuote(stopCalled) + "; exit 0 ;;\n" +
		" show) printf 'not-found\\n'; exit 0 ;;\n" +
		" *) exit 88 ;;\nesac\n"
	run := "#!/bin/sh\n: > " + shellQuote(startEntered) + "\n" +
		"while [ -e " + shellQuote(gate) + " ]; do sleep 0.05; done\n" +
		"printf 'accepted\\n'\n"
	for name, script := range map[string]string{"systemctl": ctl, "systemd-run": run} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	startDone := make(chan Response, 1)
	go func() {
		startDone <- s.startJobBounded(Request{Command: "printf harmless", OperationID: "liveness-start", PrincipalID: "owner", PrincipalClass: "direct"})
	}()
	waitForJobLivenessMarker(t, startEntered)
	startedAt := time.Now()
	busy := s.jobStop(Request{JobID: targetID})
	if busy.OK || busy.ErrorCode != "job_state_unavailable" || busy.Status != "busy" ||
		!busy.Retryable || busy.JobID != targetID {
		t.Fatalf("stop did not fail safely on unavailable state lock: %+v", busy)
	}
	if time.Since(startedAt) > jobStateLockTimeout+2*time.Second {
		t.Fatalf("stop lock wait exceeded bounded deadline: %s", time.Since(startedAt))
	}
	if _, err := os.Stat(stopCalled); !os.IsNotExist(err) {
		t.Fatalf("busy response nonetheless executed systemctl stop: %v", err)
	}
	claim, _, found, err := findJobClaimForID(claims, targetID)
	if err != nil || !found || claim.VerifiedStopped {
		t.Fatalf("stop proof forged without stop attempt: %+v found=%v err=%v", claim, found, err)
	}

	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-startDone:
		if !resp.OK || resp.Status != "accepted" {
			t.Fatalf("released startup failed: %+v", resp)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("released systemd-run never returned")
	}
	assertSerializedStopExit(t, s.jobStop(Request{JobID: targetID}), -1)
	claim, _, found, err = findJobClaimForID(claims, targetID)
	if err != nil || !found || !claim.VerifiedStopped {
		t.Fatalf("retry did not persist protected stop: %+v found=%v err=%v", claim, found, err)
	}
}

// The same command deadline applies to launching, where a timed-out reply
// may hide a real side effect. Never delete the protected launching claim.
func TestTimedOutSystemdRunKeepsUnknownLaunchClaim(t *testing.T) {
	s, state, workspace := newSharedRuntimeTestServer(t)
	jobs := filepath.Join(state, "jobs")
	claims := filepath.Join(jobs, "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	const id = "993"
	paths := jobPathsFor(jobs, id)
	for _, p := range []string{paths.command, paths.status, paths.started} {
		if err := os.WriteFile(p, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	claimPath := filepath.Join(claims, "job-"+id+".json")
	claim := jobClaim{Version: 1, JobID: id, Fingerprint: "safe",
		State: "launching", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := createJobClaim(claimPath, claim); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	ctl := "#!/bin/sh\ncase \"$1\" in list-units) exit 0 ;; show) printf 'not-found\\n'; exit 0 ;; *) exit 81 ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(ctl), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\nexec sleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	started := time.Now()
	result := s.launchPreparedJobWithTimeout(Request{Workspace: workspace}, paths, id, 80*time.Millisecond)
	if result.OK || result.ErrorCode != "unknown_completion" || result.Status != "unknown" {
		t.Fatalf("timed-out launch was misclassified: %+v", result)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("timed-out systemd-run did not honor its deadline: %s", time.Since(started))
	}
	saved, found, err := readJobClaim(claimPath)
	if err != nil || !found || saved.State != "launching" {
		t.Fatalf("protected launch claim lost: %+v found=%v err=%v", saved, found, err)
	}
	if _, err := os.Stat(paths.command); err != nil {
		t.Fatalf("launch handoff lost after uncertain timeout: %v", err)
	}
}

// A timeout while verifying stopped-unit inactivity must remain unknown.
// Even a successful systemctl stop exit cannot independently prove stop.
func TestJobStopVerificationTimeoutCannotForgeStoppedClaim(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	const id = "994"
	verifiedStopJob(t, jobs, claims, id, "")
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in stop) exit 0 ;; list-units) exec sleep 60 ;; *) exit 83 ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	started := time.Now()
	result := s.jobStop(Request{JobID: id})
	if result.OK || result.ErrorCode != "unknown_completion" || result.Status != "unknown" ||
		!strings.Contains(result.Error, "deadline") {
		t.Fatalf("unverified stop advertised success: %+v", result)
	}
	if time.Since(started) > 7*time.Second {
		t.Fatalf("post-stop verification was not bounded: %s", time.Since(started))
	}
	claim, _, found, err := findJobClaimForID(claims, id)
	if err != nil || !found || claim.VerifiedStopped {
		t.Fatalf("unverified stop persisted root proof: %+v found=%v err=%v", claim, found, err)
	}
}

// Ensure a caller-supplied cancellation/deadline is propagated and machine-
// readable instead of a fabricated PID1 success or an unbounded subprocess.
func TestBoundedSystemdCommandReturnsDeadline(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexec sleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	started := time.Now()
	_, err := runBoundedJobSystemd(context.Background(), 40*time.Millisecond, "systemctl", "show", "ai-job-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected actionable systemd timeout, got: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("bounded systemd command delayed %s", time.Since(started))
	}
}
