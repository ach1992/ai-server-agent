package executor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Passive runtime_list must not create a terminal registry or imply that
// a stale same-principal record is an eligible, reconnectable worker handle.
func TestPassiveRuntimeWorkerTerminalScopeAndPrincipal(t *testing.T) {
	s, _, workspace := newSharedRuntimeTestServer(t)
	// Linux sun_path is only 107 bytes; Go's descriptive t.TempDir() test
	// names exceed it before tmux's random socket suffix is appended.
	state, err := os.MkdirTemp("/tmp", "asa-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	s.cfg.StateDir = state
	jobs := filepath.Join(state, "jobs")
	if err := os.Mkdir(jobs, 0711); err != nil {
		t.Fatal(err)
	}
	mockActiveUnits(t)
	req := Request{PrincipalID: "principal-a", PrincipalClass: "direct", Kind: "terminal", Workspace: workspace}
	if got := s.runtimeList(req); !got.OK || len(got.RuntimeEntries) != 0 {
		t.Fatalf("empty inventory failed: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(state, "terminal-sessions")); !os.IsNotExist(err) {
		t.Fatalf("read-only inventory created a registry: %v", err)
	}
	for _, dir := range []string{"terminal-sessions", "worker-terminals"} {
		if err := os.Mkdir(filepath.Join(state, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	id := "stdio_" + strings.Repeat("a", 32)
	socketDir := filepath.Join(state, "worker-terminals", ".asa-tmux-ABC123")
	if err := os.Mkdir(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "socket")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rec := terminalRecord{
		Version: 1, ID: id, OwnerID: req.PrincipalID, OwnerClass: req.PrincipalClass,
		Workspace: workspace, Root: false, Scoped: true, Lifecycle: "active",
		SocketDir: socketDir, Name: "asa_" + strings.Repeat("b", 24), Pane: "%0",
		Columns: 80, Rows: 24, ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	}
	path := filepath.Join(state, "terminal-sessions", id+".json")
	write := func() {
		t.Helper()
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	shown := s.runtimeList(req)
	if !shown.OK || len(shown.RuntimeEntries) != 1 || shown.RuntimeEntries[0].ID != id ||
		shown.RuntimeEntries[0].Kind != "terminal" || shown.RuntimeEntries[0].Status != "unknown" {
		t.Fatalf("unverified PID1 scope must remain unknown: %+v", shown)
	}
	if shown.RuntimeEntries[0].CreatedAt == "" || shown.RuntimeEntries[0].ExpiresAt == "" {
		t.Fatalf("missing retained terminal time bound: %+v", shown.RuntimeEntries[0])
	}
	foreign := s.runtimeList(Request{PrincipalID: "principal-b", PrincipalClass: "direct", Kind: "terminal"})
	if !foreign.OK || len(foreign.RuntimeEntries) != 0 {
		t.Fatalf("leaked another principal's Terminal: %+v", foreign)
	}
	otherWorkspace := filepath.Join(workspace, "other-project")
	if err := os.Mkdir(otherWorkspace, 0700); err != nil {
		t.Fatal(err)
	}
	other := s.runtimeList(Request{PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, Kind: "terminal", Workspace: otherWorkspace})
	if !other.OK || len(other.RuntimeEntries) != 0 {
		t.Fatalf("leaked cross-workspace Terminal: %+v", other)
	}
	// Root PTY must not become discoverable just because principal matches.
	rec.Root = true
	write()
	if got := s.runtimeList(req); len(got.RuntimeEntries) != 0 {
		t.Fatalf("root PTY leaked: %+v", got)
	}
	rec.Root = false
	write()
	rec.ExpiresAt = time.Now().UTC().Add(-time.Second)
	write()
	if got := s.runtimeList(req); len(got.RuntimeEntries) != 0 {
		t.Fatalf("expired PTY advertised as recoverable: %+v", got)
	}
	rec.ExpiresAt = time.Now().UTC().Add(30 * time.Minute)
	write()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.runtimeList(req); len(got.RuntimeEntries) != 0 {
		t.Fatalf("vanished socket advertised as recoverable: %+v", got)
	}
}
