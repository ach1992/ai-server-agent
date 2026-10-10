package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	browserTraceMaxBytes  = 512 << 10
	browserTraceReadBytes = 8192
)

type browserTraceParams struct {
	Operation   string          `json:"operation"`
	Steps       json.RawMessage `json:"steps,omitempty"`
	Offset      int64           `json:"offset,omitempty"`
	FileVersion string          `json:"file_version,omitempty"`
}

// browserSessionTrace reuses the existing principal/workspace-bound worker,
// per-session mutex, nonce-bound framed stdout and Browser profile admission.
// A recording wraps one bounded flow; no unbounded cross-call recording or
// public artifact URL is created. Read is an 8 KiB version-pinned window only.
func (s *Server) browserSessionTrace(ctx context.Context, req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	if len(req.Content) == 0 || len(req.Content) > 16384 || req.TimeoutMS != 0 {
		return browserSessionError("invalid_browser_trace", "validation", errors.New("bounded trace parameters required"))
	}
	dec := json.NewDecoder(strings.NewReader(req.Content))
	dec.DisallowUnknownFields()
	var p browserTraceParams
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return browserSessionError("invalid_browser_trace", "validation", errors.New("invalid trace parameters"))
	}
	var steps []json.RawMessage
	switch p.Operation {
	case "record":
		if p.Offset != 0 || p.FileVersion != "" || json.Unmarshal(p.Steps, &steps) != nil || len(steps) < 1 || len(steps) > 12 {
			return browserSessionError("invalid_browser_trace", "validation", errors.New("record requires 1..12 steps and no offset/version"))
		}
		// Do not accept arbitrary encoded values disguised as flow steps.
		for _, step := range steps {
			if len(step) == 0 || step[0] != '{' || len(step) > 8192 {
				return browserSessionError("invalid_browser_trace", "validation", errors.New("invalid trace flow step"))
			}
		}
	case "read", "discard":
		if len(p.Steps) != 0 || p.Offset < 0 || p.Offset > browserTraceMaxBytes || len(p.FileVersion) != 71 || !strings.HasPrefix(p.FileVersion, "sha256:") {
			return browserSessionError("invalid_browser_trace", "validation", errors.New("read/discard require bounded offset and pinned SHA256"))
		}
		d, er := hex.DecodeString(strings.TrimPrefix(p.FileVersion, "sha256:"))
		if er != nil || hex.EncodeToString(d) != strings.TrimPrefix(p.FileVersion, "sha256:") || (p.Operation == "discard" && p.Offset != 0) {
			return browserSessionError("invalid_browser_trace", "validation", errors.New("invalid/stale trace version format or discard offset"))
		}
	default:
		return browserSessionError("invalid_browser_trace", "validation", errors.New("operation must be record, read or discard"))
	}

	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("previous Browser action unverified; close session"))
	}
	uncertain := func() Response {
		state.uncertain = true
		r := browserSessionError("browser_session_action_uncertain", "state", errors.New("trace action/transfer completion or cleanup could not be proved; close session; never replay flow"))
		r.SessionID = entry.id
		r.Status = "uncertain"
		return r
	}
	var rnd [16]byte
	if _, err = rand.Read(rnd[:]); err != nil {
		return browserSessionError("browser_nonce_unavailable", "security", err)
	}
	nonce := hex.EncodeToString(rnd[:])
	command := map[string]any{"nonce": nonce}
	switch p.Operation {
	case "record":
		command["type"] = "trace_record"
		command["steps"] = steps
	case "read":
		command["type"] = "trace_read"
		command["offset"] = p.Offset
		command["file_version"] = p.FileVersion
	case "discard":
		command["type"] = "trace_discard"
		command["file_version"] = p.FileVersion
	}
	raw, err := json.Marshal(command)
	if err != nil || len(raw) > 16384 {
		return browserSessionError("invalid_browser_trace", "validation", errors.New("trace frame exceeds input limit"))
	}
	if err = s.workerStdioWrite(req, entry.id, append(raw, '\n')); err != nil {
		return uncertain()
	}
	timeout := 8 * time.Second
	if p.Operation == "record" {
		timeout = 35 * time.Second
	}
	frameCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	frame, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil {
		return uncertain()
	}
	if frame.Event == "error" {
		switch frame.Reason {
		case "trace_unavailable", "trace_stale", "trace_invalid_offset":
			// These known lookup refusals are valid only for data retrieval
			// and explicit retirement. During record, Browser actions may
			// already have executed and a lookup-shaped error is unknown.
			if p.Operation == "record" {
				return uncertain()
			}
			r := browserSessionError(frame.Reason, "state", fmt.Errorf("trace %s; record or use the exact SHA256 and offset", frame.Reason))
			r.SessionID = entry.id
			r.Status = "not_delivered"
			return r
		case "trace_too_large":
			if p.Operation != "record" {
				return uncertain()
			}
			// This is a known size refusal ONLY when the worker returned a
			// valid completed flow result. Unknown action completion is not safe
			// to retry or report as an ordinary resource failure.
			if !validBrowserTraceFlowResult(frame.Result) {
				return uncertain()
			}
			// Flow steps were already executed; do NOT imply that retrying
			// record is idempotent or that a ZIP is available to read.
			r := browserSessionError("too_large", "resource", errors.New("flow executed; resulting trace ZIP exceeded 512 KiB; no artifact retained; do not blindly replay actions"))
			r.SessionID = entry.id
			r.Status = "flow_executed_trace_unavailable"
			r.Output = string(frame.Result)
			r.OutputEncoding = "json"
			return r
		default:
			return uncertain()
		}
	}
	if p.Operation == "discard" {
		if frame.Event != "trace_discarded" {
			return uncertain()
		}
		return Response{OK: true, Status: "discarded", SessionID: entry.id}
	}
	if p.Operation == "record" {
		if frame.Event != "trace_meta" || frame.Mime != "application/zip" || frame.Size < 4 || frame.Size > browserTraceMaxBytes ||
			!validTraceDigest(frame.SHA256) || len(frame.Result) == 0 || len(frame.Result) > 16000 {
			return uncertain()
		}
		if !validBrowserTraceFlowResult(frame.Result) {
			return uncertain()
		}
		var outcome struct {
			OK bool `json:"ok"`
		}
		_ = json.Unmarshal(frame.Result, &outcome)
		size := int64(frame.Size)
		r := Response{OK: outcome.OK, Status: "recorded", SessionID: entry.id,
			MIMEType: "application/zip", FileSize: &size, FileVersion: "sha256:" + frame.SHA256,
			Output: string(frame.Result), OutputEncoding: "json", BytesSeen: size}
		if !outcome.OK {
			r.Status = "recorded_flow_failed"
			r.ReasonCode = "browser_flow_failed"
			r.ErrorCode = "browser_flow_failed"
			r.ErrorClass = "action"
			r.Error = "Browser flow failed at a known step; trace is available under this session; do not automatically replay"
		}
		return r
	}
	if frame.Event != "trace_chunk" || frame.Mime != "application/zip" || frame.Size < 4 || frame.Size > browserTraceMaxBytes ||
		frame.Offset != p.Offset || "sha256:"+frame.SHA256 != p.FileVersion || !validTraceDigest(frame.ChunkSHA256) {
		return uncertain()
	}
	maxExpected := int64(browserTraceReadBytes)
	if remaining := int64(frame.Size) - p.Offset; remaining < maxExpected {
		maxExpected = remaining
	}
	if maxExpected < 0 || len(frame.Data) != base64.StdEncoding.EncodedLen(int(maxExpected)) {
		return uncertain()
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(frame.Data)
	if err != nil || int64(len(payload)) != maxExpected {
		return uncertain()
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != frame.ChunkSHA256 {
		return uncertain()
	}
	next := p.Offset + int64(len(payload))
	size := int64(frame.Size)
	return Response{OK: true, Status: "window", SessionID: entry.id, Output: frame.Data, OutputEncoding: "base64",
		MIMEType: "application/zip", FileVersion: p.FileVersion, FileSize: &size,
		RequestedOffset: p.Offset, Offset: int64Ptr(p.Offset), NextOffset: &next, EOF: boolPtr(next == size),
		Truncated: next < size, BytesSeen: size, BytesReturned: int64(len(payload))}
}

func validTraceDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	raw, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(raw) == s
}

// This is the same flow result invariant used by browser_session_flow.
// A complete assertion failure is a known failed action; missing/malformed
// outcome can conceal partial execution and must poison the live session.
func validBrowserTraceFlowResult(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 16000 {
		return false
	}
	var v struct {
		OK         *bool `json:"ok"`
		FailedStep *int  `json:"failed_step"`
	}
	if json.Unmarshal(raw, &v) != nil || v.OK == nil {
		return false
	}
	return (*v.OK && v.FailedStep == nil) || (!*v.OK && v.FailedStep != nil)
}
