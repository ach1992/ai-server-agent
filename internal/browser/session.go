package browser

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

type SessionOptions struct {
	Workspace         string
	SessionID         string
	Steps             []FlowStep
	TimeoutMS         int64
	IgnoreHTTPSErrors bool
}

// Managed sessions reuse the same pinned Browser runtime, shared persistent
// profile, Agent admission mutex, and executor-private stateful worker broker.
// Unlike browser_e2e, refs remain valid across calls until navigation,
// next snapshot, same-document identity change, close or expiry.
func (m *Manager) SessionOpen(ctx context.Context, opts SessionOptions) (executor.Response, error) {
	if opts.Workspace == "" || !filepath.IsAbs(opts.Workspace) || filepath.Clean(opts.Workspace) != opts.Workspace {
		return browserError("invalid_browser_workspace", "validation", "exact absolute workspace is required"), nil
	}
	if !m.mu.TryLock() {
		return browserBusy("Browser session open"), nil
	}
	defer m.mu.Unlock()
	if status := m.inspectStatus(ctx, true); !status.Ready {
		return browserError("browser_runtime_not_ready", "state", "pinned Browser runtime is not ready: "+status.Reason), nil
	}
	script, err := managedSessionRunner(m.engineDir(), filepath.Join(m.dataDir(), "profile"), filepath.Join(m.dataDir(), "tmp"), opts.IgnoreHTTPSErrors)
	if err != nil {
		return browserError("invalid_browser_session", "validation", err.Error()), nil
	}
	ctx, _, err = executor.EnsureRequestCorrelationContext(ctx)
	if err != nil {
		return browserUnknown("browser session correlation", err, false), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := executor.ClientCallContext(callCtx, m.cfg.ExecutorSocket, m.token, executor.Request{
		Action: "browser_session_open", Workspace: opts.Workspace, Content: script,
	})
	if err != nil {
		return browserUnknown("browser session open", err, false), nil
	}
	return resp, nil
}

func (m *Manager) SessionFlow(ctx context.Context, opts SessionOptions) (executor.Response, error) {
	if opts.SessionID == "" || opts.Workspace == "" {
		return browserError("invalid_browser_session", "validation", "workspace and session_id required"), nil
	}
	if _, err := flowScript(opts.Steps); err != nil {
		return browserError("invalid_browser_flow", "validation", err.Error()), nil
	}
	timeout, err := normalizeRunTimeout(opts.TimeoutMS)
	if err != nil {
		return browserError("invalid_timeout", "validation", err.Error()), nil
	}
	steps, err := json.Marshal(opts.Steps)
	if err != nil {
		return browserError("invalid_browser_flow", "validation", "cannot serialize Browser flow"), nil
	}
	if len(steps) > maxFlowInputBytes-128 {
		return browserError("invalid_browser_flow", "validation", "Browser step payload exceeds framed input budget"), nil
	}
	if !m.mu.TryLock() {
		return browserBusy("Browser session action"), nil
	}
	defer m.mu.Unlock()
	ctx, _, err = executor.EnsureRequestCorrelationContext(ctx)
	if err != nil {
		return browserUnknown("browser session action correlation", err, false), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout+browserExecutorClientGrace)
	defer cancel()
	resp, err := executor.ClientCallContext(callCtx, m.cfg.ExecutorSocket, m.token, executor.Request{
		Action: "browser_session_flow", Workspace: opts.Workspace, SessionID: opts.SessionID, Content: string(steps), TimeoutMS: int64(timeout / time.Millisecond),
	})
	if err != nil {
		return browserUnknown("browser session action", err, false), nil
	}
	return resp, nil
}

func (m *Manager) SessionStatus(ctx context.Context, opts SessionOptions) (executor.Response, error) {
	return m.sessionControl(ctx, opts, "browser_session_status")
}
func (m *Manager) SessionClose(ctx context.Context, opts SessionOptions) (executor.Response, error) {
	return m.sessionControl(ctx, opts, "browser_session_close")
}
func (m *Manager) sessionControl(ctx context.Context, opts SessionOptions, action string) (executor.Response, error) {
	if opts.SessionID == "" || opts.Workspace == "" {
		return browserError("invalid_browser_session", "validation", "workspace and session_id required"), nil
	}
	if action != "browser_session_close" && action != "browser_session_status" {
		return browserError("invalid_browser_session", "validation", "invalid control"), errors.New("invalid Browser session control")
	}
	if !m.mu.TryLock() {
		return browserBusy("Browser session control"), nil
	}
	defer m.mu.Unlock()
	var err error
	ctx, _, err = executor.EnsureRequestCorrelationContext(ctx)
	if err != nil {
		return browserUnknown("browser session control correlation", err, false), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := executor.ClientCallContext(callCtx, m.cfg.ExecutorSocket, m.token, executor.Request{
		Action: action, Workspace: opts.Workspace, SessionID: opts.SessionID,
	})
	if err != nil {
		return browserUnknown("browser session control", err, false), nil
	}
	return resp, nil
}
