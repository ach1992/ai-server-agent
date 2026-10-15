package executor

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

// Explicit operator-only source acceptance against real Node/vscode-php-debug,
// PHP 8.4 and Xdebug. Run on the approved nonproduction root Executor host;
// no resident Agent/Executor executable, socket or systemd service is altered.
// These fixture paths do NOT authorize production untrusted runtime selection.
func TestApprovedRootPHPCliDBGpBrokerCgroupEndToEnd(t *testing.T) {
	const hostname = "CLY581741"
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if host != hostname || os.Geteuid() != 0 || os.Getenv("ASA_ROOT_PHP_DAP_APPROVED_HOST") != hostname {
		t.Skip("requires explicitly approved root Executor on designated development host")
	}
	const proof = "/srv/ai-workspace/asa140-php-xdebug-proof"
	const helper = "/srv/ai-workspace/asa140-root-e2e-20261015/worker-helper"
	const base = "/srv/ai-workspace/asa140-root-e2e-20261015"
	const node = proof + "/toolchain/node-v24.18.1-linux-x64/bin/node"
	const adapter = proof + "/php-adapter-production-prototype/out/phpDebug.js"
	const php = "/usr/bin/php8.4"
	const xdebug = proof + "/extract/usr/lib/php/20240924/xdebug.so"
	for _, p := range []string{helper, node, adapter, php, xdebug, proof + "/dap_fixture.php"} {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("missing/untrusted test fixture %s: %v", p, err)
		}
	}
	account, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid == 0 {
		t.Fatalf("invalid worker identity: %v", err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp(base, "root-php-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(state); err != nil {
			t.Errorf("test state cleanup: %v", err)
		}
	})
	if err := os.Chmod(state, 0755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(state, "worker-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, uid, gid); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{WorkspaceDir: proof, StateDir: state},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(), runs: newRunLimiterWith(2, 1),
		workspaceHelperBinary: helper,
		phpDAPNodeBinary:      node, phpDAPCLIBinary: adapter,
		phpDAPNativeBinary: php, phpDAPXdebugModule: xdebug,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 52*time.Second)
	defer cancel()
	stat := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: proof, Path: "dap_fixture.php"})
	if !stat.OK || stat.FileVersion == "" {
		t.Fatalf("versioned worker PHP source stat failed: %+v", stat)
	}
	req := Request{Action: "debug_launch", Language: "php", DebugMode: "cli", DebugProgram: "dap_fixture.php",
		FileVersion: stat.FileVersion, Workspace: proof, DebugStopOnEntry: false,
		PrincipalID: "php-root-acceptance", PrincipalClass: "direct", RequestID: "php-root-proof"}
	start := s.debugAction(ctx, req)
	if !start.OK || start.SessionID == "" {
		t.Fatalf("real PHP/Xdebug launch failed: %+v", start)
	}
	id := start.SessionID
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := s.workerStdioClose(req, id); err != nil {
				t.Errorf("PHP root source cleanup uncertain: %v", err)
			}
		}
	})
	entry, err := s.sessions.get(req, id)
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	d, pid := entry.dap, entry.pid
	entry.mu.Unlock()
	if d == nil || d.containment == nil || !d.containment.verified || !d.containment.containsPID(pid) || d.phpProxy == nil {
		t.Fatal("real original Node/php adapter and private DBGp proxy not root-contained")
	}
	group, proxy := d.containment.path, d.phpProxy
	if !strings.Contains(group, "/ai-server-agent-executor.service/asa-dap-") ||
		!strings.Contains(proxy.dir, "/.asa-php-dap-") {
		t.Fatalf("unexpected proof identities cgroup=%s proxy=%s", group, proxy.dir)
	}
	action := func(method string, thread int) Response {
		q := req
		q.Action, q.SessionID, q.DebugMethod, q.DebugThreadID = "debug_action", id, method, thread
		return s.debugAction(ctx, q)
	}
	bp := req
	bp.Action, bp.SessionID, bp.DebugMethod = "debug_action", id, "breakpoints"
	bp.Path, bp.FileVersion, bp.DebugLines = "dap_fixture.php", stat.FileVersion, []int{3}
	if r := s.debugAction(ctx, bp); !r.OK {
		t.Fatalf("PHP exact source breakpoints failed: %+v", r)
	}
	if r := action("configure", 0); !r.OK {
		t.Fatalf("PHP Xdebug configure failed: %+v", r)
	}
	hit := false
	for until := time.Now().Add(13 * time.Second); time.Now().Before(until); {
		q := req
		q.Action, q.SessionID = "debug_status", id
		result := s.debugAction(ctx, q)
		if !result.OK {
			t.Fatalf("root PHP DAP status failed: %+v", result)
		}
		if strings.Contains(result.Output, "\"reason\":\"breakpoint\"") {
			hit = true
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if !hit {
		t.Fatal("real PHP breakpoint never reached through authenticated DBGp proxy")
	}
	if !proxy.verified {
		t.Fatal("DBGp original PHP client identity not positively proven")
	}
	threads := action("threads", 0)
	if !threads.OK || !strings.Contains(threads.Output, "threads") {
		t.Fatalf("PHP threads missing: %+v", threads)
	}
	stack := action("stackTrace", 1)
	if !stack.OK || !strings.Contains(stack.Output, "dap_fixture.php") {
		t.Fatalf("real PHP source stack missing: %+v", stack)
	}
	if r := action("next", 1); !r.OK {
		t.Fatalf("real Xdebug step failed: %+v", r)
	}
	stop := req
	stop.Action, stop.SessionID = "debug_stop", id
	outcome := s.debugAction(ctx, stop)
	if !outcome.OK {
		t.Fatalf("PHP source stop/owned-process cleanup failed: %+v", outcome)
	}
	closed = true
	for _, path := range []string{group, proxy.dir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("test-owned PHP cgroup/socket leaked %q err=%v", path, err)
		}
	}
	if _, err := s.sessions.get(req, id); err == nil {
		t.Fatal("closed PHP source debug session remains addressable")
	}
	t.Log("PASS real Xdebug/DBGp exact PHP source breakpoint+stack+step; independently verified private root-owned cgroup and socket cleanup")
}
