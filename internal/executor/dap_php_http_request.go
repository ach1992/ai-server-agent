package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// One explicit, authenticated, immutable-path PHP request. This action is
// side-effecting: never auto-retry after dispatch, even on timeout/cancel.
// Only 127.0.0.1 and the already pinned PHP listener are permitted; no user
// URL, DNS, redirects, non-GET body, proxy, cookie or response-content leak.
func (s *Server) debugPHPHTTPRequest(ctx context.Context, req Request, d *dapState) (response Response) {
	t := d.phpHTTP
	if t == nil || d.adapter != "php/xdebug" {
		return fileError("php_http_request_unsupported", "validation", errors.New("HTTP request requires PHP HTTP-mode DAP session"))
	}
	d.mu.Lock()
	configured := d.configured
	d.mu.Unlock()
	if err := t.proxy.terminalFailure(); err != nil {
		return fileError("php_http_dbgp_provenance_failed", "state", err)
	}
	if !configured {
		return fileError("debug_not_configured", "state", errors.New("set PHP breakpoints and call debug_configure before initiating one HTTP request"))
	}
	if err := t.verifyService(ctx, s.cfg.WorkerUser); err != nil {
		return fileError("php_http_service_drift", "state", err)
	}
	t.mu.Lock()
	if t.requestStarted {
		t.mu.Unlock()
		return fileError("php_http_request_already_dispatched", "state", errors.New("this debugger session already attempted its one HTTP request; inspect debug_status, never retry a potentially side-effecting request"))
	}
	t.mu.Unlock()
	jobID := t.serviceJobID()
	if jobID == "" {
		return fileError("php_http_service_drift", "state", errors.New("HTTP Service Job retired before request dispatch"))
	}
	auditObject := req.SessionID + "\x00job/" + jobID
	if blocked := s.beginActionAudit(req, "debug_http_request", "worker", auditObject, "developer_debugger"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	defer func() {
		response = s.finishActionAudit(req, "debug_http_request", "worker", auditObject, "developer_debugger", started, response)
	}()
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.port))
	targetURL := &url.URL{Scheme: "http", Host: host, Path: "/" + t.program}
	query := make(url.Values, 1)
	query.Set("XDEBUG_TRIGGER", t.trigger)
	targetURL.RawQuery = query.Encode()
	// Build all non-I/O input before marking potentially dispatched.
	requestCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		cancel()
		return fileError("php_http_invalid_request", "validation", err)
	}
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, MaxConnsPerHost: 1,
		MaxIdleConns: 0, ResponseHeaderTimeout: 25 * time.Second,
		MaxResponseHeaderBytes: 8 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != host {
				return nil, errors.New("php_http_refused_nonlocal_destination")
			}
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", host)
			if err != nil {
				return nil, err
			}
			// Re-prove original pinned PHP/listener identity after dialing and
			// before sending the side-effecting request bytes. A stopped or
			// replaced service must never become an arbitrary localhost GET.
			if err := t.verifyPHPIdentity(); err != nil {
				_ = conn.Close()
				return nil, errors.New("php_http_original_listener_changed_before_request")
			}
			if err := t.proxy.terminalFailure(); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("php_http_dbgp_provenance_failed_before_http_send: %w", err)
			}
			if err := t.bindAuthorizedRequestSocket(conn); err != nil {
				_ = conn.Close()
				return nil, err
			}
			if err := t.proxy.terminalFailure(); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("php_http_dbgp_provenance_failed_before_http_send: %w", err)
			}
			return conn, nil
		},
	}
	client := &http.Client{
		Timeout: 30 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // report redirect as failure; no second request
		},
	}
	if err := t.proxy.terminalFailure(); err != nil {
		cancel()
		transport.CloseIdleConnections()
		return fileError("php_http_dbgp_provenance_failed", "state", err)
	}
	if !t.markRequestDispatched(cancel) {
		cancel()
		transport.CloseIdleConnections()
		return fileError("php_http_request_already_dispatched", "state", errors.New("this session already dispatched its request"))
	}
	go func() {
		defer cancel()
		defer transport.CloseIdleConnections()
		// The test-only hook forces the unauthorized triggered request to
		// race after HTTP admission but before the authorized connection.
		if hook := t.beforeAuthorizedDialTestHook; hook != nil {
			hook()
		}
		if err := t.proxy.terminalFailure(); err != nil {
			t.mu.Lock()
			t.requestDone = true
			t.requestError = "php_http_dbgp_provenance_failed_before_http_send: " + err.Error()
			t.mu.Unlock()
			return
		}
		result, err := client.Do(httpRequest)
		code := 0
		status := ""
		if result != nil {
			code = result.StatusCode
			if result.Body != nil {
				_, readErr := io.Copy(io.Discard, result.Body)
				_ = result.Body.Close()
				if err == nil && readErr != nil {
					err = readErr
				}
			}
			if code < 200 || code >= 300 {
				status = "non_2xx_http_response"
			}
		}
		if err != nil {
			status = "http_request_completion_unverified"
		}
		if status == "" && t.proxy != nil {
			proofCtx, proofCancel := context.WithTimeout(context.Background(), 5*time.Second)
			proofErr := t.proxy.await(proofCtx)
			proofCancel()
			if proofErr != nil {
				status = "dbgp_peer_identity_not_verified"
			}
		}
		t.mu.Lock()
		t.requestDone = true
		t.requestError = status
		t.httpCode = code
		t.mu.Unlock()
	}()
	b, _ := json.Marshal(map[string]any{
		"http_request": "dispatched_once", "target": "verified_loopback_service",
		"may_execute_code": true, "retry_allowed": false, "http_port": t.port,
	})
	return Response{OK: true, SessionID: req.SessionID, Status: "request_dispatched",
		OutputEncoding: "json", Output: string(b), BytesSeen: int64(len(b)), BytesReturned: int64(len(b)),
		DurationMS: time.Since(started).Milliseconds()}
}

func (t *phpHTTPDebugTarget) markRequestDispatched(cancel context.CancelFunc) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.requestStarted {
		return false
	}
	t.requestStarted = true
	t.cancel = cancel
	return true
}

func (t *phpHTTPDebugTarget) requestSnapshot() map[string]any {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	result := map[string]any{"dispatched": t.requestStarted, "completed": t.requestDone}
	if t.proxy != nil {
		t.proxy.mu.Lock()
		result["dbgp_verified"] = t.proxy.verified && t.proxy.failure == nil
		if t.proxy.failure != nil {
			result["dbgp_provenance_error"] = t.proxy.failure.Error()
		}
		t.proxy.mu.Unlock()
	}
	if t.requestDone {
		result["http_status"] = t.httpCode
		if t.requestError != "" {
			result["result"] = t.requestError
		} else {
			result["result"] = "response_completed"
		}
	}
	return result
}

// DAP disconnect should normally let the PHP request resume. A bounded wait
// prevents leaving an orphaned client task. Cancellation only closes this
// request's HTTP connection and NEVER signals the Service Job or PHP process.
func (t *phpHTTPDebugTarget) finishHTTPRequestAfterDetach() {
	if t == nil {
		return
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		done := !t.requestStarted || t.requestDone
		t.mu.Unlock()
		if done {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
