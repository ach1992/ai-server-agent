package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ach1992/ai-server-agent/internal/managedincus"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

func managedJSON(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		return fileError("managed_encode_failed", "state", err)
	}
	resp := Response{OK: true, Status: "observed", Output: string(b), OutputEncoding: "json", BytesSeen: int64(len(b)), BytesReturned: int64(len(b))}
	if native, ok := v.(managedincus.Result); ok {
		// The observation itself can succeed while its native readiness
		// probe timed out. Keep both facts visible at the top level.
		resp.TimedOut = native.TimedOut
	}
	return resp
}
func managedErr(err error) Response {
	// Classify the higher-level operation and recovery semantics first.
	// A wrapped native command is evidence, not grounds for converting an
	// uncertain create or storage preflight into an ordinary process failure.
	// Use the outer operation reason before inspecting nested native errors.
	// Only when that outer reason has no recognized semantic marker may a
	// lower-level error (for example direct incus_missing) classify it.
	code := "managed_operation_failed"
	if err != nil {
		primary := strings.SplitN(err.Error(), ":", 2)[0]
		code = managedReasonCode(primary)
		if code == "managed_operation_failed" {
			code = managedReasonCode(err.Error())
		}
	}
	natives := managedNativeFailures(err)
	hasNative := len(natives) > 0
	class := "managed"
	if hasNative && code == "managed_operation_failed" {
		code, class = "native_command_failed", "process"
	}
	resp := fileError(code, class, err)
	if hasNative {
		if code == "native_command_failed" {
			resp.Status = "native_command_failed"
		}
		// Primary launch error remains the top-level exit code. When both
		// launch and reconciliation fail, each stage retains its own exit
		// code, timeout and bounded diagnostic in the returned output.
		resp.ExitCode = natives[0].ExitCode
		resp.OutputEncoding = "utf-8"
		for _, native := range natives {
			resp.TimedOut = resp.TimedOut || native.TimedOut
			resp.BytesSeen += native.BytesSeen
			resp.OmittedBytes += native.OmittedBytes
			resp.Truncated = resp.Truncated || native.Truncated
		}
		if len(natives) == 1 {
			resp.Output = natives[0].Diagnostic
		} else {
			var stages []string
			for _, native := range natives {
				// Quoted text can grow sixfold. 4096 source bytes/stage
				// leaves both stage envelopes inside the 64-KiB cap.
				excerpt := native.Diagnostic
				if len(excerpt) > 4096 {
					excerpt = excerpt[:4096]
					for !utf8.ValidString(excerpt) {
						excerpt = excerpt[:len(excerpt)-1]
					}
					resp.OmittedBytes += int64(len(native.Diagnostic) - len(excerpt))
					resp.Truncated = true
				}
				stages = append(stages, fmt.Sprintf("native_stage=%q exit_code=%d timed_out=%t diagnostic=%q",
					native.Stage, native.ExitCode, native.TimedOut, excerpt))
			}
			resp.Output = strings.Join(stages, "\n")
		}
		resp.BytesReturned = int64(len(resp.Output))
	}
	return resp
}

// Classify only recognized Agent reason markers, never provider stderr.
func managedReasonCode(message string) string {
	switch {
	case strings.Contains(message, "_uncertain"), strings.Contains(message, "_unverified"):
		return "outcome_uncertain"
	case strings.Contains(message, "_missing"), strings.Contains(message, "not_found"):
		return "resource_missing"
	case strings.Contains(message, "_mismatch"), strings.Contains(message, "_collision"), strings.Contains(message, "_conflict"):
		return "identity_conflict"
	case strings.Contains(message, "_expired"):
		return "credential_expired"
	case strings.Contains(message, "_revoked"):
		return "credential_revoked"
	case strings.Contains(message, "approval_required"):
		return "approval_required"
	case strings.Contains(message, "_unavailable"), strings.Contains(message, "_unready"):
		return "dependency_unavailable"
	}
	return "managed_operation_failed"
}

// Traverse only the native error chain, stopping at each typed CLI failure
// so an errors.Join(launchErr, inspectErr) cannot discard the second stage.
func managedNativeFailures(err error) []*managedincus.NativeCommandError {
	var failures []*managedincus.NativeCommandError
	var visit func(error)
	visit = func(e error) {
		if e == nil || len(failures) >= 2 {
			return
		}
		if native, ok := e.(*managedincus.NativeCommandError); ok {
			failures = append(failures, native)
			return
		}
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, child := range multi.Unwrap() {
				visit(child)
			}
		} else if child := errors.Unwrap(e); child != nil {
			visit(child)
		}
	}
	visit(err)
	return failures
}

