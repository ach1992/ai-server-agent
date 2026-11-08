package executor

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/managedincus"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

func managedJSON(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		return fileError("managed_encode_failed", "state", err)
	}
	return Response{OK: true, Status: "observed", Output: string(b), OutputEncoding: "json", BytesSeen: int64(len(b)), BytesReturned: int64(len(b))}
}
func managedErr(err error) Response {
	code := "managed_operation_failed"
	if err != nil {
		switch {
		case contains(err, "_missing", "not_found"):
			code = "resource_missing"
		case contains(err, "_mismatch", "_collision", "_conflict"):
			code = "identity_conflict"
		case contains(err, "_expired"):
			code = "credential_expired"
		case contains(err, "_revoked"):
			code = "credential_revoked"
		case contains(err, "approval_required"):
			code = "approval_required"
		case contains(err, "_unavailable", "_unready"):
			code = "dependency_unavailable"
		case contains(err, "_uncertain", "_unverified"):
			code = "outcome_uncertain"
		}
	}
	return fileError(code, "managed", err)
}
func contains(err error, needles ...string) bool {
	if err == nil {
		return false
	}
	for _, s := range needles {
		if stringsContains(err.Error(), s) {
			return true
		}
	}
	return false
}
func stringsContains(s, sub string) bool {
	// kept separate to ensure native CLI stderr is not accidentally promoted
	// into an Error value or returned to a client.
	return len(sub) > 0 && strings.Contains(s, sub)
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
	resp.ExitCode = result.ExitCode
	resp.Output = result.Output
	resp.OutputEncoding = "utf-8"
	resp.BytesSeen = int64(len(result.Output))
	resp.BytesReturned = int64(len(result.Output))
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
		resp = fileError("incus_unavailable", "dependency", errors.New(result.Diagnostic))
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
