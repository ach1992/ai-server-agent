package executor

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// A real systemd service RESTART is required to reproduce the original bug.
// This gated test creates its OWN disposable broker service. It never
// restarts or modifies an installed AI Server Agent or its configured services.
func TestApprovedWorkerScopeRealSystemdUnitRestart(t *testing.T) {
	host, _ := os.Hostname()
	if host != "CLY535343" || os.Geteuid() != 0 ||
		os.Getenv("ASA_PTY_SCOPE_APPROVED_HOST") != host {
		t.Skip("independent approved root-owned disposable-host gate")
	}
	if childDir := os.Getenv("ASA_PTY_SCOPE_CHILD_TEST_DIR"); childDir != "" {
		approvedScopeRestartChild(t, childDir)
		return
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	unit := "asa-pty-broker-restart-proof-" + hex.EncodeToString(nonce[:]) + ".service"
	base, err := os.MkdirTemp("/tmp", "asa-pty-real-restart-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	state := filepath.Join(base, "state")
	if err := os.Mkdir(state, 0711); err != nil {
		t.Fatal(err)
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(worker.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	privateHome := filepath.Join(state, "worker-home")
	if err := os.Mkdir(privateHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(privateHome, uid, gid); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "project")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Scoped unit and backend cleanup remain parent-owned even if a child
	// crashes or reconnect fails part-way. The marker is root-controlled.
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/systemctl", "stop", unit).Run()
		body, err := os.ReadFile(filepath.Join(base, "owned-terminal.json"))
		if err == nil {
			var owned struct {
				Generation string `json:"generation"`
				Socket     string `json:"socket"`
			}
			if json.Unmarshal(body, &owned) == nil && validTmuxSessionName(owned.Generation) {
				_ = exec.Command("/usr/bin/tmux", "-S", owned.Socket, "kill-server").Run()
				_ = stopScopedTerminalBackend(owned.Generation)
			}
		}
	})
	launch := exec.Command("/usr/bin/systemd-run", "--quiet", "--collect",
		"--unit="+strings.TrimSuffix(unit, ".service"),
		"--property=KillMode=control-group", "--property=Type=simple",
		"--setenv=ASA_PTY_SCOPE_APPROVED_HOST="+host,
		"--setenv=ASA_PTY_SCOPE_CHILD_TEST_DIR="+base,
		executable, "-test.run=^TestApprovedWorkerScopeRealSystemdUnitRestart$", "-test.v")
	if out, err := launch.CombinedOutput(); err != nil {
		t.Fatalf("isolated broker service could not start: %v; %s", err, out)
	}
	first := waitApprovedScopeTestFile(base, "first-ready", 25*time.Second)
	if first != "ready" {
		t.Fatalf("real first broker session failed: %s", first)
	}
	firstPID, err := approvedScopeUnitPID(unit)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/usr/bin/systemctl", "restart", unit).Run(); err != nil {
		t.Fatalf("isolated broker systemd restart failed: %v", err)
	}
	second := waitApprovedScopeTestFile(base, "second-ready", 25*time.Second)
	if second != "recovered" {
		t.Fatalf("recovered second broker session failed: %s", second)
	}
	secondPID, err := approvedScopeUnitPID(unit)
	if err != nil {
		t.Fatal(err)
	}
	if firstPID <= 0 || secondPID <= 0 || firstPID == secondPID {
		t.Fatalf("no real service restart witnessed: first PID %d second PID %d", firstPID, secondPID)
	}
	t.Logf("real systemd service restart PASS: broker PID %d -> %d, unchanged authenticated PTY backend, post-recovery worker command", firstPID, secondPID)
}

func waitApprovedScopeTestFile(dir, name string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, "failure")); err == nil {
			return "CHILD_FAILURE: " + string(data)
		}
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(40 * time.Millisecond)
	}
	return "timed_out"
}

