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

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
	"golang.org/x/sys/unix"
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
	base, err := os.MkdirTemp("/var/tmp", "asa-php-http-real-")
	if err != nil {
		t.Fatal(err)
	}
	// Register cleanup BEFORE systemd-run. A failed/ambiguous launch reply
	// can still have created a transient service; never erase its workspace
	// while its original unit may be running.
	unitID, claimPath := "", ""
	launchAttempted := false
	t.Cleanup(func() {
		reconciled := !launchAttempted
		if launchAttempted {
			// The real writer durably records its id before systemd-run.
			// Reconcile even when start_job returns an uncertain response.
			claim, found, claimErr := readJobClaim(claimPath)
			if claimErr != nil || !found || claim.PrincipalID != "php-http-root-e2e" ||
				claim.PrincipalClass != "direct" || claim.Workspace != filepath.Join(base, "workspace") ||
				claim.Kind != "service" || claim.Root || (unitID != "" && claim.JobID != unitID) {
				t.Errorf("test-owned start_job claim cannot be reconciled; preserving fixture at %s: found=%t err=%v", base, found, claimErr)
				return
			}
			unitID = claim.JobID
			if _, err := safeID(unitID); err != nil {
				t.Errorf("untrusted test-owned Job ID %q; preserving %s", unitID, base)
				return
			}
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
	// Copy the pinned test executable for the worker-only PHP fixture;
	// /var/tmp is disk-backed while the root test binary may live on /tmp.
	original, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	copyFile, err := os.OpenFile(workerTest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(copyFile, original)
	closeErr := copyFile.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy isolated worker fixture: %v, %v", copyErr, closeErr)
	}
	if err := os.Chmod(workerTest, 0755); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name   string
		mode   os.FileMode
		worker bool
	}{
		{"workspace", 0700, true}, {"state", 0755, false},
		{"state/worker-home", 0700, true}, {"state/jobs", 0711, false}, {"state/jobs/claims", 0700, false},
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
	owner := Request{PrincipalID: "php-http-root-e2e", PrincipalClass: "direct", Workspace: workspace}
	s := &Server{cfg: config.Config{WorkspaceDir: workspace, StateDir: state, WorkerUser: "aiworker"},
		guard: policy.New(nil), audit: audit.New(filepath.Join(base, "audit.jsonl")),
		workerUID: uint32(uid), workerGID: uint32(gid), sessions: newStdioSessionBroker(),
		runs: newRunLimiterWith(2, 1), workspaceHelperBinary: helper,
		phpDAPNodeBinary: node, phpDAPCLIBinary: adapter,
		phpDAPNativeBinary: php, phpDAPXdebugModule: xdebug}
	// Exercise the ACTUAL start_job claim writer and transient systemd runner.
	// A test-only TestMain handles the normal job-runner command using this
	// exact test executable. Only this new Job belongs to the fixture.
	operationID := "asa174-php-http-" + hex.EncodeToString(random[:8])
	claimPath = filepath.Join(state, "jobs", "claims", operationClaimName(operationID, owner.PrincipalID))
	parts := []string{
		"ASA_PHP_HTTP_WORKER_FIXTURE=1",
		"ASA_PHP_HTTP_FIXTURE_WORKSPACE=" + shellQuote(workspace),
		"ASA_PHP_HTTP_FIXTURE_TRIGGER=" + shellQuote(trigger),
		"ASA_PHP_HTTP_FIXTURE_PORT=" + strconv.Itoa(port),
		"ASA_PHP_HTTP_FIXTURE_SOCKET=" + shellQuote(socket),
		"ASA_PHP_HTTP_FIXTURE_PHP=" + shellQuote(php),
		"ASA_PHP_HTTP_FIXTURE_MODULE=" + shellQuote(xdebug),
		shellQuote(workerTest),
		"-test.run=^TestPHPHTTPServiceWorkerFixture$",
		"-test.timeout=90s",
	}
	startReq := owner
	startReq.Action, startReq.Kind, startReq.Label = "start_job", "service", "asa174-http"
	startReq.OperationID, startReq.Command = operationID, strings.Join(parts, " ")
	launchAttempted = true
	launched := s.startJob(startReq)
	if !launched.OK || launched.Status != "accepted" || launched.JobID == "" {
		t.Fatalf("isolated real start_job failed or ambiguous: %+v", launched)
	}
	jobID := launched.JobID
	unitID = jobID
	claim, found, err := readJobClaim(claimPath)
	if err != nil || !found || claim.JobID != jobID || claim.State != "started" ||
		claim.Kind != "service" || claim.PrincipalID != owner.PrincipalID ||
		claim.PrincipalClass != owner.PrincipalClass || claim.Workspace != workspace {
		t.Fatalf("real start_job protected claim writer unverified: found=%t claim=%+v err=%v", found, claim, err)
	}
	owner.JobID = jobID
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
	// Live root rejection checks must precede any adapter launch or HTTP
	// side effect. A known Job ID, UID or deterministic socket is not access.
	for _, tc := range []struct {
		name   string
		change func(*Request)
		code   string
	}{
		{"wrong_principal", func(r *Request) { r.PrincipalID = "other-principal" }, "php_http_service_unverified"},
		{"wrong_principal_class", func(r *Request) { r.PrincipalClass = "other" }, "php_http_service_unverified"},
		{"wrong_job_id", func(r *Request) { r.JobID = "999999999999999" }, "php_http_service_unverified"},
		{"wrong_port", func(r *Request) {
			if r.DebugHTTPPort == 65535 {
				r.DebugHTTPPort--
			} else {
				r.DebugHTTPPort++
			}
		}, "php_http_service_unverified"},
		{"wrong_trigger", func(r *Request) {
			r.DebugHTTPTrigger = strings.Repeat("0", 32)
			if r.DebugHTTPTrigger == trigger {
				r.DebugHTTPTrigger = strings.Repeat("1", 32)
			}
		}, "php_http_service_unverified"},
		{"wrong_source_version", func(r *Request) { r.FileVersion = "not-the-current-version" }, "debug_program_changed"},
	} {
		attempt := req
		tc.change(&attempt)
		if out := s.debugAction(ctx, attempt); out.OK || out.ErrorCode != tc.code || out.SessionID != "" {
			t.Fatalf("unsafe live HTTP service identity %s accepted or wrong error: %+v", tc.name, out)
		}
	}
	start := s.debugAction(ctx, req)
	if !start.OK || start.SessionID == "" {
		t.Fatalf("HTTP DAP failed to launch: %+v", start)
	}
	sid, closed := start.SessionID, false
	// The adapter must run in a dedicated Executor-owned cgroup, never in
	// the original verified PHP Service Job cgroup.
	entry, err := s.sessions.get(req, sid)
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	d := entry.dap
	if d == nil || d.phpHTTP == nil || d.phpHTTP.service == nil || d.containment == nil {
		entry.mu.Unlock()
		t.Fatal("independent HTTP service/adapter containment unavailable")
	}
	adapterPID, phpPID := d.adapterPID, d.phpHTTP.phpPID
	adapterCG, serviceCG := d.containment.path, d.phpHTTP.service.unit.cgroup
	entry.mu.Unlock()
	if adapterPID <= 1 || phpPID <= 1 || adapterCG == "" ||
		serviceCG == "" || strings.HasSuffix(adapterCG, serviceCG) {
		t.Fatal("DAP adapter is not independently pinned/contained")
	}
	adapterMember, adapterErr := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", adapterPID))
	phpMember, phpErr := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", phpPID))
	if adapterErr != nil || phpErr != nil ||
		strings.TrimSpace(string(adapterMember)) != "0::"+strings.TrimPrefix(adapterCG, "/sys/fs/cgroup") ||
		strings.TrimSpace(string(phpMember)) != "0::"+serviceCG {
		t.Fatalf("original PHP and DAP adapter cgroup isolation unverified: adapter=%q PHP=%q errors=%v,%v", adapterMember, phpMember, adapterErr, phpErr)
	}
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
	if replay := action("http_request", 0); replay.OK || replay.ErrorCode != "php_http_request_already_dispatched" {
		t.Fatalf("potentially side-effecting duplicate HTTP request was not rejected: %+v", replay)
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
	// Spoof: separate DAP session, no authorized HTTP trigger. A root-test
	// process may dial the root-owned AF_UNIX path, but its SO_PEERCRED UID/PID
	// must never be mistaken for the original worker-owned PHP process.
	spoof := s.debugAction(ctx, req)
	if !spoof.OK || spoof.SessionID == "" {
		t.Fatalf("isolated spoof-test DAP session unavailable: %+v", spoof)
	}
	spoofSID := spoof.SessionID
	spoofClosed := false
	t.Cleanup(func() {
		if !spoofClosed {
			if e := s.workerStdioClose(req, spoofSID); e != nil {
				t.Errorf("spoof-test DAP cleanup uncertain: %v", e)
			}
		}
	})
	peer, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatalf("spoofed root peer could not attempt its own test socket: %v", err)
	}
	_ = peer.Close()
	spoofEntry, err := s.sessions.get(req, spoofSID)
	if err != nil {
		t.Fatal(err)
	}
	spoofEntry.mu.Lock()
	spoofDAP := spoofEntry.dap
	spoofEntry.mu.Unlock()
	if spoofDAP == nil || spoofDAP.phpProxy == nil {
		t.Fatal("spoof-test DBGp provenance broker missing")
	}
	spoofCtx, spoofCancel := context.WithTimeout(ctx, 4*time.Second)
	spoofErr := spoofDAP.phpProxy.await(spoofCtx)
	spoofCancel()
	if spoofErr == nil || !strings.Contains(spoofErr.Error(), "php_http_dbgp_peer_is_not_original_service_php") {
		t.Fatalf("unverified live socket peer accepted or wrong provenance failure: %v", spoofErr)
	}
	spoofStop := req
	spoofStop.Action, spoofStop.SessionID = "debug_stop", spoofSID
	if out := s.debugAction(ctx, spoofStop); !out.OK {
		t.Fatalf("spoof-test adapter-only cleanup uncertain: %+v", out)
	}
	spoofClosed = true
	if active, err := jobUnitActive(jobID); err != nil || !active {
		t.Fatalf("spoof rejection changed independently owned service: %v %t", err, active)
	}
	// Crash ONLY this session's pinned Node adapter. The root-owned
	// adapter cgroup cleans the debug resources; the original Service must
	// remain independent and continue answering HTTP.
	crash := s.debugAction(ctx, req)
	if !crash.OK || crash.SessionID == "" {
		t.Fatalf("isolated crash-test DAP session unavailable: %+v", crash)
	}
	crashSID := crash.SessionID
	crashClosed := false
	t.Cleanup(func() {
		if !crashClosed {
			if e := s.workerStdioClose(req, crashSID); e != nil {
				t.Errorf("crash-test DAP cleanup uncertain: %v", e)
			}
		}
	})
	crashEntry, err := s.sessions.get(req, crashSID)
	if err != nil {
		t.Fatal(err)
	}
	crashEntry.mu.Lock()
	fd := -1
	if crashEntry.pidPin != nil {
		fd, err = unix.Dup(int(crashEntry.pidPin.Fd()))
	}
	crashEntry.mu.Unlock()
	if err != nil || fd < 0 {
		t.Fatalf("original crash-test adapter pin unavailable: %v", err)
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("signal test-owned pinned DAP adapter failed: %v", err)
	}
	_ = unix.Close(fd)
	adapterGone := false
	for until := time.Now().Add(7 * time.Second); time.Now().Before(until); {
		status := req
		status.Action, status.SessionID = "debug_status", crashSID
		out := s.debugAction(ctx, status)
		if !out.OK {
			t.Fatalf("crashed adapter status unavailable: %+v", out)
		}
		if strings.Contains(out.Output, "\"adapter_exited\":true") && strings.Contains(out.Output, "\"cleanup_verified\":true") {
			adapterGone = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !adapterGone {
		t.Fatal("unexpected adapter exit did not yield verified independent cgroup cleanup")
	}
	crashStop := req
	crashStop.Action, crashStop.SessionID = "debug_stop", crashSID
	if out := s.debugAction(ctx, crashStop); !out.OK {
		t.Fatalf("crashed adapter-only DAP cleanup failed: %+v", out)
	}
	crashClosed = true
	if active, err := jobUnitActive(jobID); err != nil || !active {
		t.Fatalf("adapter crash stopped original PHP Service: %v %t", err, active)
	}
	res, err = client.Get(endpoint)
	if err != nil {
		t.Fatalf("service unavailable after adapter crash: %v", err)
	}
	data, _ = io.ReadAll(io.LimitReader(res.Body, 64))
	_ = res.Body.Close()
	if string(data) != "42\n" {
		t.Fatalf("service changed after adapter crash: %q", data)
	}
	// Model real service drift: stop the independently authorized Job
	// while a fresh DAP adapter is configured but BEFORE any HTTP dispatch.
	// No request may reach a dead/replaced localhost listener; debugger
	// cleanup must succeed without misrepresenting service liveness.
	drift := s.debugAction(ctx, req)
	if !drift.OK || drift.SessionID == "" {
		t.Fatalf("isolated drift-test DAP session unavailable: %+v", drift)
	}
	driftSID, driftClosed := drift.SessionID, false
	t.Cleanup(func() {
		if !driftClosed {
			if e := s.workerStdioClose(req, driftSID); e != nil {
				t.Errorf("drift-test DAP cleanup uncertain: %v", e)
			}
		}
	})
	driftBP := bp
	driftBP.SessionID = driftSID
	if out := s.debugAction(ctx, driftBP); !out.OK {
		t.Fatalf("drift-test PHP breakpoint setup: %+v", out)
	}
	driftConfigure := req
	driftConfigure.Action, driftConfigure.SessionID, driftConfigure.DebugMethod = "debug_action", driftSID, "configure"
	if out := s.debugAction(ctx, driftConfigure); !out.OK {
		t.Fatalf("drift-test PHP configure: %+v", out)
	}
	// The separately authorized Job stop is the ONLY action that should
	// transition the Service from active to inactive.
	jobStop := owner
	jobStop.Action = "job_stop"
	if out := s.jobStop(jobStop); !out.OK || out.Status != "stopped" {
		t.Fatalf("explicit verified test-owned job_stop failed: %+v", out)
	}
	driftRequest := req
	driftRequest.Action, driftRequest.SessionID, driftRequest.DebugMethod = "debug_action", driftSID, "http_request"
	if out := s.debugAction(ctx, driftRequest); out.OK || out.ErrorCode != "php_http_service_drift" {
		t.Fatalf("dead/replaced PHP Service HTTP request not refused before dispatch: %+v", out)
	}
	driftStop := req
	driftStop.Action, driftStop.SessionID = "debug_stop", driftSID
	if out := s.debugAction(ctx, driftStop); out.OK || out.ErrorCode != "php_http_service_drift" ||
		out.Status != "cleanup_confirmed_service_liveness_unverified" {
		t.Fatalf("drifted Service adapter-only cleanup must distinguish service failure: %+v", out)
	}
	driftClosed = true
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("drifted Service debugger-owned Unix socket not removed: %v", err)
	}
	if active, err := jobUnitActive(jobID); err != nil || active {
		t.Fatalf("original PHP service was not stopped by explicit job_stop: %v %t", err, active)
	}
	stoppedClaim, found, err := readJobClaim(claimPath)
	if err != nil || !found || !stoppedClaim.VerifiedStopped {
		t.Fatalf("test-owned protected claim lacks verified stop: found=%t %+v %v", found, stoppedClaim, err)
	}
	if out := s.debugAction(ctx, req); out.OK || out.SessionID != "" || out.ErrorCode != "php_http_service_unverified" {
		t.Fatalf("stopped PHP Service Job was accepted as a live debug target: %+v", out)
	}
	t.Log("PHP_HTTP_ROOT_REAL_START_JOB_CLAIM_WRITER_DBGp_BREAKPOINT_SPOOF_CRASH_DRIFT_AND_EXPLICIT_STOP_PASS")
}

// Test-only shim: the real start_job writer launches os.Executable()
// as the original systemd service runner. Production uses the same
// RunJobHelper entry point in cmd/ai-server-agent/main.go.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "job-runner" {
		os.Exit(RunJobHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
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
