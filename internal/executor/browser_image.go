package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// A deliberately small, session-owned continuation of the existing Browser
// screenshot. The pinned Node runner retains at most one 1 MiB JPEG in its
// private process memory. The Executor never exposes a path, URL, whole-image
// frame, or an unauthenticated artifact handle; every window reuses the same
// principal/workspace/session checks and 64 KiB broker ring as Browser trace.
const (
	browserImageMaxBytes  = 1 << 20
	browserImageReadBytes = 8192
)

type browserImageParams struct {
	Operation   string `json:"operation"`
	Quality     int    `json:"quality,omitempty"`
	MaxWidth    int    `json:"max_width,omitempty"`
	MaxHeight   int    `json:"max_height,omitempty"`
	CropX       int    `json:"crop_x,omitempty"`
	CropY       int    `json:"crop_y,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	FileVersion string `json:"file_version,omitempty"`
}

func (s *Server) browserSessionImage(ctx context.Context, req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	if len(req.Content) == 0 || len(req.Content) > 512 || req.TimeoutMS != 0 {
		return browserSessionError("invalid_browser_image", "validation", errors.New("bounded image operation required"))
	}
	dec := json.NewDecoder(strings.NewReader(req.Content))
	dec.DisallowUnknownFields()
	var p browserImageParams
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return browserSessionError("invalid_browser_image", "validation", errors.New("invalid image operation fields"))
	}
	switch p.Operation {
	case "capture":
		if p.FileVersion != "" || p.Offset != 0 || p.Limit != 0 || p.Quality < 15 || p.Quality > 70 ||
			p.MaxWidth < 320 || p.MaxWidth > 1024 || p.MaxHeight < 1 || p.MaxHeight > 720 ||
			p.CropX < 0 || p.CropY < 0 {
			return browserSessionError("invalid_browser_image", "validation", errors.New("capture requires quality 15..70, width 320..1024 and viewport crop height 1..720"))
		}
	case "read", "discard":
		if p.Operation == "read" && p.Limit == 0 {
			p.Limit = browserImageReadBytes
		}
		if p.Limit < 0 || p.Limit > browserImageReadBytes ||
			(p.Operation == "discard" && p.Limit != 0) {
			return browserSessionError("invalid_browser_image", "validation", errors.New("read limit must be 1..8192, discard requires no limit"))
		}
		if p.Quality != 0 || p.MaxWidth != 0 || p.MaxHeight != 0 || p.CropX != 0 || p.CropY != 0 ||
			p.Offset < 0 || p.Offset > browserImageMaxBytes ||
			(p.Operation == "discard" && p.Offset != 0) ||
			!validImageDigest(p.FileVersion) {
			return browserSessionError("invalid_browser_image", "validation", errors.New("read/discard require exact SHA-256 version and bounded offset"))
		}
	default:
		return browserSessionError("invalid_browser_image", "validation", errors.New("operation must be capture, read or discard"))
	}

	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("previous Browser operation unverified; close before retry"))
	}
	if !state.imageStagingSupported {
		r := browserSessionError("browser_image_unavailable", "compatibility", errors.New("installed Browser runner cannot stage large JPEGs; upgrade Agent and Executor together"))
		r.SessionID, r.Status = entry.id, "not_delivered"
		return r
	}
	uncertain := func() Response {
		state.uncertain = true
		r := browserSessionError("browser_session_action_uncertain", "state", errors.New("image staging/transfer completion uncertain; close session, never assume bytes delivered"))
		r.SessionID, r.Status = entry.id, "uncertain"
		return r
	}
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return browserSessionError("browser_nonce_unavailable", "security", err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	command := map[string]any{"nonce": nonce, "type": "image_" + p.Operation}
	switch p.Operation {
	case "capture":
		command["quality"], command["max_width"], command["max_height"] = p.Quality, p.MaxWidth, p.MaxHeight
		command["crop_x"], command["crop_y"] = p.CropX, p.CropY
	case "read":
		command["offset"], command["file_version"], command["limit"] = p.Offset, p.FileVersion, p.Limit
	case "discard":
		command["file_version"] = p.FileVersion
	}
	raw, err := json.Marshal(command)
	if err != nil || len(raw) > 512 || s.workerStdioWrite(req, entry.id, append(raw, '\n')) != nil {
		return uncertain()
	}
	timeout := 8 * time.Second
	if p.Operation == "capture" {
		timeout = 20 * time.Second
	}
	frameCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	frame, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil {
		return uncertain()
	}
	if frame.Event == "error" {
		var code, class, message string
		switch frame.Reason {
		case "image_too_large":
			if p.Operation == "capture" {
				code, class, message = "too_large", "resource", "viewport JPEG exceeds the 1 MiB per-session limit; choose a smaller region rather than increasing transport buffers"
			}
		case "image_invalid_region":
			if p.Operation == "capture" {
				code, class, message = "browser_image_invalid_region", "validation", "image crop origin is outside the actual viewport"
			}
		case "image_capture_failed":
			if p.Operation == "capture" {
				code, class, message = "browser_image_capture_failed", "runtime", "Browser could not complete JPEG capture"
			}
		case "image_unavailable", "image_stale", "image_invalid_offset", "image_read_budget_exhausted":
			if p.Operation != "capture" {
				code, class, message = frame.Reason, "state", "image not delivered: staged data/version/offset is unavailable or its finite read budget was exhausted"
			}
		}
		if code == "" {
			return uncertain()
		}
		r := browserSessionError(code, class, errors.New(message))
		r.SessionID, r.Status = entry.id, "not_delivered"
		return r
	}
	switch p.Operation {
	case "discard":
		if frame.Event != "image_discarded" {
			return uncertain()
		}
		return Response{OK: true, SessionID: entry.id, Status: "discarded"}
	case "capture":
		if frame.Event != "image_meta" || frame.Mime != "image/jpeg" ||
			frame.Size < 4 || frame.Size > browserImageMaxBytes || !validTraceDigest(frame.SHA256) {
			return uncertain()
		}
		var actualQuality int
		if len(frame.Quality) == 0 || json.Unmarshal(frame.Quality, &actualQuality) != nil || actualQuality != p.Quality {
			return uncertain()
		}
		var region browserCaptureRegion
		regionDec := json.NewDecoder(strings.NewReader(string(frame.Region)))
		regionDec.DisallowUnknownFields()
		if regionDec.Decode(&region) != nil || regionDec.Decode(new(any)) != io.EOF ||
			region.ViewportWidth < 1 || region.ViewportHeight < 1 ||
			region.X != p.CropX || region.Y != p.CropY ||
			region.X >= region.ViewportWidth || region.Y >= region.ViewportHeight ||
			region.Width != min(p.MaxWidth, region.ViewportWidth-region.X) ||
			region.Height != min(p.MaxHeight, region.ViewportHeight-region.Y) {
			return uncertain()
		}
		size := int64(frame.Size)
		return Response{OK: true, Status: "staged", SessionID: entry.id,
			MIMEType: "image/jpeg", FileVersion: "sha256:" + frame.SHA256, FileSize: &size,
			CaptureQuality: actualQuality, RegionVerified: true, CaptureCropX: region.X, CaptureCropY: region.Y,
			CaptureWidth: region.Width, CaptureHeight: region.Height, BytesSeen: size}
	case "read":
		if frame.Event != "image_chunk" || frame.Mime != "image/jpeg" ||
			frame.Size < 4 || frame.Size > browserImageMaxBytes ||
			frame.Offset != p.Offset || "sha256:"+frame.SHA256 != p.FileVersion ||
			!validTraceDigest(frame.ChunkSHA256) {
			return uncertain()
		}
		count := int64(p.Limit)
		if remain := int64(frame.Size) - p.Offset; remain < count {
			count = remain
		}
		if count < 0 || len(frame.Data) != base64.StdEncoding.EncodedLen(int(count)) {
			return uncertain()
		}
		part, err := base64.StdEncoding.Strict().DecodeString(frame.Data)
		if err != nil || int64(len(part)) != count {
			return uncertain()
		}
		sum := sha256.Sum256(part)
		if hex.EncodeToString(sum[:]) != frame.ChunkSHA256 {
			return uncertain()
		}
		next := p.Offset + int64(len(part))
		size, off, eof := int64(frame.Size), p.Offset, next == int64(frame.Size)
		return Response{OK: true, Status: "window", SessionID: entry.id,
			MIMEType: "image/jpeg", FileVersion: p.FileVersion, FileSize: &size,
			RequestedOffset: off, Offset: &off, NextOffset: &next, EOF: &eof,
			Truncated: !eof, BytesSeen: size, BytesReturned: int64(len(part)),
			Output: frame.Data, OutputEncoding: "base64"}
	}
	return uncertain()
}

func validImageDigest(version string) bool {
	return len(version) == 71 && strings.HasPrefix(version, "sha256:") && validTraceDigest(strings.TrimPrefix(version, "sha256:"))
}
