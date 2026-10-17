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

// A launch that timed out after systemd-run was invoked must not be
// re-dispatched just because the transient unit is not currently visible.
// The protected same-key identity survives a cold Executor restart, and
// later positive unit/numeric evidence must still recover correctly.
func TestTimedOutLaunchSameOperationIDNeverBlindlyRedispatches(t *testing.T) {
	server, state, workspace := newSharedRuntimeTestServer(t)
	jobs, claims := filepath.Join(state, "jobs"), filepath.Join(state, "jobs", "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	const id = "995"
	req := Request{
		Command: "printf original", OperationID: "no-blind-launch-retry",
		PrincipalID: "owner", PrincipalClass: "direct", Kind: "task", Workspace: workspace,
	}
	paths := jobPathsFor(jobs, id)
	if err := server.prepareJobFiles(paths, req); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(claims, operationClaimName(req.OperationID, req.PrincipalID))
	claim := jobClaim{Version: 1, JobID: id, Fingerprint: server.jobFingerprint(req),
		State: "launching", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Workspace: workspace, PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, Kind: "task"}
	if err := createJobClaim(claimPath, claim); err != nil {
		t.Fatal(err)
	}

	bin, signals := t.TempDir(), t.TempDir()
	count := filepath.Join(signals, "launch-count")
	active := filepath.Join(signals, "unit-active")
	ctl := "#!/bin/sh\ncase \"$1\" in\n" +
		" list-units) if [ -e " + shellQuote(active) + " ]; then printf 'ai-job-995.service loaded active running test\\n'; fi ;;\n" +
		" show) if [ -e " + shellQuote(active) + " ]; then printf 'loaded\\n'; else printf 'not-found\\n'; fi ;;\n" +
		" *) exit 80 ;;\nesac\n"
	run := "#!/bin/sh\nprintf x >> " + shellQuote(count) + "\nexec sleep 60\n"
	for name, script := range map[string]string{"systemctl": ctl, "systemd-run": run} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	timedOut := server.launchPreparedJobWithTimeout(req, paths, id, 60*time.Millisecond)
	if timedOut.OK || timedOut.ErrorCode != "unknown_completion" || timedOut.Status != "unknown" {
		t.Fatalf("timeout was misclassified: %+v", timedOut)
	}
	mustNotRedispatch := func(s *Server) {
		t.Helper()
		retry := s.startJobBounded(req)
		if retry.OK || retry.Status != "unknown" || retry.ErrorCode != "unknown_completion" ||
			!retry.IdempotentReplay || retry.JobID != id {
			t.Fatalf("same-key retry was not fail-closed: %+v", retry)
		}
		raw, err := os.ReadFile(count)
		if err != nil || string(raw) != "x" {
			t.Fatalf("uncertain launch was dispatched more than once: %q, err=%v", raw, err)
		}
		if _, err := os.Stat(paths.command); err != nil {
			t.Fatalf("protected handoff lost during uncertain retry: %v", err)
		}
		stored, found, err := readJobClaim(claimPath)
		if err != nil || !found || stored.State != "launching" || stored.JobID != id {
			t.Fatalf("uncertain claim changed: %+v, found=%t err=%v", stored, found, err)
		}
	}
	mustNotRedispatch(server)
	cold := &Server{cfg: server.cfg, token: server.token, guard: server.guard,
		audit: server.audit, workerUID: server.workerUID, workerGID: server.workerGID}
	mustNotRedispatch(cold)

	conflictReq := req
	conflictReq.Command = "printf changed"
	if conflict := cold.startJobBounded(conflictReq); conflict.OK || conflict.ErrorCode != "idempotency_conflict" {
		t.Fatalf("material same-key change bypassed conflict: %+v", conflict)
	}
	raw, _ := os.ReadFile(count)
	if string(raw) != "x" {
		t.Fatalf("conflicting replay invoked systemd-run again: %q", raw)
	}

	// A subsequently visible exact unit provides positive start evidence,
	// without requiring or permitting a second systemd-run invocation.
	if err := os.WriteFile(active, nil, 0600); err != nil {
		t.Fatal(err)
	}
	recovered := cold.startJobBounded(req)
	if !recovered.OK || recovered.Status != "accepted" || recovered.JobID != id || !recovered.IdempotentReplay {
		t.Fatalf("positive unit evidence did not recover launch: %+v", recovered)
	}
	stored, found, err := readJobClaim(claimPath)
	if err != nil || !found || stored.State != "started" {
		t.Fatalf("confirmed started claim was not persisted: %+v err=%v", stored, err)
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	if err := writeJobStatus(paths.status, 17); err != nil {
		t.Fatal(err)
	}
	terminal := cold.startJobBounded(req)
	if !terminal.OK || terminal.Status != "completed" || terminal.ExitCode != 17 || !terminal.IdempotentReplay {
		t.Fatalf("genuine numeric terminal recovery lost: %+v", terminal)
	}
	raw, _ = os.ReadFile(count)
	if string(raw) != "x" {
		t.Fatalf("numeric recovery re-dispatched prior launch: %q", raw)
	}
}

// Expiry can retire demonstrably unsent "claimed" records, but a
// "launching" record may have reached PID1 even if the unit is gone.
func TestStaleUncertainLaunchRetainsProtectedClaimAndHandoff(t *testing.T) {
	server, jobs, claims := verifiedStopFixture(t)
	const id = "996"
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.command, []byte("printf maybe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{paths.status, paths.started} {
		if err := os.WriteFile(p, nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	claimPath := filepath.Join(claims, operationClaimName("very-old-uncertain-launch"))
	claim := jobClaim{Version: 1, JobID: id, Fingerprint: strings.Repeat("a", 64),
		State: "launching", CreatedAt: time.Now().Add(-2 * executorConnectionTimeout).UTC().Format(time.RFC3339Nano)}
	if err := createJobClaim(claimPath, claim); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := server.cleanupStalePrelaunchClaims(jobs, claims, time.Now()); err != nil {
			t.Fatal(err)
		}
		stored, found, err := readJobClaim(claimPath)
		if err != nil || !found || stored.State != "launching" || stored.JobID != id {
			t.Fatalf("stale uncertain claim falsely retired: %+v found=%t err=%v", stored, found, err)
		}
		raw, err := os.ReadFile(paths.command)
		if err != nil || string(raw) != "printf maybe" {
			t.Fatalf("stale uncertain handoff lost: %q err=%v", raw, err)
		}
	}
	// A missing handoff does not retroactively prove that systemd never
	// accepted the old launch; do not convert the claim to failed.
	if err := os.Remove(paths.command); err != nil {
		t.Fatal(err)
	}
	replay := server.resumeClaimedJob(Request{}, jobs, claimPath, claim, claim.Fingerprint)
	if replay.OK || replay.ErrorCode != "unknown_completion" || replay.JobID != id ||
		replay.Status != "unknown" || !replay.IdempotentReplay {
		t.Fatalf("lost handoff was misclassified as safe non-execution: %+v", replay)
	}
	stored, found, err := readJobClaim(claimPath)
	if err != nil || !found || stored.State != "launching" {
		t.Fatalf("lost handoff changed uncertain claim: %+v found=%t err=%v", stored, found, err)
	}
}

// The inventory helper runs while start_job owns jobsMu. CommandContext
// alone cannot bound Wait when a systemctl descendant keeps the collected
// stdout/stderr pipe open after the direct process exits.
func TestInheritedInventoryPipeIsBoundedAndReleasesJobLock(t *testing.T) {
	server, state, workspace := newSharedRuntimeTestServer(t)
	jobs, claims := filepath.Join(state, "jobs"), filepath.Join(state, "jobs", "claims")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	const targetID = "997"
	verifiedStopJob(t, jobs, claims, targetID, jobStatusUnknown+"\n")
	bin, signals := t.TempDir(), t.TempDir()
	hold := filepath.Join(signals, "hold-output-pipe")
	inventoryCalled := filepath.Join(signals, "inventory-called")
	pipeChildExited := filepath.Join(signals, "pipe-child-exited")
	launchCalled := filepath.Join(signals, "launch-called")
	if err := os.WriteFile(hold, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(hold) })
	// Only wildcard admission inventory inherits the blocked output.
	// Exact-unit probes and stop are still available for recovery.
	ctl := "#!/bin/sh\ncase \"$1\" in\n" +
		" list-units)\n" +
		"   if [ \"$6\" = 'ai-job-*.service' ]; then\n" +
		"     : > " + shellQuote(inventoryCalled) + "\n" +
		"     (while [ -e " + shellQuote(hold) + " ]; do sleep 0.05; done; printf x >> " + shellQuote(pipeChildExited) + ") &\n" +
		"     exit 0\n" +
		"   fi\n" +
		"   exit 0 ;;\n" +
		" stop) exit 0 ;;\n" +
		" show) printf 'not-found\\n'; exit 0 ;;\n" +
		" *) exit 88 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(ctl), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "systemd-run"),
		[]byte("#!/bin/sh\n: > "+shellQuote(launchCalled)+"\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	started := time.Now()
	resp := server.startJobBounded(Request{Command: "printf no-launch",
		OperationID: "inherited-pipe-admission", PrincipalID: "owner", PrincipalClass: "direct", Workspace: workspace})
	elapsed := time.Since(started)
	if resp.OK || resp.ErrorCode != "job_state_unavailable" || resp.JobID != "" {
		t.Fatalf("inventory pipe ambiguity was accepted as safe capacity: %+v", resp)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("inherited pipe blocked jobsMu for %s", elapsed)
	}
	if _, err := os.Stat(inventoryCalled); err != nil {
		t.Fatalf("inventory fixture was not exercised: %v", err)
	}
	if _, err := os.Stat(launchCalled); !os.IsNotExist(err) {
		t.Fatalf("capacity uncertainty launched systemd-run: %v", err)
	}

	// A rejected start has released jobsMu, so another Job can be stopped
	// without an Executor restart or false stopped proof.
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	waitForJobLivenessMarker(t, pipeChildExited)
	// Wait for the fixture child to exit, not just signal its final write.
	time.Sleep(150 * time.Millisecond)
	if err := os.Remove(pipeChildExited); err != nil {
		t.Fatal(err)
	}
	assertSerializedStopExit(t, server.jobStop(Request{JobID: targetID}), -1)
	if restored := server.jobStatus(Request{JobID: targetID}); !restored.OK ||
		restored.Status != "stopped" || restored.ExitCode != -1 {
		t.Fatalf("executor failed to recover Job observation: %+v", restored)
	}

	// Even an earlier caller deadline is honored while inherited output
	// remains open; it must not be replaced with a fresh 4-second budget.
	if err := os.WriteFile(hold, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started = time.Now()
	_, err := activeAgentUnitIDsContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inventory ignored parent deadline: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("inventory canceled by caller still hung on a pipe: %s", time.Since(started))
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	waitForJobLivenessMarker(t, pipeChildExited)
	time.Sleep(150 * time.Millisecond)
}
