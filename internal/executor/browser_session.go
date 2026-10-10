package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Browser profile admission is shared between one-shot browser_run/setup and
// the executor-owned Browser session consumer. The gate is held until verified
// pinned-process-group termination. Acquiring the lease never queues callers.
type browserSessionAdmission struct {
	mu     sync.Mutex
	holder string
}

func (a *browserSessionAdmission) acquireRun() bool { return a.try("one-shot") }
func (a *browserSessionAdmission) releaseRun()      { a.release("one-shot") }
func (a *browserSessionAdmission) try(holder string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holder != "" {
		return false
	}
	a.holder = holder
	return true
}
func (a *browserSessionAdmission) transfer(from, to string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holder != from {
		return false
	}
	a.holder = to
	return true
}

// release reports whether this exact owner held the lease. A Browser
// broker entry must not be discarded unless release was confirmed.
func (a *browserSessionAdmission) release(holder string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holder != holder {
		return false
	}
	a.holder = ""
	return true
}
func (a *browserSessionAdmission) matches(holder string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holder == holder
}
func (a *browserSessionAdmission) reserved() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holder != ""
}

type browserStdioState struct {
	mu        sync.Mutex // single in-flight action / ordered stream cursor
	cursor    uint64
	partial   []byte
	uncertain bool
}

const (
	browserSessionMaxScript = 110 << 10
	browserSessionMaxFrame  = 18 << 10
	browserSessionMaxCalls  = 128
)

func browserSessionError(code, class string, err error) Response {
	return fileError(code, class, err)
}
func browserSessionBusy() Response {
	return Response{Error: "shared Browser profile or session busy", ReasonCode: "resource_limit", ErrorCode: "resource_limit", ErrorClass: "resource", Status: "busy", Retryable: true}
}
func (s *Server) browserSessionAction(ctx context.Context, req Request) Response {
	if req.Root || req.Approval || req.PrincipalID == "" || req.PrincipalClass == "" {
		return browserSessionError("browser_session_requires_worker_principal", "authorization", errors.New("authenticated unprivileged principal required"))
	}
	switch req.Action {
	case "browser_session_open":
		return s.browserSessionOpen(ctx, req)
	case "browser_session_flow":
		return s.browserSessionFlow(ctx, req)
	case "browser_session_capture":
		return s.browserSessionCapture(ctx, req)
	case "browser_session_status":
		return s.browserSessionStatus(req)
	case "browser_session_close":
		return s.browserSessionClose(req)
	default:
		return browserSessionError("invalid_browser_session_action", "validation", errors.New("unknown Browser session action"))
	}
}

func (s *Server) browserSessionEntry(req Request) (*stdioSession, error) {
	entry, err := s.sessions.get(req, req.SessionID)
	if err != nil || entry.kind != "browser" || entry.browser == nil || !s.browserAdmission.matches(entry.id) {
		return nil, errSessionNotFound
	}
	return entry, nil
}

