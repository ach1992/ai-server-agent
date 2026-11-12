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
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The executor is the single authority for Browser admission. Shared profile
// holders exclude other shared holders, independently isolated profiles occupy
// configurable bounded slots, and setup/cleanup exclude ALL Browser activity.
// Reservations survive uncertain process cleanup until the broker verifies an
// exact process-group stop. No queue or second Browser-process broker exists.
type browserSessionAdmission struct {
	mu                sync.Mutex
	holders           map[string]string // opaque reservation/session ID -> shared/isolated/maintenance
	isolatedHostUIDs  map[string]uint32 // unique host filesystem owner held until verified Browser cleanup
	sharedHolder      string
	maintenanceHolder string
}

func browserProfileMode(input string) (string, bool) {
	switch input {
	case "", "shared":
		return "shared", true
	case "isolated":
		return "isolated", true
	default:
		return "", false
	}
}

func browserReservationID() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return "browser_" + hex.EncodeToString(nonce[:]), nil
}

func (s *Server) browserConcurrentSlots() int {
	n := s.cfg.EffectiveRuntime().BrowserConcurrentSlots
	if n < 1 || n > 64 {
		return 2
	}
	return n
}

// beforeFirst runs while the admission mutex excludes every concurrent
// Browser launch. It is called only on an empty admission set, preventing
// cleanup of scratch directories owned by a live managed Browser peer.
func (a *browserSessionAdmission) tryMode(holder, mode string, limit int, beforeFirst func() error) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if holder == "" || limit < 1 || limit > 64 ||
		(mode != "shared" && mode != "isolated" && mode != "maintenance") ||
		a.maintenanceHolder != "" || a.holders[holder] != "" {
		return false, nil
	}
	if mode == "maintenance" {
		if len(a.holders) != 0 {
			return false, nil
		}
	} else {
		if len(a.holders) >= limit || (mode == "shared" && a.sharedHolder != "") {
			return false, nil
		}
	}
	if len(a.holders) == 0 && beforeFirst != nil {
		if err := beforeFirst(); err != nil {
			return false, err
		}
	}
	if a.holders == nil {
		a.holders = make(map[string]string)
	}
	if mode == "isolated" {
		if a.isolatedHostUIDs == nil {
			a.isolatedHostUIDs = make(map[string]uint32)
		}
		var uid uint32
		for candidate := browserSessionHostUIDBase; candidate < browserSessionHostUIDBase+browserSessionHostUIDSlots; candidate++ {
			occupied := false
			for _, current := range a.isolatedHostUIDs {
				if current == candidate {
					occupied = true
					break
				}
			}
			if !occupied {
				uid = candidate
				break
			}
		}
		if uid == 0 {
			return false, errors.New("no isolated Browser filesystem identity available")
		}
		a.isolatedHostUIDs[holder] = uid
	}
	a.holders[holder] = mode
	if mode == "shared" {
		a.sharedHolder = holder
	}
	if mode == "maintenance" {
		a.maintenanceHolder = holder
	}
	return true, nil
}

func (s *Server) browserAcquire(holder, mode string) (bool, error) {
	return s.browserAdmission.tryMode(holder, mode, s.browserConcurrentSlots(), s.browserReclaimScratch)
}

// Compatibility helpers are retained for existing shared-profile admission
// clients and tests; production one-shot runs use unique reservation IDs.
func (a *browserSessionAdmission) acquireRun() bool {
	return a.try("one-shot")
}
func (a *browserSessionAdmission) releaseRun() { a.release("one-shot") }
func (a *browserSessionAdmission) try(holder string) bool {
	ok, _ := a.tryMode(holder, "shared", 2, nil)
	return ok
}
func (a *browserSessionAdmission) transfer(from, to string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	mode := a.holders[from]
	if mode == "" || to == "" || a.holders[to] != "" {
		return false
	}
	delete(a.holders, from)
	a.holders[to] = mode
	if uid := a.isolatedHostUIDs[from]; uid != 0 {
		delete(a.isolatedHostUIDs, from)
		a.isolatedHostUIDs[to] = uid
	}
	if a.sharedHolder == from {
		a.sharedHolder = to
	}
	if a.maintenanceHolder == from {
		a.maintenanceHolder = to
	}
	return true
}

