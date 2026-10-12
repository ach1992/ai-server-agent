package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

// Seed a broker-owned, completed adapter session. These fixtures never send
// signals to real host processes: the original pidfd/cgroup lifecycle is
// exercised separately by the existing real-Delve and privileged tests.
func completedDAPSession(t *testing.T, stage string, cleanupErr error) (*Server, Request, net.Conn) {
	t.Helper()
	s, owner, workspace := testStdioBroker(t)
	s.audit = audit.New(t.TempDir() + "/dap-audit.jsonl")
	agentSide, adapterSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close(); _ = adapterSide.Close() })
	owner.Action = "debug_status"
	owner.SessionID = "stdio_dap_completed_recovery"
	entry := &stdioSession{
		id: owner.SessionID, kind: "dap", ownerID: owner.PrincipalID,
		ownerClass: owner.PrincipalClass, workspace: workspace,
		exited: true, exitCode: 137, cleanupErr: cleanupErr,
		dap: &dapState{conn: agentSide, adapter: "go/delve", workspace: workspace, stage: stage},
	}
	s.sessions.mu.Lock()
	s.sessions.sessions[entry.id] = entry
	s.sessions.mu.Unlock()
	return s, owner, adapterSide
}

func statusBody(t *testing.T, resp Response) map[string]any {
	t.Helper()
	if !resp.OK {
		t.Fatalf("debug status failed: %+v", resp)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(resp.Output), &body); err != nil {
		t.Fatalf("invalid structured status: %v, %q", err, resp.Output)
	}
	return body
}

