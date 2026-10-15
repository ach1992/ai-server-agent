package executor

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixtureRustELF() []byte {
	h := make([]byte, 64)
	copy(h, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(h[16:18], 3) // PIE ET_DYN
	machine := uint16(0x3e)
	if runtime.GOARCH == "arm64" {
		machine = 0xb7
	}
	binary.LittleEndian.PutUint16(h[18:20], machine)
	return h
}

func TestRustDebugELFHeaderRejectsForeignOrNonExecutable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"valid_pie", fixtureRustELF(), true},
		{"script", []byte("#!/bin/sh\necho not a Rust binary\n"), false},
		{"empty", nil, false},
		{"wrong_arch", func() []byte { h := fixtureRustELF(); binary.LittleEndian.PutUint16(h[18:20], 0x28); return h }(), false},
		{"wrong_elf_type", func() []byte { h := fixtureRustELF(); binary.LittleEndian.PutUint16(h[16:18], 1); return h }(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := rustDebugELFHeader(tc.data)
			if (err == nil) != tc.valid {
				t.Fatalf("ELF identity %s valid=%t, error=%v", tc.name, tc.valid, err)
			}
		})
	}
}

func TestRustDAPStatusAndUnsupportedModesFailClosed(t *testing.T) {
	s, req, workspace := testStdioBroker(t)
	req.Action, req.Language = "debug_adapter_status", "rust"
	status := s.debugAction(context.Background(), req)
	if !status.OK || !strings.Contains(status.Output, `"adapter":"rust/lldb-dap"`) ||
		!strings.Contains(status.Output, `"attach_supported":false`) {
		t.Fatalf("Rust DAP readiness did not describe explicit local-only capabilities: %+v", status)
	}
	req.Action, req.DebugMode, req.DebugProgram, req.FileVersion = "debug_launch", "attach", "prebuilt", "version"
	result := s.debugAction(context.Background(), req)
	if result.OK || result.ErrorCode != "rust_dap_unsupported_mode" || result.SessionID != "" {
		t.Fatalf("Rust attach unexpectedly admitted: %+v", result)
	}
	_ = workspace
}

func TestRustDAPRequiresRootContainmentBeforeAdapterLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("must run as unprivileged worker")
	}
	requireWorkerLandlockV2(t)
	s, req, workspace := testStdioBroker(t)
	builder, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go builder unavailable for real worker helper")
	}
	helper := filepath.Join(t.TempDir(), "agent-workspace-helper")
	cmd := exec.Command(builder, "build", "-o", helper, "./cmd/ai-server-agent")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build worker helper: %v: %s", err, out)
	}
	s.workspaceHelperBinary = helper
	// The fixture consists only of a valid *header*, not runnable program
	// code. Admission must abort before any adapter ever sees it.
	if err := os.WriteFile(filepath.Join(workspace, "rust-fixture"), fixtureRustELF(), 0700); err != nil {
		t.Fatal(err)
	}
	check := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "rust-fixture"})
	if !check.OK {
		t.Fatalf("fixture workspace stat failed: %+v", check)
	}
	req.Action, req.Language, req.DebugMode, req.DebugProgram, req.FileVersion = "debug_launch", "rust", "exec", "rust-fixture", check.FileVersion
	response := s.debugAction(context.Background(), req)
	if response.OK || response.ErrorCode != "dap_containment_unavailable" || response.SessionID != "" {
		t.Fatalf("uncontained Rust LLDB target unexpectedly admitted: %+v", response)
	}
	// A source mutation must be detected even before the containment gate.
	if err := os.WriteFile(filepath.Join(workspace, "rust-fixture"), append(fixtureRustELF(), byte(1)), 0700); err != nil {
		t.Fatal(err)
	}
	stale := s.debugAction(context.Background(), req)
	if stale.OK || stale.ErrorCode != "debug_program_changed" {
		t.Fatalf("stale Rust ELF unexpectedly admitted: %+v", stale)
	}
	s.sessions.mu.Lock()
	n := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed Rust launch left %d sessions", n)
	}
}

// Test the real executor debug_action routing, principal/workspace identity,
// source-version preconditions and existing DAP wire with Rust .rs sources.
// The dummy transport is not a substitute for LLDB adapter E2E acceptance.
func TestRustDAPBreakpointUsesVersionedWorkerSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged worker")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.workspaceHelperBinary = buildNodeProofWorkspaceHelper(t)
	if err := os.WriteFile(filepath.Join(workspace, "main.rs"), []byte("fn main() { let value = 7; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stat := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: "main.rs"})
	if !stat.OK {
		t.Fatalf("Rust workspace source unavailable: %+v", stat)
	}
	id, err := s.workerStdioSession(owner, "dap", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.workerStdioClose(owner, id) }()
	e, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	client, adapter := net.Pipe()
	defer client.Close()
	defer adapter.Close()
	d := &dapState{conn: client, workspace: workspace, adapter: "rust/lldb-dap", stage: "starting"}
	e.mu.Lock()
	e.dap = d
	e.mu.Unlock()
	owner.Action, owner.SessionID, owner.DebugMethod, owner.Path = "debug_action", id, "breakpoints", "main.rs"
	owner.DebugLines, owner.FileVersion = []int{1}, "stale"
	if got := s.debugAction(t.Context(), owner); got.OK || got.ErrorCode != "debug_source_changed" {
		t.Fatalf("stale Rust breakpoint accepted: %+v", got)
	}
	owner.Path, owner.FileVersion = "main.py", stat.FileVersion
	if got := s.debugAction(t.Context(), owner); got.OK || got.ErrorCode != "debug_invalid_breakpoints" {
		t.Fatalf("Python source accepted as Rust breakpoint: %+v", got)
	}
	owner.Path, owner.FileVersion = "main.rs", stat.FileVersion
	done := make(chan error, 1)
	go func() {
		r, err := dapFixtureRead(bufio.NewReader(adapter))
		if err != nil {
			done <- err
			return
		}
		if r.Command != "setBreakpoints" {
			done <- os.ErrInvalid
			return
		}
		done <- dapFrame(adapter, map[string]any{"seq": 2, "type": "response", "request_seq": r.Seq, "command": "setBreakpoints", "success": true, "body": map[string]any{"breakpoints": []any{map[string]any{"line": 1, "verified": true}}}})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	got := s.debugAction(ctx, owner)
	if !got.OK || !strings.Contains(got.Output, `"verified":true`) {
		t.Fatalf("Rust breakpoint via real executor DAP action failed: %+v", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Rust DAP fixture protocol hung")
	}
	if d.sourceVersions["main.rs"] != stat.FileVersion {
		t.Fatal("Rust breakpoint source identity not pinned")
	}
}
