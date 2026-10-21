package executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ach1992/ai-server-agent/internal/policy"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserConcurrentSharedIsolatedAndMaintenance(t *testing.T) {
	var a browserSessionAdmission
	acquire := func(id, mode string, limit int, want bool) {
		t.Helper()
		got, err := a.tryMode(id, mode, limit, nil)
		if err != nil || got != want {
			t.Fatalf("acquire %s/%s/%d = %t %v, want %t", id, mode, limit, got, err, want)
		}
	}
	acquire("iso1", "isolated", 2, true)
	acquire("iso2", "isolated", 2, true)
	acquire("iso3", "isolated", 2, false)
	acquire("shared", "shared", 2, false)
	acquire("setup", "maintenance", 2, false)
	if a.release("wrong") || !a.matches("iso1") {
		t.Fatal("foreign release affected isolated lease")
	}
	if !a.transfer("iso1", "stdio-iso1") || a.matches("iso1") {
		t.Fatal("transfer must preserve exact identity")
	}
	if !a.release("stdio-iso1") {
		t.Fatal("isolation lease not retired")
	}
	acquire("shared", "shared", 2, true)
	acquire("shared2", "shared", 2, false)
	acquire("setup", "maintenance", 2, false)
	if !a.release("shared") || !a.release("iso2") {
		t.Fatal("exact leases not released")
	}
	acquire("setup", "maintenance", 2, true)
	acquire("iso4", "isolated", 2, false)
	if !a.release("setup") || a.reserved() {
		t.Fatal("setup reservation leaked")
	}
	acquire("iso-low", "isolated", 1, true)
	acquire("iso-extra", "isolated", 1, false)
	if !a.release("iso-low") {
		t.Fatal("downscaled reservation unreleasable")
	}
}

