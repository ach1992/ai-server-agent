package executor

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

func TestAuditWorkspaceTargetPathsScopeAndBounds(t *testing.T) {
	tooLong := strings.Repeat("a", 257)
	cases := []struct {
		name    string
		req     Request
		want    []string
		omitted int
	}{
		{"single", Request{Action: "workspace_write", Path: "docs/../src/main.go"}, []string{"src/main.go"}, 0},
		{"batch", Request{Action: "workspace_apply_edits", WorkspaceEdits: []WorkspaceFileEdit{{Path: "src/a.go"}, {Path: "docs/b.md"}}}, []string{"src/a.go", "docs/b.md"}, 0},
		{"transfer-commit", Request{Action: "workspace_transfer", TransferOp: "commit", Path: "bin/out.dat"}, []string{"bin/out.dat"}, 0},
		{"transfer-staging", Request{Action: "workspace_transfer", TransferOp: "chunk", Path: "bin/out.dat"}, nil, 0},
		{"root", Request{Action: "workspace_write", Root: true, Path: "src/main.go"}, nil, 0},
		{"privileged-file", Request{Action: "write_file", Root: true, Path: "/etc/shadow"}, nil, 0},
		{"read-only", Request{Action: "workspace_read", Path: "src/main.go"}, nil, 0},
		{"invalid-long", Request{Action: "workspace_write", Path: tooLong}, nil, 1},
		{"invalid-traversal", Request{Action: "workspace_write", Path: "../private.key"}, nil, 1},
		{"invalid-git", Request{Action: "workspace_write", Path: ".git/config"}, nil, 1},
		{"invalid-control", Request{Action: "workspace_write", Path: "src/\nsecret"}, nil, 1},
		{"mixed-batch", Request{Action: "workspace_apply_edits", WorkspaceEdits: []WorkspaceFileEdit{{Path: "src/good.go"}, {Path: tooLong}, {Path: "more/ok.txt"}}}, []string{"src/good.go", "more/ok.txt"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, omitted := auditWorkspaceTargetPaths(tc.req, tc.req.Action)
			if !reflect.DeepEqual(got, tc.want) || omitted != tc.omitted {
				t.Fatalf("got paths=%q omitted=%d; want paths=%q omitted=%d", got, omitted, tc.want, tc.omitted)
			}
		})
	}
}

func TestAuditWorkspaceWriteRecordsOnlyBoundedTargetsAndOutcome(t *testing.T) {
	s, workspace, auditPath := newAuditContractServer(t)
	req := Request{
		Action: "workspace_apply_edits", Workspace: workspace,
		WorkspaceEdits: []WorkspaceFileEdit{{Path: "src/one.go"}, {Path: "src/two.go"}},
		RequestID:      "req-target", PrincipalID: "direct-default", Content: "API_KEY=secret-only-in-content",
	}
	command := req.Workspace + "\x00" + req.Path
	if blocked := s.beginActionAudit(req, req.Action, "worker", command, "workspace"); blocked != nil {
		t.Fatalf("pre-action audit blocked: %+v", *blocked)
	}
	result := s.finishActionAudit(req, req.Action, "worker", command, "workspace", time.Now(), Response{
		OK: false, ErrorCode: "partial_workspace_edit", ReasonCode: "partial_workspace_edit",
	})
	if result.AuditDegraded || result.OK {
		t.Fatalf("simulated partial outcome should be recorded without audit degradation: %+v", result)
	}
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), workspace) || strings.Contains(string(raw), req.Content) {
		t.Fatalf("absolute workspace or file contents leaked into audit")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected start + completion audit, got %d lines", len(lines))
	}
	for i, line := range lines {
		var entry audit.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.SchemaVersion != audit.SchemaVersion || !reflect.DeepEqual(entry.TargetPaths, []string{"src/one.go", "src/two.go"}) || entry.TargetPathsOmitted != 0 || entry.CommandFingerprint == "" {
			t.Fatalf("audit target metadata invalid: %+v", entry)
		}
		if i == 0 && (entry.Phase != "start" || entry.Success != nil) {
			t.Fatalf("start claimed outcome: %+v", entry)
		}
		if i == 1 && (entry.Phase != "complete" || entry.Success == nil || *entry.Success || entry.ReasonCode != "partial_workspace_edit") {
			t.Fatalf("completion outcome incorrect: %+v", entry)
		}
	}
}

func TestAuditRootWriteKeepsPathsFingerprintOnly(t *testing.T) {
	s, _, auditPath := newAuditContractServer(t)
	const path = "/etc/ssh/sshd_config"
	req := Request{Action: "write_file", Root: true, Path: path, RequestID: "req-root"}
	if blocked := s.beginActionAudit(req, req.Action, "root", path, "root"); blocked != nil {
		t.Fatalf("root audit failed: %+v", *blocked)
	}
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), path) || strings.Contains(string(raw), "target_paths") {
		t.Fatalf("privileged host pathname leaked into log: %s", raw)
	}
	var entry audit.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.CommandFingerprint == "" || len(entry.TargetPaths) != 0 {
		t.Fatalf("root path audit not fingerprint-only: %+v", entry)
	}
}
