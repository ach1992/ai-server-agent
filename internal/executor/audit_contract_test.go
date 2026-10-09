package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

func newAuditContractServer(t *testing.T) (*Server, string) {
	t.Helper()
	workspace := t.TempDir()
	return &Server{
		cfg:       config.Config{WorkspaceDir: workspace, StateDir: t.TempDir()},
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
		runs:      newRunLimiterWith(1, 1),
	}, workspace
}

func TestPreActionAuditFailureBlocksRunSideEffect(t *testing.T) {
	s, workspace := newAuditContractServer(t)
	s.auditWriteHook = func(phase string) error {
		if phase == "start" {
			return errors.New("forced pre-action audit failure")
		}
		return nil
	}
	marker := filepath.Join(workspace, "must-not-exist")
	resp := s.runContext(context.Background(), Request{Command: "touch " + shellQuote(marker), RequestID: "req-preaudit"})
	if resp.OK || resp.ErrorCode != "audit_unavailable" || resp.Status != "degraded_safety" {
		t.Fatalf("unexpected audit failure response: %+v", resp)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command side effect occurred despite missing pre-action audit; err=%v", err)
	}
}

func TestCompletionAuditFailureSurfacesOutcomeAndBlocksNextMutation(t *testing.T) {
	s, workspace := newAuditContractServer(t)
	s.auditWriteHook = func(phase string) error {
		if phase == "complete" {
			return errors.New("forced completion audit failure")
		}
		return nil
	}
	first := filepath.Join(workspace, "first.txt")
	resp := s.writeFile(Request{Path: first, Content: "committed", Root: true, RequestID: "req-complete-fail"})
	if !resp.OK || !resp.AuditDegraded || resp.AuditError == "" {
		t.Fatalf("real operation outcome was not preserved with explicit audit degradation: %+v", resp)
	}
	if got, err := os.ReadFile(first); err != nil || string(got) != "committed" {
		t.Fatalf("first mutation result missing: %q err=%v", got, err)
	}

	s.auditWriteHook = nil
	second := filepath.Join(workspace, "second.txt")
	blocked := s.writeFile(Request{Path: second, Content: "must not be written", Root: true, RequestID: "req-after-degrade"})
	if blocked.OK || blocked.ErrorCode != "audit_unavailable" || blocked.Status != "degraded_safety" {
		t.Fatalf("subsequent mutation was not blocked after audit degradation: %+v", blocked)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("subsequent mutation ran after audit degradation; err=%v", err)
	}
}

func TestPreActionAuditFailureBlocksFileWrite(t *testing.T) {
	s, workspace := newAuditContractServer(t)
	s.auditWriteHook = func(phase string) error {
		if phase == "start" {
			return errors.New("forced pre-action audit failure")
		}
		return nil
	}
	path := filepath.Join(workspace, "blocked-write.txt")
	resp := s.writeFile(Request{Path: path, Content: "must not be written", Root: true, RequestID: "req-write"})
	if resp.OK || resp.ErrorCode != "audit_unavailable" {
		t.Fatalf("write was not blocked by missing pre-action audit: %+v", resp)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file appeared despite missing pre-action audit; err=%v", err)
	}
}

func TestPreActionAuditFailureBlocksPersistentJobStateMutation(t *testing.T) {
	s, _ := newAuditContractServer(t)
	s.auditWriteHook = func(phase string) error {
		if phase == "start" {
			return errors.New("forced pre-action audit failure")
		}
		return nil
	}
	resp := s.startJobBounded(Request{Command: "printf should-not-launch", OperationID: "op-audit-block", RequestID: "req-job"})
	if resp.OK || resp.ErrorCode != "audit_unavailable" {
		t.Fatalf("persistent job was not blocked by missing pre-action audit: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "jobs")); !os.IsNotExist(err) {
		t.Fatalf("persistent job state was created before pre-action audit; err=%v", err)
	}
}

func TestBrowserAuditActionOverrideIsBounded(t *testing.T) {
	if got := auditActionName(Request{AuditAction: "browser_run"}, "run"); got != "browser_run" {
		t.Fatalf("browser action override = %q", got)
	}
	if got := auditActionName(Request{AuditAction: "caller-controlled"}, "run"); got != "run" {
		t.Fatalf("untrusted audit action override escaped bounded set: %q", got)
	}
}

func TestAuditCorrelationAcrossDirectAndGatewayPrincipals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s := &Server{audit: audit.New(path)}
	principals := []struct {
		id, class, name string
	}{
		{id: "direct-default", class: "direct", name: "direct/default"},
		{id: "mcp-gateway", class: "gateway", name: "mcp-gateway"},
	}
	for i, principal := range principals {
		req := Request{
			RequestID:      "req-correlation-" + string(rune('a'+i)),
			OperationID:    "op-correlation-" + string(rune('a'+i)),
			ApprovalID:     "approval-correlation-" + string(rune('a'+i)),
			PrincipalID:    principal.id,
			PrincipalClass: principal.class,
			PrincipalName:  principal.name,
		}
		if blocked := s.beginActionAudit(req, "start_job", "worker", "printf secret-token-value", "command"); blocked != nil {
			t.Fatalf("start audit failed: %+v", blocked)
		}
		resp := s.finishActionAudit(req, "start_job", "worker", "printf secret-token-value", "command", time.Now(), Response{OK: true, JobID: "job-" + string(rune('a'+i))})
		if !resp.OK || resp.AuditDegraded {
			t.Fatalf("completion audit failed: %+v", resp)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-token-value") {
		t.Fatalf("raw command payload leaked into audit: %s", b)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 {
		t.Fatalf("audit lines=%d, want 4", len(lines))
	}
	seenPrincipals := map[string]int{}
	for _, line := range lines {
		var event audit.Entry
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.RequestID == "" || event.OperationID == "" || event.ApprovalID == "" || event.CommandFingerprint == "" {
			t.Fatalf("correlation metadata incomplete: %+v", event)
		}
		seenPrincipals[event.PrincipalID]++
	}
	if seenPrincipals["direct-default"] != 2 || seenPrincipals["mcp-gateway"] != 2 {
		t.Fatalf("principal correlation counts=%v", seenPrincipals)
	}
}
