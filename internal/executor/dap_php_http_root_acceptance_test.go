package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	fallbackJobID, fallbackClaimPath := "", ""
	fallbackAttempted := false
	var fallbackOriginal phpHTTPServiceUnit
	var originalUnit phpHTTPServiceUnit // must be captured from live root manager, never guessed from name
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
				active, activeErr := jobUnitActive(unitID)
				if activeErr != nil {
					t.Errorf("isolated PHP HTTP unit activity unverified: %v", activeErr)
				} else if !active && !exists {
					reconciled = true
				} else if !active {
					// A loaded but inactive unit must not be stopped.
					reconciled = true
				} else if originalUnit.id == "" {
					t.Errorf("test-owned unit is active but its original InvocationID was never captured; refusing stop")
				} else if stopErr := stopOriginalPHPHTTPFixtureUnit(context.Background(), originalUnit, "aiworker", uint32(uid)); stopErr != nil {
					t.Errorf("test-owned original unit cannot be safely stopped: %v", stopErr)
				} else {
					stillActive, activeErr := jobUnitActive(unitID)
					if activeErr == nil && !stillActive {
						reconciled = true
					} else {
						t.Errorf("test-owned unit termination not confirmed: active=%t err=%v", stillActive, activeErr)
					}
				}
			}
		}
		if !reconciled {
			t.Errorf("preserving task-owned PHP HTTP fixture for reconciliation at %s", base)
			return
		}
		if fallbackAttempted {
			claim, found, err := readJobClaim(fallbackClaimPath)
			if err != nil || !found || claim.Kind != "service" || claim.Root ||
				claim.PrincipalID != "php-http-root-e2e" || claim.PrincipalClass != "direct" ||
				claim.Workspace != filepath.Join(base, "workspace") ||
				(fallbackJobID != "" && claim.JobID != fallbackJobID) {
				t.Errorf("second test-owned fallback claim cannot be reconciled; preserving %s: found=%t err=%v", base, found, err)
				return
			}
			fallbackJobID = claim.JobID
			if _, err := safeID(fallbackJobID); err != nil {
				t.Errorf("untrusted fallback Service Job ID, preserving %s: %v", base, err)
				return
			}
			active, err := jobUnitActive(fallbackJobID)
			if err != nil {
				t.Errorf("fallback Service unit activity unknown, preserving %s: %v", base, err)
				return
			}
			if active {
				if fallbackOriginal.id != fallbackJobID {
					t.Errorf("fallback Service lacks captured original identity; preserving %s", base)
					return
				}
				if err := stopOriginalPHPHTTPFixtureUnit(context.Background(), fallbackOriginal, "aiworker", uint32(uid)); err != nil {
					t.Errorf("fallback Service original runner cleanup uncertain, preserving %s: %v", base, err)
					return
				}
				active, err = jobUnitActive(fallbackJobID)
				if err != nil || active {
					t.Errorf("fallback Service remains active after targeted pin stop; preserving %s", base)
					return
				}
			}
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
	source := []byte(`<?php
function double(int $n): int {
    $result = $n * 2;
    return $result;
}
if (isset($_GET['XDEBUG_TRIGGER'])) {
    file_put_contents(__DIR__ . '/triggered-hits.log', "hit\n", FILE_APPEND);
}
echo double(21), "\n";
`)
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
	originalUnit, err = readPHPHTTPServiceUnit(context.Background(), jobID, "aiworker")
	if err != nil {
		t.Fatalf("capture original test-owned live InvocationID before fallback cleanup: %v", err)
	}
	// Discriminating real-systemd replacement/identity-drift guard: a
	// forged expectation must NEVER lead to a stop of this live Service.
	for _, change := range []struct {
		name    string
		replace func(*phpHTTPServiceUnit)
	}{
		{"InvocationID", func(u *phpHTTPServiceUnit) {
			u.invocation = strings.Repeat("0", 32)
			if u.invocation == originalUnit.invocation {
				u.invocation = strings.Repeat("1", 32)
			}
		}},
		{"MainPID", func(u *phpHTTPServiceUnit) { u.mainPID++ }},
		{"ControlGroup", func(u *phpHTTPServiceUnit) { u.cgroup += "-replacement" }},
	} {
		forged := originalUnit
		change.replace(&forged)
		if err := stopOriginalPHPHTTPFixtureUnit(context.Background(), forged, "aiworker", uint32(uid)); err == nil {
			t.Fatalf("fallback cleanup would stop a unit with drifted %s", change.name)
		}
		if active, err := jobUnitActive(jobID); err != nil || !active {
			t.Fatalf("%s identity drift guard stopped original unit: %v %t", change.name, err, active)
		}
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

	// The contender uses the SAME localhost source and XDEBUG_TRIGGER as the
	// authorized HTTP GET. Force it to run while requestStarted=true but
	// before the Agent even dials, so PID/fileuri/trigger alone would match.
	t.Run("competing_trigger_after_request_admission", func(t *testing.T) {
		attack := s.debugAction(ctx, req)
		if !attack.OK || attack.SessionID == "" {
			t.Fatalf("competing DAP launch: %+v", attack)
		}
		id := attack.SessionID
		cleaned := false
		defer func() {
			if !cleaned {
				if err := s.workerStdioClose(req, id); err != nil {
					t.Errorf("competing DAP cleanup: %v", err)
				}
			}
		}()
		entry, err := s.sessions.get(req, id)
		if err != nil {
			t.Fatal(err)
		}
		entry.mu.Lock()
		dp := entry.dap
		entry.mu.Unlock()
		if dp == nil || dp.phpProxy == nil || dp.phpHTTP == nil {
			t.Fatal("competing DBGp target missing")
		}
		attackBP := bp
		attackBP.SessionID = id
		if out := s.debugAction(ctx, attackBP); !out.OK {
			t.Fatalf("competing breakpoint: %+v", out)
		}
		config := req
		config.Action, config.SessionID, config.DebugMethod = "debug_action", id, "configure"
		if out := s.debugAction(ctx, config); !out.OK {
			t.Fatalf("competing configure: %+v", out)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		dp.phpHTTP.beforeAuthorizedDialTestHook = func() { close(entered); <-release }
		authorized := req
		authorized.Action, authorized.SessionID, authorized.DebugMethod = "debug_action", id, "http_request"
		if out := s.debugAction(ctx, authorized); !out.OK || out.Status != "request_dispatched" {
			t.Fatalf("authorized request did not enter pre-dial gate: %+v", out)
		}
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not reach deterministic pre-dial gate")
		}
		otherClient := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
		otherDone := make(chan struct{})
		go func() {
			defer close(otherDone)
			out, err := otherClient.Get(endpoint + "?XDEBUG_TRIGGER=" + trigger)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(out.Body, 256))
				_ = out.Body.Close()
			}
		}()
		const expectedFailure = "php_http_dbgp_no_active_authorized_request_socket"
		var refused error
		for until := time.Now().Add(4 * time.Second); time.Now().Before(until); {
			refused = dp.phpProxy.terminalFailure()
			if refused != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if refused == nil || !strings.Contains(refused.Error(), expectedFailure) {
			t.Fatalf("competing triggered local request was admitted or rejected for wrong reason: %v", refused)
		}
		close(release)
		aborted := false
		for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
			status := req
			status.Action, status.SessionID = "debug_status", id
			got := s.debugAction(ctx, status)
			if got.OK && strings.Contains(got.Output, "\"completed\":true") {
				if !strings.Contains(got.Output, "php_http_dbgp_provenance_failed_before_http_send") {
					t.Fatalf("Agent attempted HTTP after terminal broker failure: %+v", got)
				}
				aborted = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !aborted {
			t.Fatal("pre-dial authorized request was not suppressed")
		}
		dp.phpHTTP.mu.Lock()
		actualSocket := dp.phpHTTP.requestSocket
		dp.phpHTTP.mu.Unlock()
		if actualSocket != nil {
			t.Fatal("Agent opened request socket after rejected competing DBGp peer")
		}
		status := req
		status.Action, status.SessionID = "debug_status", id
		if got := s.debugAction(ctx, status); !got.OK ||
			!strings.Contains(got.Output, expectedFailure) ||
			!strings.Contains(got.Output, "dbgp_provenance_error") {
			t.Fatalf("debug_status concealed terminal provenance: %+v", got)
		}
		detach := req
		detach.Action, detach.SessionID = "debug_stop", id
		if out := s.debugAction(ctx, detach); !out.OK {
			t.Fatalf("competing-trigger adapter-only cleanup failed: %+v", out)
		}
		cleaned = true
		select {
		case <-otherDone:
		case <-time.After(5 * time.Second):
			t.Fatal("competing request did not settle")
		}
		if active, err := jobUnitActive(jobID); err != nil || !active {
			t.Fatalf("competing triggered request stopped original service: %v %t", err, active)
		}
	})
	// R2: a terminal broker failure recorded BEFORE HTTP admission must
	// never permit even a single side-effecting Agent GET.
	t.Run("terminal_provenance_blocks_http_dispatch", func(t *testing.T) {
		attack := s.debugAction(ctx, req)
		if !attack.OK || attack.SessionID == "" {
			t.Fatalf("R2 DAP launch: %+v", attack)
		}
		id, cleaned := attack.SessionID, false
		defer func() {
			if !cleaned {
				if err := s.workerStdioClose(req, id); err != nil {
					t.Errorf("R2 cleanup: %v", err)
				}
			}
		}()
		entry, err := s.sessions.get(req, id)
		if err != nil {
			t.Fatal(err)
		}
		entry.mu.Lock()
		dp := entry.dap
		entry.mu.Unlock()
		if dp == nil || dp.phpProxy == nil {
			t.Fatal("R2 proxy missing")
		}
		// A separate root test peer can connect but is NOT the pinned
		// worker-owned PHP process. Its terminal rejection is stable.
		other, err := net.DialTimeout("unix", socket, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = other.Close()
		var refusal error
		for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
			refusal = dp.phpProxy.terminalFailure()
			if refusal != nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		if refusal == nil {
			t.Fatal("R2 invalid peer did not fail terminally")
		}
		config := req
		config.Action, config.SessionID, config.DebugMethod = "debug_action", id, "configure"
		_ = s.debugAction(ctx, config)
		request := req
		request.Action, request.SessionID, request.DebugMethod = "debug_action", id, "http_request"
		if out := s.debugAction(ctx, request); out.OK ||
			out.ErrorCode != "php_http_dbgp_provenance_failed" ||
			!strings.Contains(out.Error, refusal.Error()) {
			t.Fatalf("known terminal peer failure did not block HTTP: %+v", out)
		}
		status := req
		status.Action, status.SessionID = "debug_status", id
		if out := s.debugAction(ctx, status); !out.OK ||
			!strings.Contains(out.Output, refusal.Error()) {
			t.Fatalf("debug_status lost original terminal error: %+v", out)
		}
		detach := req
		detach.Action, detach.SessionID = "debug_stop", id
		if out := s.debugAction(ctx, detach); !out.OK {
			t.Fatalf("R2 debugger-only cleanup failed: %+v", out)
		}
		cleaned = true
		if active, err := jobUnitActive(jobID); err != nil || !active {
			t.Fatalf("R2 terminal failure affected original Service: %v %t", err, active)
		}
	})
	// F1: terminal provenance arriving AFTER one-shot admission and after
	// the actual TCP dial must remain a precise "before-send" result. Force
	// the two independent DialContext health checks deterministically.
	t.Run("async_pre_send_provenance_classification", func(t *testing.T) {
		hitsPath := filepath.Join(workspace, "triggered-hits.log")
		for _, stage := range []string{"before_bind", "after_bind"} {
			t.Run(stage, func(t *testing.T) {
				before, err := os.ReadFile(hitsPath)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				attack := s.debugAction(ctx, req)
				if !attack.OK || attack.SessionID == "" {
					t.Fatalf("F1 DAP launch: %+v", attack)
				}
				id, cleaned := attack.SessionID, false
				defer func() {
					if !cleaned {
						if err := s.workerStdioClose(req, id); err != nil {
							t.Errorf("F1 DAP cleanup uncertain: %v", err)
						}
					}
				}()
				entry, err := s.sessions.get(req, id)
				if err != nil {
					t.Fatal(err)
				}
				entry.mu.Lock()
				dp := entry.dap
				entry.mu.Unlock()
				if dp == nil || dp.phpHTTP == nil || dp.phpProxy == nil {
					t.Fatal("F1 target/broker unavailable")
				}
				attackBP := bp
				attackBP.SessionID = id
				if out := s.debugAction(ctx, attackBP); !out.OK {
					t.Fatalf("F1 breakpoint setup: %+v", out)
				}
				config := req
				config.Action, config.SessionID, config.DebugMethod = "debug_action", id, "configure"
				if out := s.debugAction(ctx, config); !out.OK {
					t.Fatalf("F1 configure: %+v", out)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				dp.phpHTTP.afterAuthorizedDialTestHook = func(point string) {
					if point == stage {
						close(entered)
						<-release
					}
				}
				request := req
				request.Action, request.SessionID, request.DebugMethod = "debug_action", id, "http_request"
				if out := s.debugAction(ctx, request); !out.OK || out.Status != "request_dispatched" {
					t.Fatalf("F1 single-request admission: %+v", out)
				}
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("F1 request did not reach selected post-dial gate")
				}
				peer, err := net.DialTimeout("unix", socket, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = peer.Close()
				const cause = "php_http_dbgp_peer_is_not_original_service_php"
				var terminal error
				for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
					terminal = dp.phpProxy.terminalFailure()
					if terminal != nil {
						break
					}
					time.Sleep(15 * time.Millisecond)
				}
				if terminal == nil || !strings.Contains(terminal.Error(), cause) {
					t.Fatalf("F1 invalid peer was not rejected: %v", terminal)
				}
				close(release)
				verified := false
				for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
					status := req
					status.Action, status.SessionID = "debug_status", id
					out := s.debugAction(ctx, status)
					if !out.OK {
						t.Fatalf("F1 status: %+v", out)
					}
					var payload struct {
						HTTPRequest struct {
							Completed bool   `json:"completed"`
							Result    string `json:"result"`
							Failure   string `json:"dbgp_provenance_error"`
						} `json:"http_request"`
					}
					if err := json.Unmarshal([]byte(out.Output), &payload); err != nil {
						t.Fatalf("F1 status payload invalid: %v", err)
					}
					if payload.HTTPRequest.Completed {
						if payload.HTTPRequest.Result != errPHPHTTPProvenanceBeforeSend.Error() ||
							!strings.Contains(payload.HTTPRequest.Failure, cause) {
							t.Fatalf("F1 before-send classification/provenance lost: %+v", payload.HTTPRequest)
						}
						verified = true
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if !verified {
					t.Fatal("F1 async Dial refused no completed before-send status")
				}
				after, err := os.ReadFile(hitsPath)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatalf("F1 request reached PHP handler despite before-send refusal: before=%q after=%q", before, after)
				}
				stop := req
				stop.Action, stop.SessionID = "debug_stop", id
				if out := s.debugAction(ctx, stop); !out.OK {
					t.Fatalf("F1 adapter-only stop failed: %+v", out)
				}
				cleaned = true
				if active, err := jobUnitActive(jobID); err != nil || !active {
					t.Fatalf("F1 invalid peer stopped original PHP Service: active=%t err=%v", active, err)
				}
			})
		}
	})
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
	// F2: also exercise the positive fallback path on a SECOND completely
	// test-owned transient Service, after the primary PHP Job was stopped
	// through the separately authorized job_stop path. No installed service
	// or reusable name is ever passed to systemctl stop by fallback cleanup.
	t.Run("fallback_pidfd_stops_exact_original_systemd_service", func(t *testing.T) {
		fallbackOperation := "asa174-fallback-" + hex.EncodeToString(random[:8])
		fallbackReq := owner
		fallbackReq.Action, fallbackReq.Kind, fallbackReq.Label = "start_job", "service", "asa174-cleanup"
		fallbackReq.JobID = ""
		fallbackReq.OperationID, fallbackReq.Command = fallbackOperation, "exec /bin/sleep 60"
		fallbackClaimPath = filepath.Join(state, "jobs", "claims", operationClaimName(fallbackOperation, owner.PrincipalID))
		fallbackAttempted = true
		launched := s.startJob(fallbackReq)
		if !launched.OK || launched.JobID == "" || launched.Status != "accepted" {
			t.Fatalf("F2 isolated fallback service could not start: %+v", launched)
		}
		fallbackJobID = launched.JobID
		claim, found, err := readJobClaim(fallbackClaimPath)
		if err != nil || !found || claim.JobID != fallbackJobID || claim.State != "started" ||
			claim.Kind != "service" || claim.PrincipalID != owner.PrincipalID ||
			claim.PrincipalClass != owner.PrincipalClass || claim.Workspace != workspace {
			t.Fatalf("F2 separate test-owned service claim not durable: found=%t claim=%+v err=%v", found, claim, err)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			fallbackOriginal, err = readPHPHTTPServiceUnit(context.Background(), fallbackJobID, "aiworker")
			if err == nil {
				break
			}
			time.Sleep(35 * time.Millisecond)
		}
		if err != nil || fallbackOriginal.id != fallbackJobID {
			t.Fatalf("F2 actual original runner unit identity unavailable: %v %+v", err, fallbackOriginal)
		}
		if err := stopOriginalPHPHTTPFixtureUnit(context.Background(), fallbackOriginal, "aiworker", uint32(uid)); err != nil {
			t.Fatalf("F2 pin-scoped original service cleanup failed: %v", err)
		}
		if active, err := jobUnitActive(fallbackJobID); err != nil || active {
			t.Fatalf("F2 PIDFD cleanup did not retire test-owned systemd unit: active=%t err=%v", active, err)
		}
		t.Log("PHP_HTTP_ROOT_PIDFD_ONLY_FALLBACK_NO_NAME_STOP_PASS")
	})
	t.Log("PHP_HTTP_ROOT_REAL_START_JOB_CLAIM_WRITER_DBGp_BREAKPOINT_SPOOF_CRASH_DRIFT_AND_EXPLICIT_STOP_PASS")
}

// Fallback cleanup may target only the original, verified job runner via
// its pinned pidfd. A systemctl stop by the reusable unit *name* is unsafe:
// another actor might replace that name between validation and PID1 dispatch.
// The job runner's exit lets systemd retire its own transient Service cgroup.
// An ambiguous or replaced Service is retained for explicit reconciliation.
func stopOriginalPHPHTTPFixtureUnit(ctx context.Context, original phpHTTPServiceUnit, worker string, uid uint32) error {
	return stopOriginalPHPHTTPFixtureUnitWith(ctx, original, worker, uid, phpHTTPFixtureStopOps{
		read: readPHPHTTPServiceUnit, pin: verifyPHPHTTPServiceRunner, active: jobUnitActiveContext,
	})
}

// A narrow test seam substitutes read-only service observations and exact
// runner pinning. The production fixture uses only the real root proofs.
type phpHTTPFixtureStopOps struct {
	read            func(context.Context, string, string) (phpHTTPServiceUnit, error)
	pin             func(phpHTTPServiceUnit, uint32) (*os.File, error)
	active          func(context.Context, string) (bool, error)
	afterFinalCheck func() // test only: simulate unit-name replacement at the check/use boundary
}

func stopOriginalPHPHTTPFixtureUnitWith(ctx context.Context, original phpHTTPServiceUnit, worker string, uid uint32, ops phpHTTPFixtureStopOps) error {
	if original.id == "" || original.invocation == "" || uid == 0 ||
		ops.read == nil || ops.pin == nil || ops.active == nil {
		return fmt.Errorf("php_http_test_original_unit_identity_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, jobSystemdStopTimeout)
	defer cancel()
	live, err := ops.read(ctx, original.id, worker)
	if err != nil || live != original {
		return fmt.Errorf("php_http_test_unit_replaced_or_unverified: %v", err)
	}
	pidfd, err := ops.pin(live, uid)
	if err != nil {
		return fmt.Errorf("php_http_test_original_runner_unverified: %w", err)
	}
	defer pidfd.Close()
	current, err := ops.read(ctx, original.id, worker)
	if err != nil || current != original || unix.PidfdSendSignal(int(pidfd.Fd()), 0, nil, 0) != nil {
		return fmt.Errorf("php_http_test_unit_identity_changed_before_fallback_stop: %v", err)
	}
	// No unit-name stop here or afterward, even if the name now resolves to
	// another invocation. SIGTERM targets only the pinned original runner.
	if err := signalPinnedPHPHTTPFixtureRunner(ctx, pidfd, ops.afterFinalCheck); err != nil {
		return fmt.Errorf("php_http_test_original_runner_stop_unverified: %w", err)
	}
	for {
		active, err := ops.active(ctx, original.id)
		if err != nil {
			return fmt.Errorf("php_http_test_unit_postsignal_state_unverified: %w", err)
		}
		if !active {
			return nil // PID1 retired the original Service; never issue a name stop
		}
		live, err := ops.read(ctx, original.id, worker)
		if err == nil && live != original {
			return errors.New("php_http_test_replacement_unit_active_after_original_runner_stop")
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("php_http_test_original_unit_still_active_after_pinned_stop: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("php_http_test_original_unit_still_active_after_pinned_stop: %w", ctx.Err())
		case <-time.After(40 * time.Millisecond):
		}
	}
}

// A pidfd cannot retarget to a later process that reuses the numeric PID or
// systemd unit name. Poll for exit rather than treating signal delivery as a
// successful stop. No numeric kill or systemctl name fallback is permitted.
func signalPinnedPHPHTTPFixtureRunner(ctx context.Context, pin *os.File, afterFinalCheck func()) error {
	if pin == nil || pin.Fd() == ^uintptr(0) {
		return errors.New("php_http_test_original_runner_pidfd_missing")
	}
	fd := int(pin.Fd())
	if afterFinalCheck != nil {
		afterFinalCheck()
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil {
		return fmt.Errorf("php_http_test_original_pidfd_signal_failed: %w", err)
	}
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := unix.Poll(pfd, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("php_http_test_original_runner_exit_poll_failed: %w", err)
		}
		if pfd[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		if pfd[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return errors.New("php_http_test_original_runner_pidfd_poll_invalid")
		}
	}
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
