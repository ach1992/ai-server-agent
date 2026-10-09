package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

const maxCorrelationIDBytes = 128

type requestCorrelationContextKey struct{}

func WithNewRequestCorrelation(ctx context.Context) (context.Context, string, error) {
	id, err := newCorrelationID("req-")
	if err != nil {
		return ctx, "", err
	}
	return context.WithValue(ctx, requestCorrelationContextKey{}, id), id, nil
}

func RequestCorrelationID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestCorrelationContextKey{}).(string)
	return id, ok && id != ""
}

func EnsureRequestCorrelationContext(ctx context.Context) (context.Context, string, error) {
	if id, ok := RequestCorrelationID(ctx); ok {
		return ctx, id, nil
	}
	return WithNewRequestCorrelation(ctx)
}

func newCorrelationID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func auditActionName(req Request, fallback string) string {
	switch req.AuditAction {
	case "browser_setup", "browser_cleanup", "browser_run":
		return req.AuditAction
	default:
		return fallback
	}
}

func auditMode(root bool) string {
	if root {
		return "root"
	}
	return "worker"
}

func (s *Server) beginActionAudit(req Request, action, mode, command, policyCategory string) *Response {
	if s.audit == nil {
		return nil
	}
	decision := "allowed"
	if req.Approval {
		decision = "approved"
	}
	entry := audit.Entry{
		Phase:          "start",
		Action:         auditActionName(req, action),
		Mode:           mode,
		PolicyCategory: policyCategory,
		Decision:       decision,
		Command:        command,
		RequestID:      req.RequestID,
		OperationID:    req.OperationID,
		ApprovalID:     req.ApprovalID,
		JobID:          req.JobID,
		PrincipalID:    req.PrincipalID,
		PrincipalClass: req.PrincipalClass,
		PrincipalName:  req.PrincipalName,
	}
	if s.auditWriteHook != nil {
		if err := s.auditWriteHook("start"); err != nil {
			resp := auditUnavailableResponse(err)
			resp.RequestID = req.RequestID
			resp.OperationID = req.OperationID
			return &resp
		}
	}
	if err := s.audit.Write(entry); err != nil {
		resp := auditUnavailableResponse(err)
		resp.RequestID = req.RequestID
		resp.OperationID = req.OperationID
		return &resp
	}
	return nil
}

func (s *Server) finishActionAudit(req Request, action, mode, command, policyCategory string, started time.Time, resp Response) Response {
	resp.RequestID = req.RequestID
	resp.OperationID = req.OperationID
	if s.audit == nil {
		return resp
	}
	entry := audit.Entry{
		Phase:          "complete",
		Action:         auditActionName(req, action),
		Mode:           mode,
		PolicyCategory: policyCategory,
		Decision:       "executed",
		Success:        audit.Bool(resp.OK),
		ExitCode:       resp.ExitCode,
		DurationMS:     time.Since(started).Milliseconds(),
		ErrorClass:     resp.ErrorClass,
		ReasonCode:     resp.ReasonCode,
		Command:        command,
		RequestID:      req.RequestID,
		OperationID:    req.OperationID,
		ApprovalID:     req.ApprovalID,
		JobID:          firstNonEmpty(resp.JobID, req.JobID),
		PrincipalID:    req.PrincipalID,
		PrincipalClass: req.PrincipalClass,
		PrincipalName:  req.PrincipalName,
	}
	var auditErr error
	if s.auditWriteHook != nil {
		auditErr = s.auditWriteHook("complete")
	}
	if auditErr == nil {
		auditErr = s.audit.Write(entry)
	}
	if auditErr != nil {
		s.audit.MarkDegraded()
		resp.AuditDegraded = true
		resp.AuditError = "audit completion could not be durably recorded; further action-capable operations are blocked until the audit path is repaired and the executor is restarted"
	}
	return resp
}

func auditUnavailableResponse(err error) Response {
	detail := "required pre-action audit record could not be durably written; action was not started"
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		if len(msg) > 256 {
			msg = msg[:256]
		}
		if msg != "" {
			detail += ": " + msg
		}
	}
	return Response{
		Error:      detail,
		ReasonCode: "audit_unavailable",
		ErrorCode:  "audit_unavailable",
		ErrorClass: "audit",
		Status:     "degraded_safety",
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func validateCorrelationID(name, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxCorrelationIDBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxCorrelationIDBytes)
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' && r != ':' {
			return fmt.Errorf("%s contains unsupported characters", name)
		}
	}
	return nil
}

func ensureRequestCorrelation(req *Request) error {
	if req.RequestID == "" {
		id, err := newCorrelationID("req-")
		if err != nil {
			return fmt.Errorf("generate request correlation id: %w", err)
		}
		req.RequestID = id
	}
	if err := validateCorrelationID("request_id", req.RequestID); err != nil {
		return err
	}
	if !req.Approval {
		req.ApprovalID = ""
	} else if req.ApprovalID == "" {
		id, err := newCorrelationID("approval-")
		if err != nil {
			return fmt.Errorf("generate approval correlation id: %w", err)
		}
		req.ApprovalID = id
	}
	return validateCorrelationID("approval_id", req.ApprovalID)
}
