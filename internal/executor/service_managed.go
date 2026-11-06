package executor

import (
	"context"
	"encoding/json"
	"errors"
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
		case contains(err, "_mismatch", "_collision"):
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
			if err != nil || x.Instance == nil || !x.Instance.Managed || x.Instance.UUID != r.GuestUUID {
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
func (s *Server) managedEnvironmentAction(ctx context.Context, req Request) Response {
	if req.Root || req.PrincipalID == "" || req.PrincipalClass == "" {
		return fileError("authentication_required", "authorization", errors.New("authenticated MCP principal required"))
	}
	if s.cfg.InstanceID == "" {
		return fileError("host_identity_unavailable", "state", errors.New("Agent instance_id must be configured"))
	}
	in := req.ManagedEnv
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
	result, err := s.managedBackend().Run(ctx, in, req.Approval, secret)
	var resp Response
	if err != nil {
		resp = managedErr(err)
	} else if !result.Available && in.Operation != "status" {
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