func approvedScopeUnitPID(unit string) (int, error) {
	out, err := exec.Command("/usr/bin/systemctl", "show", "--no-pager", "-p", "MainPID", "--value", unit).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func approvedScopeRestartChild(t *testing.T, dir string) {
	fail := func(err error) {
		_ = os.WriteFile(filepath.Join(dir, "failure"), []byte(err.Error()), 0600)
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || filepath.Base(dir) == "." ||
		!strings.HasPrefix(filepath.Base(dir), "asa-pty-real-restart-") {
		fail(errors.New("untrusted isolated root fixture directory"))
	}
	project := filepath.Join(dir, "project")
	state := filepath.Join(dir, "state")
	worker, err := user.Lookup("aiworker")
	if err != nil {
		fail(err)
	}
	uid, err := strconv.Atoi(worker.Uid)
	if err != nil {
		fail(err)
	}
	gid, err := strconv.Atoi(worker.Gid)
	if err != nil {
		fail(err)
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: project, StateDir: state, WorkerUser: "aiworker"},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(),
		audit:    audit.New(filepath.Join(state, "audit.jsonl")),
	}
	req := Request{PrincipalID: "disposable-restart-owner", PrincipalClass: "direct",
		Workspace: project, Columns: 80, Rows: 24, RequestID: "real-broker-service-restart"}
	marker := filepath.Join(dir, "owned-terminal.json")
	body, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		req.Action = "terminal_open"
		res := s.terminalAction(req)
		if !res.OK {
			fail(fmt.Errorf("before restart: %+v", res))
		}
		req.SessionID, req.SessionEpoch = res.SessionID, res.SessionEpoch
		e, err := s.sessions.get(req, res.SessionID)
		if err != nil {
			fail(err)
		}
		owned := struct {
			ID         string `json:"id"`
			Epoch      string `json:"epoch"`
			Generation string `json:"generation"`
			Socket     string `json:"socket"`
		}{res.SessionID, res.SessionEpoch, e.terminal.name, e.terminal.socket}
		payload, err := json.Marshal(owned)
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(marker, payload, 0600); err != nil {
			fail(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "first-ready"), []byte("ready"), 0600); err != nil {
			fail(err)
		}
	} else {
		if err != nil {
			fail(err)
		}
		var owned struct {
			ID         string `json:"id"`
			Epoch      string `json:"epoch"`
			Generation string `json:"generation"`
			Socket     string `json:"socket"`
		}
		if err := json.Unmarshal(body, &owned); err != nil || !validTerminalID(owned.ID) ||
			!validTmuxSessionName(owned.Generation) {
			fail(errors.New("untrusted stored terminal identity"))
		}
		req.Action = "terminal_reconnect"
		req.SessionID = owned.ID
		req.SessionEpoch = owned.Epoch
		res := s.terminalAction(req)
		if !res.OK || res.SessionID != owned.ID || res.SessionEpoch == owned.Epoch ||
			!res.RetentionTruncated {
			fail(fmt.Errorf("post-real-systemd-restart reconnect failed: %+v", res))
		}
		req.SessionEpoch = res.SessionEpoch
		req.Action = "terminal_write"
		req.Content = "id -u | sed s/^/SCOPED_RESTART_WORKER_UID:/" + "\n"
		if got := s.terminalAction(req); !got.OK {
			fail(fmt.Errorf("post-restart worker command failed: %+v", got))
		}
		req.Action = "terminal_read"
		req.Content = ""
		req.Limit = 8192
		var output strings.Builder
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			got := s.terminalAction(req)
			if !got.OK {
				fail(fmt.Errorf("post-restart terminal output failed: %+v", got))
			}
			req.Cursor = got.NextCursor
			raw, err := base64.StdEncoding.DecodeString(got.Output)
			if err != nil {
				fail(err)
			}
			output.Write(raw)
			if strings.Contains(output.String(), fmt.Sprintf("SCOPED_RESTART_WORKER_UID:%d", uid)) {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		if !strings.Contains(output.String(), fmt.Sprintf("SCOPED_RESTART_WORKER_UID:%d", uid)) {
			fail(fmt.Errorf("no worker process output after real restart: %q", output.String()))
		}
		if err := os.WriteFile(filepath.Join(dir, "second-ready"), []byte("recovered"), 0600); err != nil {
			fail(err)
		}
	}
	// Deliberately leave the broker process alive for the test-owned
	// systemd service to terminate/restart by KillMode=control-group.
	for {
		time.Sleep(time.Second)
	}
}