// Creation tags are descriptive, not a second admission policy for the
// already-authenticated root operator. Native project/name/UUID/host must
// still be matched against a fresh Incus observation.
func nativeGuestIdentityMatches(x *managedincus.Summary, uuid, project, guest, hostID string) bool {
	return x != nil && x.HostID == hostID && x.Project == project &&
		x.Name == guest && x.UUID != "" && x.UUID == uuid
}

func (s *Server) managedBackend() managedincus.Backend {
	return managedincus.Backend{HostID: s.cfg.InstanceID, StateDir: s.cfg.StateDir}
}
func (s *Server) serviceAccessAction(ctx context.Context, req Request) Response {
	if req.Root || req.PrincipalID == "" || req.PrincipalClass == "" {
		return fileError("authentication_required", "authorization", errors.New("authenticated MCP principal required"))
	}
	if s.cfg.InstanceID == "" {
		return fileError("host_identity_unavailable", "state", errors.New("Agent instance_id must be configured to bind target-service references"))
	}
	st := serviceaccess.New(s.cfg.StateDir)
	in := req.ServiceAccess
	started := time.Now()
	if blocked := s.beginActionAudit(req, "service_access", "root", "service_access "+in.Operation, "managed"); blocked != nil {
		return *blocked
	}
	var resp Response
	// Passive metadata never includes the stored secret. A bounded lazy sweep
	// removes expired secret material even when there is no scheduler.
	if in.Operation != "list" && in.Operation != "inspect" {
		if _, err := st.CleanupExpired(); err != nil {
			resp = managedErr(err)
			return s.finishActionAudit(req, "service_access", "root", "service_access "+in.Operation, "managed", started, resp)
		}
	}
	switch in.Operation {
	case "list", "inspect":
		records, err := st.List()
		if err != nil {
			resp = managedErr(err)
			break
		}
		if in.Operation == "inspect" {
			for _, v := range records {
				if v.ID == in.Ref {
					resp = managedJSON(v)
					break
				}
			}
			if !resp.OK {
				resp = managedErr(errors.New("service_reference_not_found"))
			}
		} else {
			resp = managedJSON(records)
		}
	case "register":
		r := in.Registration
		r.HostID = s.cfg.InstanceID // authenticated Agent/Executor identity, never caller-asserted.
		if r.Guest != "" {
			if r.Project == "" || r.GuestUUID == "" {
				resp = managedErr(errors.New("native_guest_identity_required"))
				break
			}
			ready := s.managedBackend().Ready(ctx)
			if !ready.Available {
				resp = managedErr(errors.New("native_guest_identity_unavailable: " + ready.Diagnostic))
				break
			}
			x, err := s.managedBackend().Run(ctx, managedincus.Request{Operation: "inspect", Project: r.Project, Instance: r.Guest}, false, "")
			if err != nil || !nativeGuestIdentityMatches(x.Instance, r.GuestUUID, r.Project, r.Guest, s.cfg.InstanceID) {
				resp = managedErr(errors.New("native_guest_identity_mismatch: inspect current local Incus project/instance/UUID"))
				break
			}
		}
		meta, err := st.Register(r)
		if err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(meta)
		}
	case "update":
		change := in.Registration
		change.HostID = s.cfg.InstanceID
		meta, err := st.Modify(in.Ref, change, req.Approval)
		if err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(meta)
		}
	case "generate":
		meta, err := st.Generate(in.Ref, in.Kind, in.Username, in.TTLDays, in.NeverExpires)
		if err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(meta)
		}
	case "renew":
		meta, err := st.Renew(in.Ref, in.TTLDays, in.NeverExpires)
		if err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(meta)
		}
	case "revoke":
		meta, err := st.Revoke(in.Ref)
		if err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(meta)
		}
	case "delete":
		if !req.Approval {
			resp = managedErr(errors.New("approval_required: explicitly authorize permanent deletion of this service reference"))
			break
		}
		if err := st.Delete(in.Ref); err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(map[string]any{"deleted": true, "ref": in.Ref})
		}
	case "cleanup":
		n, err := st.CleanupExpired()
		if err != nil {
			resp = managedErr(err)
		} else {
			resp = managedJSON(map[string]any{"expired_secrets_erased": n})
		}
	default:
		resp = managedErr(errors.New("unsupported service access operation"))
	}
	return s.finishActionAudit(req, "service_access", "root", "service_access "+in.Operation, "managed", started, resp)
}