// No JS file is ever written to a project worktree, worker HOME, or shared
// Browser profile. The trusted Agent-provided script is supplied as bounded
// Node argv parts; Node imports an in-memory data: module. argv contains
// source code and managed paths only, never bearer credentials or page data.
func (s *Server) browserSessionOpen(ctx context.Context, req Request) Response {
	if req.SessionID != "" || len(req.Content) == 0 || len(req.Content) > browserSessionMaxScript || req.TimeoutMS != 0 {
		return browserSessionError("invalid_browser_session_open", "validation", errors.New("expected bounded runner script and no existing session"))
	}
	if !s.browserAdmission.try("opening") {
		return browserSessionBusy()
	}
	reserved := true
	defer func() {
		if reserved {
			s.browserAdmission.release("opening")
		}
	}()
	node := "/opt/ai-server-agent/browser/node/bin/node"
	if s.browserNodeBinary != "" { // private hermetic test fixture, never public input
		node = s.browserNodeBinary
	}
	info, err := os.Lstat(node)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return browserSessionError("browser_runtime_not_ready", "dependency", errors.New("pinned Browser Node executable unavailable"))
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	expectedUID := uint32(0)
	if s.browserNodeBinary != "" { // test fixtures are not deployed runtimes
		expectedUID = s.workerUID
	}
	if !ok || st.Uid != expectedUID || info.Mode().Perm()&0022 != 0 {
		return browserSessionError("browser_runtime_untrusted", "security", errors.New("Browser executable identity or permissions are unsafe"))
	}
	if !filepath.IsAbs(req.Workspace) || filepath.Clean(req.Workspace) != req.Workspace {
		return browserSessionError("invalid_browser_workspace", "validation", errors.New("exact absolute worker workspace required"))
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(req.Content))
	args := []string{"--input-type=module", "-e", "await import('data:text/javascript;base64,'+process.argv.slice(1).join(''));"}
	for len(encoded) > 0 {
		n := len(encoded)
		if n > 3800 {
			n = 3800
		}
		args = append(args, encoded[:n])
		encoded = encoded[n:]
	}
	if len(args) > 64 {
		return browserSessionError("input_too_large", "validation", errors.New("too many Browser argv fragments"))
	}
	id, err := s.workerStdioSession(req, "browser", req.Workspace, node, args...)
	if id != "" {
		// A child may have started even if the audit completion was degraded.
		// Hold the singleton profile reservation until explicit proof of stop.
		s.browserAdmission.transfer("opening", id)
		reserved = false
	}
	if err != nil {
		if id != "" {
			if entry, lookupErr := s.sessions.get(req, id); lookupErr == nil && entry.browser != nil {
				entry.browser.mu.Lock()
				entry.browser.uncertain = true // started, but audit completion is not trustworthy
				entry.browser.mu.Unlock()
			}
		}
		resp := browserSessionError("browser_session_start_uncertain", "runtime", errors.New("Browser launch or audit failed; inspect/close the returned session before retry"))
		resp.SessionID = id
		resp.Status = "uncertain"
		return resp
	}
	entry, err := s.sessions.get(req, id)
	if err != nil {
		return Response{SessionID: id, ErrorCode: "browser_session_start_uncertain", ErrorClass: "state", Error: "Browser session identity unavailable after start"}
	}
	// Browser-specific state was attached by the shared broker before
	// spawn; it MUST already exist, including after audit degradation.
	if entry.browser == nil {
		return browserSessionError("browser_session_start_uncertain", "state", errors.New("Browser reconciliation state missing"))
	}
	readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reply, err := s.browserSessionReadFrame(readyCtx, req, entry.browser, id, "")
	if err != nil || reply.Event != "ready" {
		// Never automatically retry launch, and do not release profile until
		// the exact process-group cleanup is proved.
		cleanup := s.browserSessionClose(Request{Action: "browser_session_close", PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, Workspace: req.Workspace, SessionID: id, RequestID: req.RequestID})
		resp := browserSessionError("browser_session_start_failed", "runtime", errors.New("Browser worker did not report a bounded ready handshake"))
		if !cleanup.OK {
			resp.SessionID = id
			resp.Status = "cleanup_unverified"
		}
		return resp
	}
	return Response{OK: true, Status: "ready", SessionID: id, OutputEncoding: "json", Output: `{"scope":"authenticated_principal_and_workspace","refs":"cross_call_until_navigation_or_next_snapshot","reconnect":"same_executor_lifetime","ttl_seconds":3600}`}
}

