package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func verifiedStopFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	state := t.TempDir()
	provisionSharedWorkerHome(t, state)
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(jobs, "claims"), 0700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// The fake systemctl is private to the fixture; no real unit is touched.
	script := "#!/bin/sh\ncase \"$1\" in\n stop|list-units) exit 0 ;;\n show) printf 'not-found\\n'; exit 0 ;;\n *) exit 88 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	s := &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
	return s, jobs, filepath.Join(jobs, "claims")
}

func verifiedStopJob(t *testing.T, jobs, claims, id, status string) jobClaim {
	t.Helper()
	paths := jobPathsFor(jobs, id)
	if err := os.WriteFile(paths.status, []byte(status), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.started, []byte("started\n"), 0640); err != nil {
		t.Fatal(err)
	}
	fixtureRuntimeClaim(t, claims, id, "owner", "service", "/srv/ai-workspace/test-owned-project", time.Now().UTC().Format(time.RFC3339Nano))
	claim, _, found, err := findJobClaimForID(claims, id)
	if err != nil || !found {
		t.Fatalf("read root claim: %v, found=%v", err, found)
	}
	return claim
}

func assertSerializedStopExit(t *testing.T, resp Response, code int) {
	t.Helper()
	if !resp.OK || resp.Status != "stopped" || resp.ExitCode != code {
		t.Fatalf("verified stop response %+v, want exit=%d", resp, code)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var serialized struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := json.Unmarshal(b, &serialized); err != nil {
		t.Fatal(err)
	}
	if serialized.Status != "stopped" || serialized.ExitCode == nil || *serialized.ExitCode != code {
		t.Fatalf("actual MCP JSON stop response %s, want exit=%d", b, code)
	}
}

func TestVerifiedJobStopRetainsRecoverableStoppedStatus(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	for _, tc := range []struct {
		name   string
		id     string
		status string
		exit   int
	}{
		{"empty runner exit", "881", "", -1},
		{"prior unknown runner exit", "882", jobStatusUnknown + "\n", -1},
		{"real numeric runner exit", "883", "7\n", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := verifiedStopJob(t, jobs, claims, tc.id, tc.status)
			stop := s.jobStop(Request{JobID: tc.id})
			assertSerializedStopExit(t, stop, tc.exit)
			raw, err := os.ReadFile(jobPathsFor(jobs, tc.id).status)
			if err != nil || string(raw) != tc.status {
				t.Fatalf("runner evidence changed by privileged stop: %q %v", raw, err)
			}
			// A new Server has no in-memory state from the one that verified stop.
			restarted := &Server{cfg: s.cfg, workerUID: uint32(os.Geteuid())}
			recovered := restarted.jobStatus(Request{JobID: tc.id})
			expected := "stopped"
			if tc.exit == 7 {
				expected = "completed"
			}
			if !recovered.OK || recovered.Status != expected || recovered.ExitCode != tc.exit {
				t.Fatalf("recover exact terminal result after restart: %+v", recovered)
			}
			list := restarted.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct", Kind: "service"})
			if !list.OK || list.Truncated {
				t.Fatalf("passive inventory error %+v", list)
			}
			found := false
			for _, entry := range list.RuntimeEntries {
				if entry.ID == tc.id {
					found = true
					if entry.Status != expected {
						t.Fatalf("passive inventory: %+v want %s", entry, expected)
					}
				}
			}
			if !found {
				t.Fatalf("stopped job missing from passive inventory: %+v", list.RuntimeEntries)
			}
			protected, path, found, err := findJobClaimForID(claims, tc.id)
			if err != nil || !found || !protected.VerifiedStopped {
				t.Fatalf("privileged stop not recorded: %+v %v", protected, err)
			}
			replay := restarted.resumeClaimedJob(Request{Kind: "service"}, jobs, path, claim, claim.Fingerprint)
			if !replay.OK || !replay.IdempotentReplay || replay.Status != expected || replay.ExitCode != tc.exit {
				t.Fatalf("idempotent replay lost protected evidence: %+v", replay)
			}
			if err := restarted.cleanupCompletedJobArtifacts(jobs, claims); err != nil {
				t.Fatalf("retention path rejected terminal record: %v", err)
			}
			again := restarted.jobStatus(Request{JobID: tc.id})
			if !again.OK || again.Status != expected || again.ExitCode != tc.exit {
				t.Fatalf("retention corrupted recovered state: %+v", again)
			}
		})
	}
}

// A worker can write its status file; it cannot assert privileged stop proof.
func TestWorkerWritableStoppedStringDoesNotProveVerifiedStop(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	verifiedStopJob(t, jobs, claims, "884", jobStatusStopped+"\n")
	untrusted := s.jobStatus(Request{JobID: "884"})
	if untrusted.OK || untrusted.ErrorCode != "unknown_completion" || untrusted.Status != "unknown" {
		t.Fatalf("worker-forged stopped status accepted: %+v", untrusted)
	}
	inventory := s.runtimeList(Request{PrincipalID: "owner", PrincipalClass: "direct", Kind: "service"})
	if !inventory.OK || len(inventory.RuntimeEntries) != 1 || inventory.RuntimeEntries[0].Status != "unknown" {
		t.Fatalf("worker-forged stopped leaked into inventory: %+v", inventory)
	}
	stop := s.jobStop(Request{JobID: "884"})
	assertSerializedStopExit(t, stop, -1)
	again := (&Server{cfg: s.cfg, workerUID: uint32(os.Geteuid())}).jobStatus(Request{JobID: "884"})
	if !again.OK || again.Status != "stopped" || again.ExitCode != -1 {
		t.Fatalf("genuine root proof not recovered after worker marker: %+v", again)
	}
}