// managedGuestExecFailure keeps the native diagnostic independently of the
// summarized Go error, without confusing a validation failure with a command
// actually dispatched to Incus.
func managedGuestExecFailure(result managedincus.Result, err error) Response {
	resp := managedErr(err)
	if !result.GuestExecAttempted {
		// Native identity/list/permission preflight failed before the
		// guest command: preserve its original stage/exit/stderr evidence.
		return resp
	}
	resp.TimedOut = resp.TimedOut || result.TimedOut
	resp.ExitCode = result.ExitCode
	resp.Output = result.Output
	resp.OutputEncoding = "utf-8"
	resp.BytesSeen = result.OutputBytesSeen
	resp.BytesReturned = int64(len(result.Output))
	if resp.BytesSeen < resp.BytesReturned {
		resp.BytesSeen = resp.BytesReturned
	}
	resp.OmittedBytes = resp.BytesSeen - resp.BytesReturned
	resp.Truncated = result.OutputTruncated
	switch {
	case strings.Contains(err.Error(), "native operation failed, exit_code="):
		resp.Status = "native_guest_exec_failed"
		resp.ErrorCode, resp.ReasonCode = "native_guest_exec_failed", "native_guest_exec_failed"
		resp.ErrorClass = "process"
	case strings.Contains(err.Error(), "guest_agent_unavailable"):
		resp.Status = "guest_agent_unavailable"
		resp.ErrorCode, resp.ReasonCode = "guest_agent_unavailable", "guest_agent_unavailable"
		resp.ErrorClass = "dependency"
	}
	return resp
}

// A failed native readiness query is a preflight failure, not evidence that
// the requested stateful operation was dispatched.
func managedUnavailableResult(result managedincus.Result) Response {
	resp := fileError("incus_unavailable", "dependency", errors.New(result.Diagnostic))
	resp.Status = "native_preflight_unavailable"
	resp.ExitCode = result.ExitCode
	resp.TimedOut = result.TimedOut
	resp.Output = result.Output
	resp.OutputEncoding = "utf-8"
	resp.BytesSeen = result.OutputBytesSeen
	resp.BytesReturned = int64(len(result.Output))
	resp.OmittedBytes = result.OutputOmittedBytes
	resp.Truncated = result.OutputTruncated
	return resp
}

func (s *Server) managedEnvironmentAction(ctx context.Context, req Request) Response {
	if req.Root || req.PrincipalID == "" || req.PrincipalClass == "" {
		return fileError("authentication_required", "authorization", errors.New("authenticated MCP principal required"))
	}
	if s.cfg.InstanceID == "" {
		return fileError("host_identity_unavailable", "state", errors.New("Agent instance_id must be configured"))
	}
	in := req.ManagedEnv
	in.ActorID = req.PrincipalID // server-authenticated only; never a client-provided operation field.
	started := time.Now()
	if blocked := s.beginActionAudit(req, "managed_environment", "root", "incus "+in.Operation, "managed"); blocked != nil {
		return *blocked
	}
	var secret string
	var preflightErr error
	if req.CredentialRef != "" {
		if in.Operation != "exec" {
			preflightErr = errors.New("credential reference is only supported for guest exec stdin")
		}
		if preflightErr == nil {
			rec, err := serviceaccess.New(s.cfg.StateDir).Resolve(req.CredentialRef, s.cfg.InstanceID)
			if err != nil {
				preflightErr = err
			} else if rec.Guest == "" || rec.Guest != in.Instance || rec.Project != in.Project || rec.GuestUUID != in.UUID {
				preflightErr = errors.New("credential_guest_identity_mismatch")
			} else {
				secret = rec.Secret
			}
		}
	}
	if preflightErr != nil {
		return s.finishActionAudit(req, "managed_environment", "root", "incus "+in.Operation, "managed", started, managedErr(preflightErr))
	}
	backend := s.managedBackend()
	result, err := backend.Run(ctx, in, req.Approval, secret)
	if result.Operation != nil &&
		(result.Operation.State == "running" || result.Operation.State == "submitted" || result.Operation.State == "outcome_uncertain") {
		s.watchNativeOperation(backend, *result.Operation)
	}
	var resp Response
	if err != nil {
		if in.Operation == "exec" {
			resp = managedGuestExecFailure(result, err)
		} else {
			resp = managedErr(err)
		}
	} else if !result.Available && in.Operation != "status" && in.Operation != "pending_creates" {
		resp = managedUnavailableResult(result)
	} else {
		resp = managedJSON(result)
	}
	if resp.OK && req.CredentialRef != "" && result.Status == "guest_command_completed" {
		// The credential never appears in guest stdout/stderr, command argv or
		// normal Agent output. Successful exec is not proof of guest login.
		_ = serviceaccess.New(s.cfg.StateDir).MarkUsed(req.CredentialRef)
	}
	return s.finishActionAudit(req, "managed_environment", "root", "incus "+in.Operation, "managed", started, resp)
}