// Releasing a guessed, cross-session or already-retired identity cannot free
// another Browser's reservation, including after uncertain cleanup.
func (a *browserSessionAdmission) release(holder string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holders[holder] == "" {
		return false
	}
	delete(a.holders, holder)
	delete(a.isolatedHostUIDs, holder)
	if a.sharedHolder == holder {
		a.sharedHolder = ""
	}
	if a.maintenanceHolder == holder {
		a.maintenanceHolder = ""
	}
	return true
}
func (a *browserSessionAdmission) matches(holder string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holders[holder] != ""
}
func (a *browserSessionAdmission) reserved() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.holders) != 0
}

func (a *browserSessionAdmission) isolatedHostUID(holder string) uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isolatedHostUIDs[holder]
}

// A global-exclusive first admission reclaims only Agent-recognized
// disposable worker-owned scratch, using an anchored filesystem root. The
// scratch parent itself must remain the original, non-symlink directory.
// Other entries (including shared profiles) are NEVER traversed or removed.
func browserScratchName(name string) bool {
	prefix := ""
	for _, candidate := range []string{"asa-profile-", "asa-trace-", "run."} {
		if strings.HasPrefix(name, candidate) {
			prefix = candidate
			break
		}
	}
	if prefix == "" {
		return false
	}
	suffix := name[len(prefix):]
	if len(suffix) == 32 { // Executor-bound random session/run token
		for _, c := range suffix {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	if len(suffix) != 6 { // Legacy mktemp scratch names
		return false
	}
	for _, c := range suffix {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (s *Server) browserReclaimScratch() error {
	if err := s.browserReclaimScratchMatching(""); err != nil {
		return err
	}
	// The original worker scratch and dedicated Browser scratch are
	// independent ownership domains. Both are swept only when there are
	// no admitted Browsers, never across active sessions.
	if s.browserUID != 0 && s.browserUID != s.workerUID {
		return s.browserReclaimScratchAt(s.isolatedBrowserScratchDir(), 0, s.browserGID, "", true)
	}
	return nil
}

// Exact session/run scratch reclamation follows verified whole-cgroup stop.
// The private flag comes from the immutable session record, never the caller.
func (s *Server) browserReclaimNamedScratch(name string, private bool, hostUID uint32) error {
	if !browserScratchName(name) || !(strings.HasPrefix(name, "asa-profile-") && len(name) == len("asa-profile-")+32 ||
		strings.HasPrefix(name, "asa-trace-") && len(name) == len("asa-trace-")+32 ||
		strings.HasPrefix(name, "run.") && len(name) == len("run.")+32) {
		return errors.New("invalid Executor Browser scratch identity")
	}
	if s.cfg.StateDir == "" {
		return errors.New("Executor Browser scratch root unavailable")
	}
	if private {
		if !validBrowserSessionHostUID(hostUID) {
			return errors.New("unrecognized isolated Browser scratch owner")
		}
		return s.browserReclaimScratchAt(s.isolatedBrowserScratchDir(), hostUID, s.browserGID, name, true)
	}
	return s.browserReclaimScratchMatching(name)
}

func (s *Server) browserReclaimScratchMatching(exact string) error {
	if s.cfg.StateDir == "" {
		return nil // zero-value standalone admission tests
	}
	return s.browserReclaimScratchAt(filepath.Join(s.cfg.StateDir, "runtime", "browser", "tmp"), s.workerUID, s.workerGID, exact, false)
}

func (s *Server) browserReclaimScratchAt(dir string, expectedUID, expectedGID uint32, exact string, private bool) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && !private {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Browser scratch: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Browser scratch is not a real directory")
	}
	if private {
		// Root-controlled parent prevents a Browser or worker from swapping
		// the isolation root itself (including with a symlink).
		if err := trustedDir(filepath.Dir(dir)); err != nil {
			return fmt.Errorf("isolated Browser parent untrusted: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode().Perm() != 0711 || stat.Uid != 0 || stat.Gid != 0 {
			return errors.New("isolated Browser scratch root ownership changed")
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open Browser scratch: %w", err)
	}
	defer root.Close()
	anchor, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("inspect Browser scratch anchor: %w", err)
	}
	defer anchor.Close()
	anchorInfo, err := anchor.Stat()
	if err != nil || !os.SameFile(info, anchorInfo) {
		return errors.New("Browser scratch anchor changed during admission")
	}
	entries, err := anchor.ReadDir(513)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read Browser scratch: %w", err)
	}
	if len(entries) > 512 {
		return errors.New("Browser scratch reclamation needs operator reconciliation: over 512 entries")
	}
	for _, entry := range entries {
		if exact != "" && entry.Name() != exact {
			continue
		}
		if !entry.IsDir() || !browserScratchName(entry.Name()) {
			if exact != "" {
				return errors.New("exact Browser scratch is not a recognized directory")
			}
			continue
		}
		child, err := root.Lstat(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("verify Browser scratch candidate: %w", err)
		}
		stat, ok := child.Sys().(*syscall.Stat_t)
		validOwner := ok && stat.Uid == expectedUID
		if private {
			// Arbitrary same-session Playwright/Node code may legitimately
			// chmod its OWN profile. After proven whole-cgroup stop, exact
			// root cleanup must not strand its admission solely because
			// the owner changed directory mode during the session.
			validOwner = ok && validBrowserSessionHostUID(stat.Uid) &&
				stat.Gid == expectedGID &&
				(expectedUID == 0 || stat.Uid == expectedUID)
		}
		if !child.IsDir() || child.Mode()&os.ModeSymlink != 0 || !validOwner {
			if exact != "" {
				return errors.New("exact Browser scratch ownership is unverified")
			}
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("reclaim Browser scratch %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// Status discloses only aggregate admission state; no session or principal ID.
type BrowserAdmissionSnapshot struct {
	Active                int  `json:"active"`
	Limit                 int  `json:"limit"`
	SharedProfileReserved bool `json:"shared_profile_reserved"`
	MaintenanceReserved   bool `json:"maintenance_reserved"`
}

func (s *Server) browserAdmissionResponse() Response {
	s.browserAdmission.mu.Lock()
	snapshot := BrowserAdmissionSnapshot{
		Active: len(s.browserAdmission.holders), Limit: s.browserConcurrentSlots(),
		SharedProfileReserved: s.browserAdmission.sharedHolder != "",
		MaintenanceReserved:   s.browserAdmission.maintenanceHolder != "",
	}
	s.browserAdmission.mu.Unlock()
	status := "available"
	if snapshot.Active > 0 {
		status = "reserved"
	}
	return Response{OK: true, Status: status, BrowserAdmission: &snapshot}
}

type browserStdioState struct {
	mu                    sync.Mutex // single in-flight action / ordered stream cursor
	cursor                uint64
	partial               []byte
	uncertain             bool
	fitQualitySupported   bool   // advertised at ready by newer Agent-provided runners
	regionSupported       bool   // advertised at ready by region-aware runners
	imageStagingSupported bool   // optional <=1 MiB private per-session JPEG with pinned windows
	captureMaxBytes       int    // ready-advertised; older runners support only 32 KiB
	scriptMaxBytes        int    // ready-advertised; older runners support only 15 KiB
	credentialRef         string // isolated session derived from a protected service credential
	credentialAuthOK      bool   // deny further Browser actions after an unverified credential login
}

const (
	browserSessionMaxScript       = 110 << 10
	browserSessionMaxFrame        = 18 << 10
	browserSessionMaxCalls        = 128
	managedBrowserScriptBytes     = 64 << 10
	managedBrowserScriptWireBytes = 6*managedBrowserScriptBytes + 512 // worst-case JSON escape + envelope
)

func browserSessionError(code, class string, err error) Response {
	return fileError(code, class, err)
}
func browserSessionBusy() Response {
	return Response{Error: "Browser profile or concurrent process capacity busy", ReasonCode: "resource_limit", ErrorCode: "resource_limit", ErrorClass: "resource", Status: "busy", Retryable: true}
}
func (s *Server) browserSessionAction(ctx context.Context, req Request) Response {
	if req.Root || req.Approval || req.PrincipalID == "" || req.PrincipalClass == "" {
		return browserSessionError("browser_session_requires_worker_principal", "authorization", errors.New("authenticated unprivileged principal required"))
	}
	if gate := s.credentialSessionGate(req); !gate.OK {
		return gate
	}
	switch req.Action {
	case "browser_session_credential_login":
		return s.browserSessionCredentialLogin(ctx, req)
	case "browser_session_open":
		return s.browserSessionOpen(ctx, req)
	case "browser_session_flow":
		return s.browserSessionFlow(ctx, req)
	case "browser_session_script":
		return s.browserSessionScript(ctx, req)
	case "browser_session_capture":
		return s.browserSessionCapture(ctx, req)
	case "browser_session_trace":
		return s.browserSessionTrace(ctx, req)
	case "browser_session_image":
		return s.browserSessionImage(ctx, req)
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
// The unstarted broker's exact scratch could not be reclaimed after a
// failed process launch. Keep its host UID reserved until operator recovery
// rather than permitting a later Browser to inherit its old files.
var errBrowserFailedLaunchCleanupUnverified = errors.New("browser_failed_launch_cleanup_unverified")

func (s *Server) browserSessionOpen(ctx context.Context, req Request) Response {
	if req.SessionID != "" || len(req.Content) == 0 || len(req.Content) > browserSessionMaxScript || req.TimeoutMS != 0 {
		return browserSessionError("invalid_browser_session_open", "validation", errors.New("expected bounded runner script and no existing session"))
	}
	mode, valid := browserProfileMode(req.BrowserProfileMode)
	if !valid {
		return browserSessionError("invalid_browser_profile_mode", "validation", errors.New("profile mode must be shared or isolated"))
	}
	if req.BrowserSandbox && mode != "isolated" {
		return browserSessionError("browser_sandbox_requires_isolated_profile", "validation", errors.New("Chromium OS sandbox is only available for isolated managed Browser sessions"))
	}
	// CI's private fake-executable fixtures do not provision host accounts.
	// Installed Executor instances always have the configured Agent identity,
	// and MUST prove the dedicated Browser account/state before admission.
	if mode == "isolated" && s.cfg.AgentUser != "" {
		if _, err := s.isolatedBrowserHome(); err != nil {
			return browserSessionError("browser_isolation_unavailable", "security", err)
		}
	}
	reservation, nonceErr := browserReservationID()
	if nonceErr != nil {
		return browserSessionError("browser_admission_unavailable", "state", nonceErr)
	}
	granted, acquireErr := s.browserAcquire(reservation, mode)
	if acquireErr != nil {
		return browserSessionError("browser_scratch_reconciliation_failed", "state", acquireErr)
	}
	if !granted {
		return browserSessionBusy()
	}
	reserved := true
	defer func() {
		if reserved {
			s.browserAdmission.release(reservation)
		}
	}()
	hostUID := uint32(0)
	if mode == "isolated" && s.cfg.AgentUser != "" {
		hostUID = s.browserAdmission.isolatedHostUID(reservation)
		if err := s.verifyBrowserSessionHostUID(hostUID); err != nil {
			return browserSessionError("browser_isolation_unavailable", "security", err)
		}
	}
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
	// Production root Executor atomically places Node and Playwright's
	// independently grouped Chromium descendants inside one private cgroup.
	// Private fixture binary overrides do not run Chromium in ordinary CI.
	var group *dapCgroup
	if s.browserNodeBinary == "" {
		group, err = createBrowserCgroup(s.cfg.EffectiveExecutorServiceUnit())
		if err != nil {
			return browserSessionError("browser_containment_unavailable", "dependency", err)
		}
		group.beforeKillTestHook = s.browserKillTestHook
	}
	var id string
	if group != nil {
		id, err = s.workerContainedBrowserSession(req, req.Workspace, node, group, hostUID, args...)
		if id == "" {
			// The failed-start broker has already tried to verify private
			// cgroup stop and exact scratch cleanup. If either is unknown,
			// retain the admission/host-UID reservation, never reuse it.
			if errors.Is(err, errBrowserFailedLaunchCleanupUnverified) {
				reserved = false
				return browserSessionError("browser_failed_launch_cleanup_unverified", "state", err)
			}
			if cleanupErr := group.abortBeforeLaunch(); cleanupErr != nil {
				reserved = false // stay fail-closed until root recovery
				return browserSessionError("browser_containment_cleanup_unknown", "state", errors.Join(err, cleanupErr))
			}
		}
	} else {
		id, err = s.workerStdioSession(req, "browser", req.Workspace, node, args...)
	}
	if id != "" {
		// A child may have started even if the audit completion was degraded.
		// Hold the exact Browser capacity/profile reservation until verified stop.
		if !s.browserAdmission.transfer(reservation, id) {
			// The broker cannot safely admit another Browser when identity
			// transfer failed. Keep the original reservation fail-closed.
			reserved = false
			return browserSessionError("browser_session_lease_reconciliation_failed", "state", errors.New("cannot bind exact process to Browser admission"))
		}
		reserved = false
	}
	if err != nil {
		// A verified pre-launch kernel refusal is not an uncertain Browser
		// side effect. Surface unavailable user namespaces/cgroup support
		// directly; keep launched/ambiguous sessions on the existing
		// authenticated inspection and close path below.
		if id == "" && mode == "isolated" &&
			(errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) ||
				errors.Is(err, syscall.ENOSYS)) {
			return browserSessionError("browser_isolation_unavailable", "dependency",
				fmt.Errorf("isolated Browser user namespace or atomic cgroup launch unavailable: %w", err))
		}
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
	// A failure must never leave a falsely released reservation or a child
	// running after an unsuccessful sandbox request. Close uses the existing
	// verified whole-cgroup stop and exact scratch reclamation.
	failedStart := func(code, class, message string) Response {
		cleanup := s.browserSessionClose(Request{Action: "browser_session_close", PrincipalID: req.PrincipalID, PrincipalClass: req.PrincipalClass, Workspace: req.Workspace, SessionID: id, RequestID: req.RequestID})
		resp := browserSessionError(code, class, errors.New(message))
		if !cleanup.OK {
			resp.SessionID = id
			resp.Status = "cleanup_unverified"
		}
		return resp
	}
	if req.BrowserSandbox && err == nil && reply.Event == "startup_error" && reply.Reason == "browser_sandbox_unavailable" {
		return failedStart("browser_sandbox_unavailable", "dependency", "Chromium OS sandbox launch failed. Check unprivileged user namespaces, seccomp and the pinned Chromium binary's AppArmor userns policy; no unsandboxed retry was attempted")
	}
	if err != nil || reply.Event != "ready" {
		return failedStart("browser_session_start_failed", "runtime", "Browser worker did not report a bounded ready handshake")
	}
	entry.browser.mu.Lock()
	entry.browser.fitQualitySupported = reply.FitQuality
	entry.browser.regionSupported = reply.CaptureRegion
	entry.browser.imageStagingSupported = reply.ImageStaging
	entry.browser.captureMaxBytes = 32 << 10
	if reply.CaptureMaxBytes > 0 && reply.CaptureMaxBytes <= browserCaptureMaxBytes {
		entry.browser.captureMaxBytes = reply.CaptureMaxBytes
	}
	entry.browser.scriptMaxBytes = 15 << 10
	if reply.ScriptMaxBytes > 0 && reply.ScriptMaxBytes <= managedBrowserScriptBytes {
		entry.browser.scriptMaxBytes = reply.ScriptMaxBytes
	}
	entry.browser.mu.Unlock()
	if req.BrowserSandbox {
		if verifyErr := s.verifyManagedBrowserSandbox(entry); verifyErr != nil {
			return failedStart("browser_sandbox_unverified", "security", "Chromium OS sandbox enforcement could not be proven; check renderer seccomp-BPF, separate user/PID namespaces and host policy: "+verifyErr.Error())
		}
	}
	output := `{"scope":"authenticated_principal_and_workspace","refs":"cross_call_until_navigation_or_next_snapshot","reconnect":"same_executor_lifetime","ttl_seconds":3600}`
	if req.BrowserSandbox {
		output = `{"scope":"authenticated_principal_and_workspace","refs":"cross_call_until_navigation_or_next_snapshot","reconnect":"same_executor_lifetime","ttl_seconds":3600,"chromium_sandbox":"verified_at_start"}`
	}
	return Response{OK: true, Status: "ready", SessionID: id, OutputEncoding: "json", Output: output}
}

type browserSessionFrame struct {
	Event           string          `json:"event"`
	Nonce           string          `json:"nonce,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Mime            string          `json:"mime,omitempty"`
	FitQuality      bool            `json:"fit_quality,omitempty"`
	CaptureRegion   bool            `json:"capture_region,omitempty"`
	ImageStaging    bool            `json:"image_staging,omitempty"`
	CaptureMaxBytes int             `json:"capture_max_bytes,omitempty"`
	ScriptMaxBytes  int             `json:"script_max_bytes,omitempty"`
	Region          json.RawMessage `json:"region,omitempty"`
	Quality         json.RawMessage `json:"quality,omitempty"` // missing vs explicit zero/null matters for legacy compatibility
	Size            int             `json:"size,omitempty"`
	SHA256          string          `json:"sha256,omitempty"`
	Parts           int             `json:"parts,omitempty"`
	Index           int             `json:"index,omitempty"`
	Data            string          `json:"data,omitempty"`
	Offset          int64           `json:"offset,omitempty"`
	ChunkSHA256     string          `json:"chunk_sha256,omitempty"`
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
			if frame.Event != "ready" && (frame.Event != "startup_error" || frame.Reason != "browser_sandbox_unavailable") {
				return browserSessionFrame{}, true, errors.New("browser_session_not_ready")
			}
		} else if frame.Nonce != nonce || (frame.Event != "result" && frame.Event != "error" &&
			frame.Event != "capture_meta" && frame.Event != "capture_part" && frame.Event != "capture_done" &&
			frame.Event != "trace_meta" && frame.Event != "trace_chunk" && frame.Event != "trace_discarded" &&
			frame.Event != "image_meta" && frame.Event != "image_chunk" && frame.Event != "image_discarded") {
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
		result, err := s.sessions.read(req, id, state.cursor, largeStdioOutputBytes)
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

// browserSessionScript runs user-supplied Playwright JavaScript on the existing
// principal/workspace-bound page. The executable is the SAME managed Browser,
// not browser_run's independently created process/profile. Stdio framing,
// audit, session ownership, uncertainty and credential revocation are shared.
func (s *Server) browserSessionScript(ctx context.Context, req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	if strings.TrimSpace(req.Content) == "" || len(req.Content) > managedBrowserScriptBytes {
		return browserSessionError("invalid_browser_script", "validation", errors.New("nonempty Playwright script required within session frame budget"))
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if req.TimeoutMS == 0 {
		timeout = 90 * time.Second
	}
	if req.TimeoutMS < 0 || timeout > 5*time.Minute {
		return browserSessionError("invalid_timeout", "validation", errors.New("Browser script timeout maximum is 5 minutes"))
	}
	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("prior Browser action completion unverified; close this session"))
	}
	if len(req.Content) > state.scriptMaxBytes {
		r := browserSessionError("browser_script_capacity_unavailable", "compatibility", fmt.Errorf("installed Browser runner supports at most %d script bytes; reopen with the upgraded Agent/Executor runner or split the script", state.scriptMaxBytes))
		r.SessionID, r.Status = entry.id, "not_executed"
		return r
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return browserSessionError("browser_nonce_unavailable", "security", err)
	}
	nonce := hex.EncodeToString(random[:])
	payload, err := json.Marshal(struct {
		Type   string `json:"type"`
		Nonce  string `json:"nonce"`
		Script string `json:"script"`
	}{Type: "script", Nonce: nonce, Script: req.Content})
	if err != nil || len(payload)+1 > managedBrowserScriptWireBytes {
		return browserSessionError("input_too_large", "validation", errors.New("encoded Playwright script exceeds bounded 64 KiB script budget"))
	}
	// Preserve the shared 16 KiB broker write bound; split only this
	// prevalidated, ordered Browser line while holding its action lock.
	payload = append(payload, '\n')
	for len(payload) > 0 {
		n := min(len(payload), maxStdioInputBytes)
		if err := s.workerStdioWrite(req, entry.id, payload[:n]); err != nil {
			state.uncertain = true
			return browserSessionError("browser_session_action_uncertain", "state", fmt.Errorf("Playwright script delivery unverified: %w; close before retry", err))
		}
		payload = payload[n:]
	}
	frameCtx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	frame, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil || frame.Event != "result" || len(frame.Result) == 0 {
		state.uncertain = true
		return browserSessionError("browser_session_action_uncertain", "state", errors.New("Playwright script completion unknown; inspect and close rather than replaying side effects"))
	}
	var outcome struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(frame.Result, &outcome) != nil || outcome.OK == nil {
		state.uncertain = true
		return browserSessionError("browser_session_action_uncertain", "state", errors.New("invalid Playwright script result"))
	}
	resp := Response{OK: *outcome.OK, Status: "result", SessionID: entry.id, Output: string(frame.Result), OutputEncoding: "json", BytesSeen: int64(len(frame.Result)), BytesReturned: int64(len(frame.Result))}
	if !*outcome.OK {
		resp.Status = "failed"
		resp.ReasonCode = "browser_script_failed"
		resp.ErrorCode = "browser_script_failed"
		resp.ErrorClass = "action"
		resp.Error = "Playwright script failed after a known completion; inspect returned diagnostics; do not automatically replay mutations"
	}
	return resp
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
	// before releasing this Browser session's exact admission lease.
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
		// Retain the broker's actual failure stage for the authenticated
		// caller; a generic "cleanup unverified" message obscures whether
		// stop verification, child wait, or the prerequisite audit failed.
		// No session reservation is released until reconciliation succeeds.
		if closeErr != nil {
			return browserSessionError("browser_session_cleanup_unverified", "state",
				fmt.Errorf("Browser cleanup unverified; session reservation retained: %w", closeErr))
		}
		return browserSessionError("browser_session_cleanup_unverified", "state",
			errors.New("Browser cleanup unverified; session reservation retained; broker completion could not be confirmed"))
	}
	// The broker atomically reconciles the Browser lease BEFORE deleting
	// the clean entry. An audit completion failure cannot strand the lease.
	if closeErr != nil {
		return Response{SessionID: entry.id, Status: "closed_audit_degraded", ErrorCode: "audit_degraded", ReasonCode: "audit_degraded", ErrorClass: "audit", Error: "Browser process termination verified and profile released; audit completion degraded; privileged audit recovery still required"}
	}
	return Response{OK: true, Status: "closed", SessionID: entry.id}
}