// systemctl stop success by itself is never proof that the unit is inactive.
func TestJobStopDoesNotPersistStoppedIfUnitStillActive(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	verifiedStopJob(t, jobs, claims, "885", "")
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1" in
 stop) exit 0 ;;
 list-units) echo 'ai-job-885.service loaded active running'; exit 0 ;;
 *) exit 88 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	stopped := s.jobStop(Request{JobID: "885"})
	if stopped.OK || stopped.ErrorCode != "unknown_completion" || stopped.Status != "unknown" {
		t.Fatalf("active unit falsely stopped: %+v", stopped)
	}
	c, _, _, err := findJobClaimForID(claims, "885")
	if err != nil || c.VerifiedStopped {
		t.Fatalf("unverified root stop proof persisted: %+v %v", c, err)
	}
}

// No following an untrusted status symlink, even after unit activity verifies.
func TestJobStopRejectsUnsafeStatusSymlink(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	verifiedStopJob(t, jobs, claims, "886", "")
	paths := jobPathsFor(jobs, "886")
	if err := os.Remove(paths.status); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "other-data")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, paths.status); err != nil {
		t.Fatal(err)
	}
	stopped := s.jobStop(Request{JobID: "886"})
	if stopped.OK || stopped.ErrorCode != "unknown_completion" {
		t.Fatalf("unsafe status symlink accepted: %+v", stopped)
	}
	b, err := os.ReadFile(sentinel)
	if err != nil || string(b) != "preserve" {
		t.Fatalf("unrelated target modified: %q %v", b, err)
	}
	claim, _, _, err := findJobClaimForID(claims, "886")
	if err != nil || claim.VerifiedStopped {
		t.Fatalf("unsafe status generated proof: %+v %v", claim, err)
	}
}

func TestLegacyJobStopWithoutStatusReturnsUnavailableExit(t *testing.T) {
	s, _, _ := verifiedStopFixture(t)
	// Older jobs may lack new-baseline recovery records.
	assertSerializedStopExit(t, s.jobStop(Request{JobID: "887"}), -1)
}

func TestConcurrentReconcileStopAndRunnerExitDoNotLoseEvidence(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	verifiedStopJob(t, jobs, claims, "888", "")
	paths := jobPathsFor(jobs, "888")
	entered := make(chan struct{})
	release := make(chan struct{})
	s.jobUnknownBeforeWriteTestHook = func() {
		close(entered)
		<-release
	}
	statusDone := make(chan Response, 1)
	go func() { statusDone <- s.jobStatus(Request{JobID: "888"}) }()
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("reconciliation never reached controlled prewrite window")
	}
	stopDone := make(chan Response, 1)
	go func() { stopDone <- s.jobStop(Request{JobID: "888"}) }()
	select {
	case early := <-stopDone:
		close(release)
		t.Fatalf("stop overtook in-flight protected reconciliation: %+v", early)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	first := <-statusDone
	if first.OK || first.ErrorCode != "unknown_completion" {
		t.Fatalf("no verified stop yet, expected unknown: %+v", first)
	}
	assertSerializedStopExit(t, <-stopDone, -1)
	restarted := &Server{cfg: s.cfg, workerUID: uint32(os.Geteuid())}
	result := restarted.jobStatus(Request{JobID: "888"})
	if !result.OK || result.Status != "stopped" || result.ExitCode != -1 {
		t.Fatalf("concurrent reconciliation lost root stop proof: %+v", result)
	}
	// Runner uses the exact same status-file lock; real numeric exit always
	// replaces a prior unknown marker and becomes the authoritative exit.
	if err := writeJobStatus(paths.status, 19); err != nil {
		t.Fatal(err)
	}
	completed := restarted.jobStatus(Request{JobID: "888"})
	if !completed.OK || completed.Status != "completed" || completed.ExitCode != 19 {
		t.Fatalf("runner numeric completion not preserved: %+v", completed)
	}
}

func TestRunnerNumericExitWaitsForUnknownStatusFileLock(t *testing.T) {
	s, jobs, claims := verifiedStopFixture(t)
	verifiedStopJob(t, jobs, claims, "889", "")
	path := jobPathsFor(jobs, "889").status
	entered := make(chan struct{})
	release := make(chan struct{})
	s.jobUnknownBeforeWriteTestHook = func() { close(entered); <-release }
	statusDone := make(chan Response, 1)
	go func() { statusDone <- s.jobStatus(Request{JobID: "889"}) }()
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("reconciliation prewrite hook not reached")
	}
	runnerDone := make(chan error, 1)
	go func() { runnerDone <- writeJobStatus(path, 23) }()
	select {
	case err := <-runnerDone:
		close(release)
		t.Fatalf("runner writer bypassed unknown-status file lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	<-statusDone
	if err := <-runnerDone; err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(23) {
		t.Fatalf("runner exit overwritten by reconciler: %q %v", raw, err)
	}
	actual := s.jobStatus(Request{JobID: "889"})
	if !actual.OK || actual.ExitCode != 23 || actual.Status != "completed" {
		t.Fatalf("runner exit lost after concurrent reconciliation: %+v", actual)
	}
}

func TestJobStopFailsClosedIfRecordedJobHasNoProtectedClaim(t *testing.T) {
	s, jobs, _ := verifiedStopFixture(t)
	statusPath := jobPathsFor(jobs, "890").status
	if err := os.WriteFile(statusPath, nil, 0640); err != nil {
		t.Fatal(err)
	}
	resp := s.jobStop(Request{JobID: "890"})
	if resp.OK || resp.ErrorCode != "unknown_completion" || resp.Status != "unknown" {
		t.Fatalf("recorded job without root claim advertised verified stop: %+v", resp)
	}
}
