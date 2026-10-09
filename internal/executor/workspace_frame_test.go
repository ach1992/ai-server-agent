package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// Exercise the real compiled self-exec helper, not a stubbed marshal decoder.
// Valid raw-file size does not imply small JSON: ASCII NUL becomes \u0000.
func TestWorkspaceHelperRequestFrameBlackBox(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged helper fixture runs without root; root delegation belongs to isolated security CI")
	}
	requireWorkerLandlockV2(t)
	root, repo, _ := workspaceFixture(t)
	binary := filepath.Join(t.TempDir(), "ai-server-agent")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workspace helper: %v: %s", err, output)
	}
	auditFile := filepath.Join(t.TempDir(), "audit.jsonl")
	s := &Server{cfg: config.Config{WorkspaceDir: root}, runs: newRunLimiterWith(1, 1),
		workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()), audit: audit.New(auditFile),
		workspaceHelperBinary: binary}
	// Max permitted single-file body with a 6:1 escaping expansion. This was
	// previously accepted then misreported as unknown_completion (>2 MiB).
	body := strings.Repeat("\x00", maxFileWriteBytes)
	op := workspaceFileOperation{Action: "workspace_write", WorkspaceRoot: root, Workspace: repo, Path: "src/high-escape.dat", Content: body, MustNotExist: true}
	serialized, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(serialized) <= 2<<20 || len(serialized) > maxWorkspaceHelperRequestBytes {
		t.Fatalf("unexpected large valid payload size %d", len(serialized))
	}
	one := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_write", Workspace: repo, Path: op.Path, Content: body, MustNotExist: true})
	if !one.OK || one.FileVersion == "" {
		t.Fatalf("valid escaped write incorrectly rejected/ambiguous: %+v", one)
	}
	if info, err := os.Stat(filepath.Join(repo, op.Path)); err != nil || info.Size() != int64(len(body)) {
		t.Fatalf("escaped file result: %v info=%v", err, info)
	}

	// Multi-file payload larger than the former 2 MiB helper limit, still
	// inside every raw-content/aggregate bound, must reach actual commits.
	batch := Request{Action: "workspace_apply_edits", Workspace: repo}
	for i := 0; i < 3; i++ {
		content := strings.Repeat(string(rune('a'+i)), 800<<10)
		batch.WorkspaceEdits = append(batch.WorkspaceEdits, WorkspaceFileEdit{
			Path: fmt.Sprintf("src/batch-%d.txt", i), Content: &content, MustNotExist: true,
		})
	}
	opBatch := workspaceFileOperation{Action: batch.Action, WorkspaceRoot: root, Workspace: repo, Edits: batch.WorkspaceEdits}
	encodedBatch, _ := json.Marshal(opBatch)
	if len(encodedBatch) <= 2<<20 || len(encodedBatch) > maxWorkspaceHelperRequestBytes {
		t.Fatalf("batch wire size=%d", len(encodedBatch))
	}
	result := s.workerWorkspaceFile(t.Context(), batch)
	if !result.OK {
		t.Fatalf("valid multi-file batch failed: %+v", result)
	}
	outcome, err := decodeWorkspaceEditResult(result)
	if err != nil || !outcome.Complete || outcome.Applied != 3 {
		t.Fatalf("wrong batch outcome: %+v err=%v", outcome, err)
	}
	for _, entry := range outcome.Files {
		if entry.Status != "applied" || entry.FileVersion == "" {
			t.Fatalf("missing applied file outcome: %+v", entry)
		}
	}

	// Raw edits are inside the 3 MiB aggregate contract but their escaped
	// JSON exceeds 8 MiB: reject deterministically before audit/process.
	over := Request{Action: "workspace_apply_edits", Workspace: repo}
	for i := 0; i < 3; i++ {
		content := strings.Repeat("\x00", 512<<10)
		over.WorkspaceEdits = append(over.WorkspaceEdits, WorkspaceFileEdit{
			Path: fmt.Sprintf("src/over-%d.txt", i), Content: &content, MustNotExist: true,
		})
	}
	wire, _ := json.Marshal(workspaceFileOperation{Action: over.Action, WorkspaceRoot: root, Workspace: repo, Edits: over.WorkspaceEdits})
	if len(wire) <= maxWorkspaceHelperRequestBytes {
		t.Fatalf("overlimit fixture insufficient: %d", len(wire))
	}
	beforeAudit, err := os.Stat(auditFile)
	if err != nil {
		t.Fatal(err)
	}
	bad := s.workerWorkspaceFile(t.Context(), over)
	if bad.OK || bad.ErrorCode != "input_too_large" || bad.ErrorClass != "resource" {
		t.Fatalf("overlimit misreported as completion: %+v", bad)
	}
	afterAudit, err := os.Stat(auditFile)
	if err != nil || afterAudit.Size() != beforeAudit.Size() {
		t.Fatalf("overlimit began audit: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(filepath.Join(repo, fmt.Sprintf("src/over-%d.txt", i))); !os.IsNotExist(err) {
			t.Fatalf("overlimit unexpectedly wrote file: %v", err)
		}
	}

	// Exact helper stdin frame boundaries. Large valid JSON followed by
	// whitespace tests the raw framing independently of public input caps.
	if err := os.WriteFile(filepath.Join(repo, "src", "frame.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	basic, _ := json.Marshal(workspaceFileOperation{Action: "workspace_read", WorkspaceRoot: root, Workspace: repo, Path: "src/frame.txt"})
	padded := append(basic, bytes.Repeat([]byte(" "), maxWorkspaceHelperRequestBytes-len(basic))...)
	call := func(raw []byte) Response {
		t.Helper()
		cmd := exec.Command(binary, "workspace-helper")
		cmd.Stdin = bytes.NewReader(raw)
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("helper framing subprocess returned error: %v", err)
		}
		var resp Response
		if err := json.Unmarshal(output, &resp); err != nil {
			t.Fatalf("helper framing response invalid: %v", err)
		}
		return resp
	}
	exact := call(padded)
	if !exact.OK || exact.Output != "ok" {
		t.Fatalf("exact frame rejected: %+v", exact)
	}
	excess := call(append(padded, ' '))
	if excess.OK || excess.ErrorCode != "input_too_large" || excess.ErrorClass != "resource" {
		t.Fatalf("one byte over frame was not deterministic: %+v", excess)
	}
}

// The public MCP client path must not turn an oversized private executor
// frame into an opaque socket failure, even before it reaches the helper.
func TestWorkspaceClientFrameRejectsBeforeDial(t *testing.T) {
	req := Request{Action: "workspace_apply_edits", Workspace: "repo"}
	for i := 0; i < 3; i++ {
		content := strings.Repeat("\x00", 512<<10)
		req.WorkspaceEdits = append(req.WorkspaceEdits, WorkspaceFileEdit{
			Path: fmt.Sprintf("src/over-%d.txt", i), Content: &content, MustNotExist: true,
		})
	}
	resp, err := ClientCallContext(context.Background(), "/definitely/nonexistent/executor.sock", "test-token", req)
	if err != nil || resp.OK || resp.ErrorCode != "input_too_large" || resp.ErrorClass != "resource" {
		t.Fatalf("private socket oversized workspace operation was not rejected before dial: resp=%+v err=%v", resp, err)
	}
}