type browserSessionFrame struct {
	Event  string          `json:"event"`
	Nonce  string          `json:"nonce,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Mime   string          `json:"mime,omitempty"`
	Size   int             `json:"size,omitempty"`
	SHA256 string          `json:"sha256,omitempty"`
	Parts  int             `json:"parts,omitempty"`
	Index  int             `json:"index,omitempty"`
	Data   string          `json:"data,omitempty"`
}

// The same broker event ring/cursor used by LSP/DAP is the sole output owner.
// An incomplete, lost or mismatched frame is a terminal uncertain result: the
// caller must inspect/close rather than replay an action with side effects.
func (s *Server) browserSessionReadFrame(ctx context.Context, req Request, state *browserStdioState, id, nonce string) (browserSessionFrame, error) {
	parse := func() (browserSessionFrame, bool, error) {
		at := bytes.IndexByte(state.partial, '\n')
		if at < 0 {
			return browserSessionFrame{}, false, nil
		}
		line := append([]byte(nil), state.partial[:at]...)
		state.partial = append([]byte(nil), state.partial[at+1:]...)
		if !bytes.HasPrefix(line, []byte("ASA_BROWSER_SESSION ")) {
			return browserSessionFrame{}, true, errors.New("browser_session_invalid_frame")
		}
		var frame browserSessionFrame
		if err := json.Unmarshal(bytes.TrimPrefix(line, []byte("ASA_BROWSER_SESSION ")), &frame); err != nil {
			return browserSessionFrame{}, true, errors.New("browser_session_invalid_json")
		}
		if nonce == "" {
			if frame.Event != "ready" {
				return browserSessionFrame{}, true, errors.New("browser_session_not_ready")
			}
		} else if frame.Nonce != nonce || (frame.Event != "result" && frame.Event != "error" &&
			frame.Event != "capture_meta" && frame.Event != "capture_part" && frame.Event != "capture_done") {
			return browserSessionFrame{}, true, errors.New("browser_session_unexpected_result")
		}
		return frame, true, nil
	}
	for {
		if frame, found, err := parse(); found {
			return frame, err
		}
		if err := ctx.Err(); err != nil {
			return browserSessionFrame{}, err
		}
		result, err := s.sessions.read(req, id, state.cursor, maxStdioOutputBytes)
		if err != nil {
			return browserSessionFrame{}, err
		}
		if result.Truncated {
			return browserSessionFrame{}, errors.New("browser_session_output_lost")
		}
		for _, event := range result.Events {
			state.cursor = event.Sequence
			if event.Stream != "stdout" {
				continue
			}
			state.partial = append(state.partial, event.Data...)
			if len(state.partial) > browserSessionMaxFrame {
				return browserSessionFrame{}, errors.New("browser_session_output_too_large")
			}
			if frame, found, err := parse(); found {
				return frame, err
			}
		}
		if !result.Running {
			return browserSessionFrame{}, errors.New("browser_session_worker_exited")
		}
		select {
		case <-ctx.Done():
			return browserSessionFrame{}, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func (s *Server) browserSessionFlow(ctx context.Context, req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	if len(req.Content) == 0 || len(req.Content) > 16384 {
		return browserSessionError("invalid_browser_flow", "validation", errors.New("bounded flow JSON required"))
	}
	var steps []json.RawMessage
	if err := json.Unmarshal([]byte(req.Content), &steps); err != nil || len(steps) < 1 || len(steps) > 12 {
		return browserSessionError("invalid_browser_flow", "validation", errors.New("1..12 serialized steps required"))
	}
	canonicalSteps, err := json.Marshal(steps)
	if err != nil {
		return browserSessionError("invalid_browser_flow", "validation", errors.New("cannot canonicalize Browser action payload"))
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if req.TimeoutMS == 0 {
		timeout = 90 * time.Second
	}
	if req.TimeoutMS < 0 || timeout > 5*time.Minute {
		return browserSessionError("invalid_timeout", "validation", errors.New("Browser session action maximum is 5 minutes"))
	}
	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("prior Browser action completion unverified; close this session"))
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return browserSessionError("browser_nonce_unavailable", "security", err)
	}
	nonce := hex.EncodeToString(random[:])
	payload := fmt.Sprintf(`{"type":"flow","nonce":%q,"steps":%s}`+"\n", nonce, canonicalSteps)
	if len(payload) > 16384 {
		return browserSessionError("input_too_large", "validation", errors.New("Browser session action frame exceeds 16 KiB"))
	}
	if err := s.workerStdioWrite(req, entry.id, []byte(payload)); err != nil {
		state.uncertain = true
		return browserSessionError("browser_session_action_uncertain", "state", errors.New("Browser action delivery could not be proven; close session before retry"))
	}
	frameCtx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	frame, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil || frame.Event != "result" || len(frame.Result) == 0 {
		state.uncertain = true
		return browserSessionError("browser_session_action_uncertain", "state", errors.New("Browser action may have happened; inspect status and close before a new action"))
	}
	// A known Browser assertion/action failure is NOT an executor success.
	// Preserve the bounded flow details/session identity without poisoning
	// the session; only unknown completion makes further actions unsafe.
	var outcome struct {
		OK         *bool `json:"ok"`
		FailedStep *int  `json:"failed_step"`
	}
	if json.Unmarshal(frame.Result, &outcome) != nil || outcome.OK == nil ||
		(*outcome.OK && outcome.FailedStep != nil) || (!*outcome.OK && outcome.FailedStep == nil) {
		state.uncertain = true
		return browserSessionError("browser_session_action_uncertain", "state", errors.New("Browser result invalid; close before retry"))
	}
	response := Response{OK: *outcome.OK, Status: "result", SessionID: entry.id, Output: string(frame.Result), OutputEncoding: "json", BytesSeen: int64(len(frame.Result)), BytesReturned: int64(len(frame.Result))}
	if !*outcome.OK {
		response.Status = "failed"
		response.ReasonCode = "browser_flow_failed"
		response.ErrorCode = "browser_flow_failed"
		response.ErrorClass = "action"
		response.Error = "Browser flow action or assertion failed at a known step; inspect bounded result before continuing"
	}
	return response
}

func (s *Server) browserSessionStatus(req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	entry.mu.Lock()
	running := !entry.exited && !entry.closed
	cleanupUncertain := entry.cleanupErr != nil
	entry.mu.Unlock()
	if !entry.browser.mu.TryLock() {
		return Response{OK: true, Status: "busy", SessionID: entry.id, Running: &running}
	}
	uncertain := entry.browser.uncertain
	entry.browser.mu.Unlock()
	status := "running"
	if !running {
		status = "stopped"
	}
	if cleanupUncertain || uncertain {
		status = "uncertain"
	}
	return Response{OK: true, Status: status, SessionID: entry.id, Running: &running, OutputEncoding: "json"}
}

func (s *Server) browserSessionClose(req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	// Send the best-effort graceful shutdown request before broker-pinned
	// process-group termination. The whole child group must be proved dead
	// before releasing the single shared-profile admission lease.
	_ = s.workerStdioWrite(req, entry.id, []byte("{\"type\":\"close\"}\n"))
	// Allow Node/Playwright to run context.close(), which also terminates
	// Chromium's own children even if Playwright launched them in a new
	// process group. An immediate SIGTERM to Node can orphan that group.
	select {
	case <-entry.done:
	case <-time.After(3 * time.Second):
	}
	closeErr := s.workerStdioClose(req, entry.id)
	if !s.sessions.cleanlyRemoved(entry) {
		state.uncertain = true
		return browserSessionError("browser_session_cleanup_unverified", "state", errors.New("Browser worker stop was not proved; shared profile remains reserved"))
	}
	// The broker atomically reconciles the Browser lease BEFORE deleting
	// the clean entry. An audit completion failure cannot strand the lease.
	if closeErr != nil {
		return Response{SessionID: entry.id, Status: "closed_audit_degraded", ErrorCode: "audit_degraded", ReasonCode: "audit_degraded", ErrorClass: "audit", Error: "Browser process termination verified and profile released; audit completion degraded; privileged audit recovery still required"}
	}
	return Response{OK: true, Status: "closed", SessionID: entry.id}
}
