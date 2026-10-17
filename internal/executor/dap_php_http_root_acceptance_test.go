package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

// Explicitly gated real integration: create only test-owned transient systemd
// service and root Executor-private DAP, never modify an installed Agent.
// This test must not be run on occupied hosts without separate Owner approval.
func TestApprovedRootPHPHTTPServiceOwnedDAP(t *testing.T) {
	const host = "CLY581741"
	actualHost, _ := os.Hostname()
	if os.Geteuid() != 0 || actualHost != host || os.Getenv("ASA_ROOT_PHP_HTTP_DAP_APPROVED_HOST") != host {
		t.Skip("requires separately approved isolated real-root PHP HTTP systemd acceptance")
	}
	const proof = "/srv/ai-workspace/asa140-php-xdebug-proof"
	const node = proof + "/toolchain/node-v24.18.1-linux-x64/bin/node"
	const adapter = proof + "/php-adapter-production-prototype/out/phpDebug.js"
	const php = "/usr/bin/php8.4"
	const xdebug = proof + "/extract/usr/lib/php/20240924/xdebug.so"
	const helper = "/srv/ai-workspace/asa140-root-e2e-20261015/worker-helper"
	for _, file := range []string{node, adapter, php, xdebug, helper} {
		fi, err := os.Lstat(file)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("missing original approved real PHP/Xdebug fixture %s: %v", file, err)
		}
	}
	account, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid64, e1 := strconv.ParseUint(account.Uid, 10, 32)
	gid64, e2 := strconv.ParseUint(account.Gid, 10, 32)
	if e1 != nil || e2 != nil || uid64 == 0 {
		t.Fatal("unverified worker UID/GID")
	}
	uid, gid := int(uid64), int(gid64)
	base, err := os.MkdirTemp("/tmp", "asa-php-http-real-")
	if err != nil {
		t.Fatal(err)
	}
	// Register cleanup BEFORE systemd-run. A failed/ambiguous launch reply
	// can still have created a transient service; never erase its workspace
	// while its original unit may be running.
	unitID := ""
	launchAttempted := false
	t.Cleanup(func() {
		reconciled := !launchAttempted
		if launchAttempted {
			exists, lookupErr := jobUnitExists(unitID)
			if lookupErr != nil {
				t.Errorf("isolated PHP HTTP unit existence unverified: %v", lookupErr)
			} else {
				if exists {
					cmd := exec.Command("/usr/bin/systemctl", "stop", "ai-job-"+unitID+".service")
					if output, stopErr := cmd.CombinedOutput(); stopErr != nil {
						t.Errorf("isolated PHP HTTP unit stop unverified: %v %.300s", stopErr, output)
					}
				}
				active, activeErr := jobUnitActive(unitID)
				if activeErr == nil && !active {
					reconciled = true
				} else {
					t.Errorf("isolated PHP HTTP unit still active or unverified: %v active=%t", activeErr, active)
				}
			}
		}
		if !reconciled {
			t.Errorf("preserving task-owned PHP HTTP fixture for reconciliation at %s", base)
			return
		}
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("test-owned isolated scratch cleanup: %v", err)
		}
	})
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workerTest := filepath.Join(base, "executor.test")
	// A hardlink preserves the exact original root test binary inode while
	// making it traversable/executable by the private worker systemd unit.
	if err := os.Link(self, workerTest); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name   string
		mode   os.FileMode
		worker bool
	}{
		{"workspace", 0700, true}, {"state", 0755, false},
		{"state/worker-home", 0700, true}, {"state/jobs", 0700, false}, {"state/jobs/claims", 0700, false},
	} {
		path := filepath.Join(base, item.name)
		if err := os.Mkdir(path, item.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, item.mode); err != nil {
			t.Fatal(err)
		}
		if item.worker {
			if err := os.Chown(path, uid, gid); err != nil {
				t.Fatal(err)
			}
		}
	}
	workspace, state := filepath.Join(base, "workspace"), filepath.Join(base, "state")
	home := filepath.Join(state, "worker-home")
	script := filepath.Join(workspace, "index.php")
	source := []byte("<?php\nfunction double(int $n): int {\n    $result = $n * 2;\n    return $result;\n}\necho double(21), \"\\n\";\n")
	if err := os.WriteFile(script, source, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(script, uid, gid); err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	trigger := hex.EncodeToString(random)
	socket, err := phpHTTPClientSocket(trigger)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	jobID := strconv.FormatInt(time.Now().UnixNano(), 10)
	if exists, e := jobUnitExists(jobID); e != nil || exists {
		t.Fatalf("isolated service ID collision: %v %t", e, exists)
	}
	unitID = jobID
	owner := Request{PrincipalID: "php-http-root-e2e", PrincipalClass: "direct", Workspace: workspace, JobID: jobID}
	claim := jobClaim{Version: 1, JobID: jobID, State: "started", Kind: "service",
		PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass,
		Workspace: workspace, Fingerprint: strings.Repeat("a", 64)}
	if err := createJobClaim(filepath.Join(state, "jobs", "claims", "root-php-http-fixture.json"), claim); err != nil {
		t.Fatal(err)
	}
	// The runner is a child of this exact, test-created unit. It runs the
	// pinned PHP executable with predeclared Xdebug socket+trigger.
	command := exec.Command("systemd-run", "--unit=ai-job-"+jobID, "--collect",
		"--property=WorkingDirectory="+workspace, "--uid=aiworker",
		"/usr/bin/env", "-i", "HOME="+home, "PATH=/usr/bin:/bin",
		"ASA_PHP_HTTP_WORKER_FIXTURE=1", "ASA_PHP_HTTP_FIXTURE_WORKSPACE="+workspace,
		"ASA_PHP_HTTP_FIXTURE_TRIGGER="+trigger, "ASA_PHP_HTTP_FIXTURE_PORT="+strconv.Itoa(port),
		"ASA_PHP_HTTP_FIXTURE_SOCKET="+socket, "ASA_PHP_HTTP_FIXTURE_PHP="+php,
		"ASA_PHP_HTTP_FIXTURE_MODULE="+xdebug,
		workerTest, "-test.run=^TestPHPHTTPServiceWorkerFixture$", "-test.timeout=90s")
	launchAttempted = true
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated worker unit start outcome uncertain; cleanup reconciles exact unit: %v %.500s", err, out)
	}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/index.php", port)
	ok := false
	for deadline := time.Now().Add(9 * time.Second); time.Now().Before(deadline); {
		res, err := client.Get(endpoint)
		if err == nil {
			data, _ := io.ReadAll(io.LimitReader(res.Body, 64))
			_ = res.Body.Close()
			if string(data) == "42\n" {
				ok = true
				break
			}
		}
		time.Sleep(70 * time.Millisecond)
	}
	if !ok {
		t.Fatal("original PHP Service Job did not serve an untriggered HTTP request")
	}
	s := &Server{cfg: config.Config{WorkspaceDir: workspace, StateDir: state, WorkerUser: "aiworker"},
		workerUID: uint32(uid), workerGID: uint32(gid), sessions: newStdioSessionBroker(),
		runs: newRunLimiterWith(2, 1), workspaceHelperBinary: helper,
		phpDAPNodeBinary: node, phpDAPCLIBinary: adapter,
		phpDAPNativeBinary: php, phpDAPXdebugModule: xdebug}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	stat := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: workspace, Path: "index.php"})
	if !stat.OK || stat.FileVersion == "" {
		t.Fatalf("source version unavailable: %+v", stat)
	}
	req := owner
	req.Action, req.Language, req.DebugMode = "debug_launch", "php", "http"
	req.DebugProgram, req.FileVersion = "index.php", stat.FileVersion
	req.DebugHTTPPort, req.DebugHTTPTrigger = port, trigger
	start := s.debugAction(ctx, req)
	if !start.OK || start.SessionID == "" {
		t.Fatalf("HTTP DAP failed to launch: %+v", start)
	}
	sid, closed := start.SessionID, false
	t.Cleanup(func() {
		if !closed {
			if err := s.workerStdioClose(req, sid); err != nil {
				t.Errorf("DAP cleanup unknown: %v", err)
			}
		}
	})
	action := func(method string, thread int) Response {
		r := req
		r.Action, r.SessionID, r.DebugMethod, r.DebugThreadID = "debug_action", sid, method, thread
		return s.debugAction(ctx, r)
	}
	bp := req
	bp.Action, bp.SessionID, bp.DebugMethod = "debug_action", sid, "breakpoints"
	bp.Path, bp.FileVersion, bp.DebugLines = "index.php", stat.FileVersion, []int{3}
	if got := s.debugAction(ctx, bp); !got.OK {
		t.Fatalf("PHP HTTP breakpoint: %+v", got)
	}
	if got := action("configure", 0); !got.OK {
		t.Fatalf("PHP HTTP configure: %+v", got)
	}
	if got := action("http_request", 0); !got.OK {
		t.Fatalf("PHP HTTP one-time request: %+v", got)
	}
	hit := false
	for deadline := time.Now().Add(14 * time.Second); time.Now().Before(deadline); {
		r := req
		r.Action, r.SessionID = "debug_status", sid
		out := s.debugAction(ctx, r)
		if !out.OK {
			t.Fatalf("PHP HTTP DAP status: %+v", out)
		}
		if strings.Contains(out.Output, "\"reason\":\"breakpoint\"") && strings.Contains(out.Output, "\"dbgp_verified\":true") {
			hit = true
			break
		}
		time.Sleep(70 * time.Millisecond)
	}
	if !hit {
		t.Fatal("no real HTTP DBGp source breakpoint with pinned peer")
	}
	if result := action("stackTrace", 1); !result.OK || !strings.Contains(result.Output, "index.php") {
		t.Fatalf("real PHP HTTP stack missing: %+v", result)
	}
	stop := req
	stop.Action, stop.SessionID = "debug_stop", sid
	if out := s.debugAction(ctx, stop); !out.OK {
		t.Fatalf("adapter-only DAP detach/cleanup: %+v", out)
	}
	closed = true
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("private socket not cleaned: %v", err)
	}
	if active, err := jobUnitActive(jobID); err != nil || !active {
		t.Fatalf("DAP stopped separately owned service: %v %t", err, active)
	}
	res, err := client.Get(endpoint)
	if err != nil {
		t.Fatalf("service unavailable after DAP stop: %v", err)
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64))
	_ = res.Body.Close()
	if string(data) != "42\n" {
		t.Fatalf("service response after DAP detach: %q", data)
	}
	t.Log("PHP_HTTP_ROOT_DBGp_BREAKPOINT_AND_SERVICE_JOB_SURVIVAL_PASS")
}

func TestPHPHTTPServiceWorkerFixture(t *testing.T) {
	if os.Getenv("ASA_PHP_HTTP_WORKER_FIXTURE") != "1" {
		t.Skip("separate root acceptance fixture only")
	}
	if os.Geteuid() == 0 {
		t.Fatal("worker PHP service cannot run as root")
	}
	php := os.Getenv("ASA_PHP_HTTP_FIXTURE_PHP")
	module := os.Getenv("ASA_PHP_HTTP_FIXTURE_MODULE")
	workspace := os.Getenv("ASA_PHP_HTTP_FIXTURE_WORKSPACE")
	token := os.Getenv("ASA_PHP_HTTP_FIXTURE_TRIGGER")
	socket := os.Getenv("ASA_PHP_HTTP_FIXTURE_SOCKET")
	port, err := strconv.Atoi(os.Getenv("ASA_PHP_HTTP_FIXTURE_PORT"))
	if err != nil || !validPHPHTTPTrigger(token) {
		t.Fatal("invalid PHP HTTP fixture")
	}
	cmd := exec.Command(php, expectedPHPHTTPArgv(php, module, workspace, port, token, socket)[1:]...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(60 * time.Second)
}