// nativeWatchTicket is at most one pending observation per durable operation.
// The journal holds at most 256 records; keep only 32 active observer goroutines.
type nativeWatchTicket struct {
	operation managedincus.NativeOperation
	deadline  time.Time
}

const nativeWatchConcurrency = 32
const nativeWatchSlice = 2 * time.Second
const nativeWatchBudget = 2 * time.Hour

// watchNativeOperation admits *all* journal-backed pending operations. When 32
// observations are active, further operations queue instead of being silently
// dropped; short observation turns ensure a long-running operation cannot
// indefinitely starve a later accepted one.
func (s *Server) watchNativeOperation(b managedincus.Backend, op managedincus.NativeOperation) {
	s.managedWatchMu.Lock()
	defer s.managedWatchMu.Unlock()
	if s.managedWatching == nil {
		s.managedWatching = map[string]bool{}
	}
	if s.managedWatching[op.ID] {
		return
	}
	s.managedWatching[op.ID] = true
	s.managedWatchQueue = append(s.managedWatchQueue, nativeWatchTicket{
		operation: op,
		deadline:  time.Now().Add(nativeWatchBudget),
	})
	s.startManagedWatchersLocked(b)
}

// startManagedWatchersLocked must hold managedWatchMu; it starts only the
// bounded active set and makes every queued operation eligible when a turn
// finishes. The queue remains in Executor memory; the durable journal is
// recovered separately after restart.
func (s *Server) startManagedWatchersLocked(b managedincus.Backend) {
	for s.managedWatchActive < nativeWatchConcurrency && len(s.managedWatchQueue) > 0 {
		ticket := s.managedWatchQueue[0]
		s.managedWatchQueue[0] = nativeWatchTicket{}
		s.managedWatchQueue = s.managedWatchQueue[1:]
		s.managedWatchActive++
		go s.observeManagedWatchTurn(b, ticket)
	}
}

func (s *Server) observeManagedWatchTurn(b managedincus.Backend, ticket nativeWatchTicket) {
	op := ticket.operation
	ctx, cancel := context.WithTimeout(context.Background(), nativeWatchSlice)
	completed := false
	if s.managedWatchObserveTestHook != nil {
		completed = s.managedWatchObserveTestHook(ctx, op.ID)
	} else {
		completed = b.WatchNativeOperation(ctx, managedincus.Request{
			Operation: "operation_status", OperationID: op.ID, ActorID: op.ActorID,
			Project: op.Project, Instance: op.Instance, UUID: op.UUID,
		})
	}
	cancel()

	s.managedWatchMu.Lock()
	defer s.managedWatchMu.Unlock()
	s.managedWatchActive--
	if !completed && time.Now().Before(ticket.deadline) {
		s.managedWatchQueue = append(s.managedWatchQueue, ticket)
	} else {
		delete(s.managedWatching, op.ID)
		if !completed {
			// Unverified work remains in the root-only journal for manual
			// operation_status/Owner-approved resolution, never marked success.
			log.Printf("managed_incus_observer_budget_exhausted: operation_id=%s; native outcome remains unverified", op.ID)
		}
	}
	s.startManagedWatchersLocked(b)
}

// resumeNativeOperations reattaches polling immediately after an Executor
// restart, before Incus's short-lived native terminal operation may expire.
// Future ChatGPT conversations can also rediscover the same records.
func (s *Server) resumeNativeOperations() {
	backend := s.managedBackend()
	candidates, err := backend.RecoverableNativeOperations()
	if err != nil {
		log.Printf("managed_incus_recovery_error: unable to inspect root-private operation journal: %v", err)
		return
	}
	for _, op := range candidates {
		s.watchNativeOperation(backend, op)
	}
}