func TestBrowserScratchExclusiveFirstAdmissionAndNoSymlinkEscape(t *testing.T) {
	s, _, _ := browserAuditFixture(t)
	s.cfg.StateDir = t.TempDir()
	tmp := filepath.Join(s.cfg.StateDir, "runtime", "browser", "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	stale := []string{"asa-profile-AbC123", "asa-trace-XyZ456", "run.Abc123"}
	for _, name := range stale {
		if err := os.Mkdir(filepath.Join(tmp, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	guard := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(guard, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guard, "sentinel"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(guard, filepath.Join(tmp, "asa-profile-ZyxW12")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmp, "shared-profile"), 0700); err != nil {
		t.Fatal(err)
	}
	grab := func(id string) {
		t.Helper()
		if ok, err := s.browserAcquire(id, "isolated"); err != nil || !ok {
			t.Fatalf("acquire %s: %t %v", id, ok, err)
		}
	}
	grab("first")
	for _, name := range stale {
		if _, err := os.Lstat(filepath.Join(tmp, name)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s not reclaimed: %v", name, err)
		}
	}
	for _, name := range []string{"asa-profile-ZyxW12", "shared-profile"} {
		if _, err := os.Lstat(filepath.Join(tmp, name)); err != nil {
			t.Fatalf("unrelated state removed: %s %v", name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(guard, "sentinel")); err != nil || string(b) != "safe" {
		t.Fatalf("escaped scratch root: %s %v", b, err)
	}
	live := filepath.Join(tmp, "asa-profile-Liv123")
	if err := os.Mkdir(live, 0700); err != nil {
		t.Fatal(err)
	}
	grab("second")
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("active peer profile lost on new admission: %v", err)
	}
	if ok, _ := s.browserAcquire("blocked", "isolated"); ok {
		t.Fatal("third Browser bypassed cap")
	}
	s.browserAdmission.release("first")
	grab("third")
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("active peer profile lost on re-admission: %v", err)
	}
	s.browserAdmission.release("second")
	s.browserAdmission.release("third")
	grab("after-close")
	defer s.browserAdmission.release("after-close")
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("orphan not reclaimed after all verified close: %v", err)
	}
}

func TestBrowserOneShotBusyMatchesManagedAdmissionContract(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	s.guard = policy.New(nil)
	if granted, err := s.browserAcquire("active-shared-browser", "shared"); err != nil || !granted {
		t.Fatalf("reserve existing shared Browser: %t %v", granted, err)
	}
	defer s.browserAdmission.release("active-shared-browser")
	req := Request{
		Action: "run", AuditAction: "browser_run", BrowserProfileMode: "shared",
		PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass,
		Workspace: owner.Workspace, Command: "echo SHOULD_NOT_EXECUTE",
	}
	got := s.runContext(context.Background(), req)
	if got.OK || got.ErrorCode != "resource_limit" || got.ErrorClass != "resource" || !got.Retryable || got.Status != "busy" {
		t.Fatalf("one-shot admission did not preserve retryable busy contract: %+v", got)
	}
	if !s.browserAdmission.matches("active-shared-browser") {
		t.Fatal("capacity refusal released active Browser reservation")
	}
}

func TestBrowserExactScratchDoesNotReclaimConcurrentPeer(t *testing.T) {
	s, _, _ := browserAuditFixture(t)
	s.cfg.StateDir = t.TempDir()
	tmp := filepath.Join(s.cfg.StateDir, "runtime", "browser", "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.browserAcquire("live-peer", "isolated"); err != nil || !ok {
		t.Fatalf("reserve live peer: %t %v", ok, err)
	}
	defer s.browserAdmission.release("live-peer")
	owned := "run." + strings.Repeat("a", 32)
	ownedTrace := "asa-trace-" + strings.Repeat("c", 32)
	peer := "asa-profile-" + strings.Repeat("b", 32)
	peerTrace := "asa-trace-" + strings.Repeat("d", 32)
	for _, name := range []string{owned, ownedTrace, peer, peerTrace} {
		if err := os.Mkdir(filepath.Join(tmp, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{owned, ownedTrace} {
		if err := s.browserReclaimNamedScratch(name, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(tmp, name)); !os.IsNotExist(err) {
			t.Fatalf("exact scratch %s not reclaimed: %v", name, err)
		}
	}
	for _, name := range []string{peer, peerTrace} {
		if _, err := os.Stat(filepath.Join(tmp, name)); err != nil {
			t.Fatalf("unrelated live Browser scratch %s removed: %v", name, err)
		}
	}
	for _, name := range []string{"../untrusted", "asa-trace-Aa1234", "asa-trace-" + strings.Repeat("g", 32)} {
		if err := s.browserReclaimNamedScratch(name, false); err == nil {
			t.Fatalf("untrusted scratch identity %s accepted", name)
		}
	}
}

func TestBrowserTwoRealBrokerIsolatedSessions(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	owner.BrowserProfileMode = "isolated"
	first := s.browserSessionAction(ctx, owner)
	if !first.OK {
		t.Fatalf("first isolated: %+v", first)
	}
	defer s.browserSessionClose(browserOwnerControl(owner, first.SessionID, "browser_session_close"))
	other := owner
	other.PrincipalID = "independent-owner"
	second := s.browserSessionAction(ctx, other)
	if !second.OK || first.SessionID == second.SessionID {
		t.Fatalf("second isolated: %+v", second)
	}
	defer s.browserSessionClose(browserOwnerControl(other, second.SessionID, "browser_session_close"))
	snap := s.browserAdmissionResponse()
	if snap.BrowserAdmission.Active != 2 || snap.BrowserAdmission.Limit != 2 || snap.BrowserAdmission.SharedProfileReserved {
		t.Fatalf("isolated aggregate drift: %+v", snap)
	}
	if busy := s.browserSessionAction(ctx, owner); busy.OK || busy.ErrorCode != "resource_limit" || !busy.Retryable {
		t.Fatalf("third Browser capacity bypass: %+v", busy)
	}
	foreign := browserOwnerControl(owner, second.SessionID, "browser_session_close")
	if got := s.browserSessionAction(ctx, foreign); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("cross-principal close: %+v", got)
	}
	// The shell fixture only sends a ready handshake (it does not execute
	// JS Browser flows). Prove independent authenticated session status here;
	// the separate real-Node Browser test owns cross-call flow behavior.
	if got := s.browserSessionAction(ctx, browserOwnerControl(other, second.SessionID, "browser_session_status")); !got.OK || got.Status != "running" {
		t.Fatalf("independent session state: %+v", got)
	}
	if got := s.browserSessionAction(ctx, browserOwnerControl(owner, first.SessionID, "browser_session_close")); !got.OK {
		t.Fatalf("first close: %+v", got)
	}
	shared := owner
	shared.BrowserProfileMode = "shared"
	both := s.browserSessionAction(ctx, shared)
	if !both.OK {
		t.Fatalf("shared + isolated should be admitted: %+v", both)
	}
	defer s.browserSessionClose(browserOwnerControl(shared, both.SessionID, "browser_session_close"))
	runtime := s.cfg.EffectiveRuntime()
	runtime.BrowserConcurrentSlots = 1 // downscale does not kill existing sessions
	s.cfg.Runtime = &runtime
	if got := s.browserSessionAction(ctx, browserOwnerControl(other, second.SessionID, "browser_session_status")); !got.OK {
		t.Fatalf("downscale lost active Browser: %+v", got)
	}
	if busy := s.browserSessionAction(ctx, owner); busy.OK || busy.ErrorCode != "resource_limit" {
		t.Fatalf("downscale bypass: %+v", busy)
	}
	if got := s.browserSessionAction(ctx, browserOwnerControl(other, second.SessionID, "browser_session_close")); !got.OK {
		t.Fatalf("second close: %+v", got)
	}
	if got := s.browserSessionAction(ctx, browserOwnerControl(shared, both.SessionID, "browser_session_close")); !got.OK {
		t.Fatalf("shared close: %+v", got)
	}
	if s.browserAdmission.reserved() {
		t.Fatal("Browser process reservation leaked")
	}
}

func TestBrowserAdmissionStatusOnlyAggregates(t *testing.T) {
	s, _, _ := browserAuditFixture(t)
	if ok, err := s.browserAcquire("private-session-id", "isolated"); err != nil || !ok {
		t.Fatalf("acquire: %t %v", ok, err)
	}
	defer s.browserAdmission.release("private-session-id")
	resp := s.browserAdmissionResponse()
	data, err := json.Marshal(resp)
	if err != nil || strings.Contains(string(data), "private-session-id") || !resp.OK || resp.Status != "reserved" || resp.BrowserAdmission.Active != 1 || resp.BrowserAdmission.SharedProfileReserved {
		t.Fatalf("aggregate Browser disclosure: %s %+v %v", data, resp, err)
	}
}

// If a synchronous Browser command's process group cannot be verified dead,
// neither shared profile nor isolated capacity may be silently re-admitted.
func TestBrowserOneShotUncertainStopKeepsReservation(t *testing.T) {
	hostname, _ := os.Hostname()
	if os.Geteuid() != 0 || os.Getenv("ASA_197_BROWSER_CONCURRENT_APPROVED_HOST") != hostname {
		t.Skip("root-only command/process cleanup fault injection requires approved disposable test host")
	}
	s, owner, _ := browserAuditFixture(t)
	s.guard = policy.New(nil)
	s.runs = newRunLimiter()
	s.browserRunTerminateTestHook = func(int) (bool, error) { return true, errors.New("synthetic PID-group cleanup uncertainty") }
	req := Request{Action: "run", AuditAction: "browser_run", Command: "printf 'BROWSER_ONESHOT_EXIT'", Workspace: owner.Workspace, BrowserProfileMode: "shared", PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass, RequestID: "test-uncertain-one-shot"}
	got := s.runContext(context.Background(), req)
	if got.OK || got.ReasonCode != "unknown_completion" || !s.browserAdmission.reserved() {
		t.Fatalf("unverified Browser cleanup released profile: %+v", got)
	}
	if s.browserAdmissionResponse().BrowserAdmission.SharedProfileReserved != true {
		t.Fatal("uncertain shared one-shot no longer blocks shared profile")
	}
	if again := s.runContext(context.Background(), req); again.OK || again.ErrorCode != "resource_limit" {
		t.Fatalf("overlapping shared profile after failed cleanup: %+v", again)
	}
	// Test-only synthetic uncertainty cannot mean a real live Browser: this
	// isolated fixture removes its reservation explicitly after assertion.
	s.browserAdmission.mu.Lock()
	var holder string
	for id := range s.browserAdmission.holders {
		holder = id
	}
	s.browserAdmission.mu.Unlock()
	if !s.browserAdmission.release(holder) {
		t.Fatal("synthetic reservation was not held")
	}
}