func TestDAPCleanAdapterCrashStatusAndRecovery(t *testing.T) {
	s, owner, peer := completedDAPSession(t, "stopped", nil)
	// Deliberately invalid queued transport bytes: after the broker has
	// completed exit/cleanup they must NOT be read and poison the outcome.
	entry, err := s.sessions.get(owner, owner.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	entry.dap.pending = []byte("Content-Length: nonsense\r\n\r\n")
	entry.dap.acceptEvent(dapPacket{Type: "event", Event: "stopped", Body: json.RawMessage(`{"reason":"exception"}`)})
	owner.Action = "debug_status"
	status := s.debugAction(context.Background(), owner)
	body := statusBody(t, status)
	if body["state"] != "failed" || body["adapter_exited"] != true || body["cleanup_verified"] != true || body["adapter_exit_code"] != float64(137) {
		t.Fatalf("a cleanly reaped failed adapter lost its truthful cleanup evidence: %v", body)
	}
	if status.Running == nil || *status.Running || status.ExitCode != 137 {
		t.Fatalf("adapter liveness/exit identity incorrect: %+v", status)
	}
	events, ok := body["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("queued crash evidence disappeared: %v", body)
	}
	if entry.dap.uncertain || len(entry.dap.pending) == 0 {
		t.Fatal("dead adapter transport was read and falsely poisoned before clean status")
	}

	owner.Action, owner.DebugMethod = "debug_action", "continue"
	owner.DebugThreadID = 1
	rejected := s.debugAction(context.Background(), owner)
	if rejected.OK || rejected.ErrorCode != "debug_session_ended" || !strings.Contains(rejected.Error, "debug_stop") {
		t.Fatalf("late debugger command not refused with safe recovery advice: %+v", rejected)
	}
	// Sending even one post-exit DAP request is unacceptable: there is no
	// target left to control, and session reuse must be explicit.
	_ = peer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	var unexpected [1]byte
	if n, err := peer.Read(unexpected[:]); n != 0 || !isReadTimeout(err) {
		t.Fatalf("dead adapter received unexpected protocol traffic: n=%d err=%v", n, err)
	}
	owner.Action = "debug_stop"
	stopped := s.debugAction(context.Background(), owner)
	if !stopped.OK || stopped.Status != "cleanup_confirmed_adapter_already_exited" {
		t.Fatalf("proven-clean crashed debugger could not be retired: %+v", stopped)
	}
	owner.Action = "debug_status"
	if again := s.debugAction(context.Background(), owner); again.OK || again.ErrorCode != "debug_session_not_found" {
		t.Fatalf("retired debugger remained addressable: %+v", again)
	}
	// Recovery really frees the shared session broker for a new worker
	// adapter, rather than keeping a phantom DAP reservation.
	owner.SessionID = ""
	id, err := s.workerStdioSession(owner, "lsp", owner.Workspace, "/bin/cat")
	if err != nil {
		t.Fatalf("fresh worker session could not start after DAP retirement: %v", err)
	}
	if err := s.workerStdioClose(owner, id); err != nil {
		t.Fatalf("fresh worker session cleanup failed: %v", err)
	}
}

func TestDAPRecoveryEvidenceDoesNotLeakAcrossPrincipalsOrPrematurelyClaimCleanup(t *testing.T) {
	s, owner, _ := completedDAPSession(t, "running", nil)
	for _, altered := range []struct {
		name string
		req  Request
	}{
		{"different_principal", func() Request { q := owner; q.PrincipalID = "different-client"; return q }()},
		{"different_workspace", func() Request { q := owner; q.Workspace = s.cfg.WorkspaceDir; return q }()},
		{"root_crossover", func() Request { q := owner; q.Root = true; q.Approval = true; return q }()},
	} {
		t.Run(altered.name, func(t *testing.T) {
			if resp := s.debugAction(context.Background(), altered.req); resp.OK || resp.ErrorCode != "debug_session_not_found" && resp.ErrorCode != "debug_worker_only" {
				t.Fatalf("cross-scope DAP status leaked broker identity: %+v", resp)
			}
		})
	}
	entry, err := s.sessions.get(owner, owner.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	entry.exited = false
	entry.mu.Unlock()
	resp := s.debugAction(context.Background(), owner)
	body := statusBody(t, resp)
	if body["adapter_exited"] != false {
		t.Fatalf("live adapter falsely marked exited: %v", body)
	}
	if _, ok := body["cleanup_verified"]; ok {
		t.Fatalf("live adapter falsely claimed cleanup proof: %v", body)
	}
	if body["state"] != "running" || resp.Running == nil || !*resp.Running {
		t.Fatalf("live DAP status changed active protocol state: %+v", resp)
	}
}

func TestDAPNormalTerminationRetainsProtocolState(t *testing.T) {
	s, owner, _ := completedDAPSession(t, "terminated", nil)
	resp := s.debugAction(context.Background(), owner)
	body := statusBody(t, resp)
	if body["state"] != "terminated" || body["cleanup_verified"] != true || body["adapter_exited"] != true {
		t.Fatalf("normal termination mislabeled as a crash: %v", body)
	}
	owner.Action = "debug_stop"
	if result := s.debugAction(context.Background(), owner); !result.OK || result.Status != "cleanup_confirmed_adapter_already_exited" {
		t.Fatalf("normal post-exit cleanup cannot be finalized: %+v", result)
	}
}

func TestDAPUnverifiedCleanupRemainsAddressableAndNonExecutable(t *testing.T) {
	s, owner, _ := completedDAPSession(t, "stopped", errors.New("private DAP containment was not proven empty"))
	resp := s.debugAction(context.Background(), owner)
	body := statusBody(t, resp)
	if body["state"] != "uncertain" || body["cleanup_verified"] != false || body["adapter_exited"] != true {
		t.Fatalf("unverified target cleanup falsely presented as safe: %v", body)
	}
	owner.Action, owner.DebugMethod = "debug_action", "evaluate"
	owner.DebugFrameID, owner.DebugExpression = 1000, "sideEffect()"
	if result := s.debugAction(context.Background(), owner); result.OK || result.ErrorCode != "debug_cleanup_uncertain" {
		t.Fatalf("potentially side-effecting request passed unverified cleanup: %+v", result)
	}
	owner.Action = "debug_stop"
	if result := s.debugAction(context.Background(), owner); result.OK || result.ErrorCode != "debug_stop_uncertain" {
		t.Fatalf("missing pidfd/cgroup proof incorrectly acknowledged cleanup: %+v", result)
	}
	owner.Action = "debug_status"
	if result := s.debugAction(context.Background(), owner); !result.OK {
		t.Fatalf("uncertain session lost its addressable reconciliation identity: %+v", result)
	}
}

// A passive status poll can observe a complete terminal event and then EOF
// before the broker publishes adapter-exit/containment evidence. That EOF
// cannot permanently poison a received valid protocol termination.
func TestDAPStatusPollTerminationEOFReconcilesAfterBrokerExit(t *testing.T) {
	s, owner, peer := completedDAPSession(t, "running", nil)
	e, err := s.sessions.get(owner, owner.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.exited = false
	e.exitCode = 0
	e.mu.Unlock()
	sent := make(chan error, 1)
	go func() {
		err := dapFrame(peer, map[string]any{
			"seq": 33, "type": "event", "event": "terminated",
			"body": map[string]any{"reason": "completed"},
		})
		_ = peer.Close()
		sent <- err
	}()
	var interim map[string]any
	for i := 0; i < 5; i++ {
		interim = statusBody(t, s.debugAction(context.Background(), owner))
		e.dap.mu.Lock()
		closed := e.dap.passiveClosed
		e.dap.mu.Unlock()
		if closed {
			break
		}
		if i == 4 {
			t.Fatal("status never observed the stream close during adapter shutdown")
		}
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if interim["state"] != "uncertain" || interim["adapter_exited"] != false {
		t.Fatalf("unreaped adapter with closed status stream was falsely proven: %v", interim)
	}
	e.dap.mu.Lock()
	if e.dap.uncertain || e.dap.stage != "terminated" {
		t.Fatalf("passive EOF poisoned a genuine terminal event: stage=%s uncertain=%v", e.dap.stage, e.dap.uncertain)
	}
	e.dap.mu.Unlock()

	// Broker publication happens AFTER the stream closed and the event was
	// already delivered/acknowledged. Status must retain terminal evidence.
	e.mu.Lock()
	e.exited = true
	e.exitCode = 0
	e.cleanupErr = nil
	e.mu.Unlock()
	final := statusBody(t, s.debugAction(context.Background(), owner))
	if final["state"] != "terminated" || final["adapter_exited"] != true ||
		final["cleanup_verified"] != true || final["adapter_exit_code"] != float64(0) {
		t.Fatalf("normal termination overwritten by stale passive transport closure: %v", final)
	}
}

func TestDAPStatusPollAbruptEOFBecomesFailedAfterVerifiedCleanup(t *testing.T) {
	s, owner, peer := completedDAPSession(t, "stopped", nil)
	e, err := s.sessions.get(owner, owner.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.exited = false
	e.mu.Unlock()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	pending := statusBody(t, s.debugAction(context.Background(), owner))
	if pending["state"] != "uncertain" || pending["adapter_exited"] != false {
		t.Fatalf("premature certainty before broker cleanup proof: %v", pending)
	}
	e.dap.mu.Lock()
	if e.dap.uncertain || !e.dap.passiveClosed {
		t.Fatalf("passive crash EOF was conflated with a transmitted request: uncertain=%v passiveClosed=%v", e.dap.uncertain, e.dap.passiveClosed)
	}
	e.dap.mu.Unlock()
	e.mu.Lock()
	e.exited, e.exitCode, e.cleanupErr = true, 137, nil
	e.mu.Unlock()
	final := statusBody(t, s.debugAction(context.Background(), owner))
	if final["state"] != "failed" || final["adapter_exited"] != true ||
		final["cleanup_verified"] != true || final["adapter_exit_code"] != float64(137) {
		t.Fatalf("verified abrupt crash was not reported failed: %v", final)
	}
}

func TestDAPUnknownSentEvaluateRemainsUncertainAfterVerifiedAdapterExit(t *testing.T) {
	s, owner, peer := completedDAPSession(t, "stopped", nil)
	e, err := s.sessions.get(owner, owner.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.exited = false
	e.dap.configured = true
	e.mu.Unlock()
	gotWire := make(chan bool, 1)
	go func() {
		var packet [4096]byte
		n, _ := peer.Read(packet[:])
		gotWire <- n > 0 && strings.Contains(string(packet[:n]), `"command":"evaluate"`)
		_ = peer.Close() // response/side-effect completion becomes unknown
	}()
	owner.Action, owner.DebugMethod = "debug_action", "evaluate"
	owner.DebugFrameID, owner.DebugExpression = 1000, "couldMutate()"
	resp := s.debugAction(context.Background(), owner)
	if resp.OK || resp.ErrorCode != "debug_action_uncertain" {
		t.Fatalf("lost evaluate acknowledgement was accepted: %+v", resp)
	}
	if !<-gotWire {
		t.Fatal("did not actually transmit the potentially side-effecting evaluate request")
	}
	e.dap.mu.Lock()
	if !e.dap.uncertain {
		t.Fatal("unknown post-send evaluation did not remain permanently uncertain")
	}
	e.dap.mu.Unlock()
	e.mu.Lock()
	e.exited, e.cleanupErr = true, nil
	e.mu.Unlock()
	owner.Action = "debug_status"
	final := statusBody(t, s.debugAction(context.Background(), owner))
	if final["state"] != "uncertain" || final["cleanup_verified"] != true {
		t.Fatalf("verified OS cleanup incorrectly cleared unknown evaluate outcome: %v", final)
	}
}

func TestDAPConcurrentVerifiedStopAndStatusDoNotDoubleRetire(t *testing.T) {
	s, owner, peer := completedDAPSession(t, "terminated", nil)
	stop := owner
	stop.Action = "debug_stop"
	results := make(chan Response, 3)
	go func() { results <- s.debugAction(context.Background(), stop) }()
	go func() { results <- s.debugAction(context.Background(), stop) }()
	go func() { results <- s.debugAction(context.Background(), owner) }()
	okStops := 0
	for i := 0; i < 3; i++ {
		select {
		case resp := <-results:
			if resp.OK && resp.Status == "cleanup_confirmed_adapter_already_exited" {
				okStops++
			} else if !resp.OK && resp.ErrorCode == "debug_session_not_found" {
				// A simultaneous status/stop after retirement is expected.
			} else if resp.OK && (resp.Status == "terminated" || resp.Status == "failed") {
				// Status won the action lock before stop, and is still valid.
			} else {
				t.Fatalf("inconsistent concurrent stop/status: %+v", resp)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent status and stop deadlocked")
		}
	}
	if okStops != 1 {
		t.Fatalf("duplicate or missing retirement: successful_stops=%d", okStops)
	}
	_ = peer.SetReadDeadline(time.Now().Add(35 * time.Millisecond))
	var unexpected [1]byte
	if n, err := peer.Read(unexpected[:]); n != 0 || (!isReadTimeout(err) && !errors.Is(err, io.EOF)) {
		t.Fatalf("post-exit DAP packet was sent: n=%d err=%v", n, err)
	}
}
