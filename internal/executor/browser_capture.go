package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"strings"
	"time"
)

const (
	browserCaptureMaxBytes  = 32 << 10
	browserCapturePartChars = 8192
)

type browserCaptureParams struct {
	Quality    int  `json:"quality"`
	MaxWidth   int  `json:"max_width"`
	FitQuality bool `json:"fit_quality"`
	CropX      int  `json:"crop_x"`
	CropY      int  `json:"crop_y"`
	MaxHeight  int  `json:"max_height"`
}

// Only explicit opt-in uses these fields; legacy screenshots preserve their
// original wire and dimensional contract. A region is bounded to the actual
// current viewport, with no implicit full-page capture or silent rescaling.
type browserCaptureRegion struct {
	X              int `json:"x"`
	Y              int `json:"y"`
	Width          int `json:"width"`
	Height         int `json:"height"`
	ViewportWidth  int `json:"viewport_width"`
	ViewportHeight int `json:"viewport_height"`
}

// Screenshot is an opt-in visual read of the same authenticated Browser
// session. No named artifact file, global artifact ID, URL or second storage
// service is created: the pinned worker emits bounded base64 frames in the
// existing per-session broker ring. All bytes are held by the current call
// and discarded on session close. The tight 32 KiB bound fits the existing
// 64 KiB broker event ring *even if the worker emits before the first read*.
func (s *Server) browserSessionCapture(ctx context.Context, req Request) Response {
	entry, err := s.browserSessionEntry(req)
	if err != nil {
		return browserSessionError("browser_session_not_found", "authorization", errSessionNotFound)
	}
	if len(req.Content) == 0 || len(req.Content) > 256 || req.TimeoutMS != 0 {
		return browserSessionError("invalid_browser_capture", "validation", errors.New("bounded capture parameters required"))
	}
	dec := json.NewDecoder(strings.NewReader(req.Content))
	dec.DisallowUnknownFields()
	var p browserCaptureParams
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF || p.Quality < 15 || p.Quality > 70 || p.MaxWidth < 320 || p.MaxWidth > 1024 {
		return browserSessionError("invalid_browser_capture", "validation", errors.New("quality 15..70 and max_width 320..1024 required"))
	}
	regionRequested := p.CropX != 0 || p.CropY != 0 || p.MaxHeight != 0
	if p.CropX < 0 || p.CropY < 0 ||
		(regionRequested && (p.MaxHeight < 1 || p.MaxHeight > 720)) {
		return browserSessionError("invalid_browser_capture", "validation", errors.New("crop_x/crop_y must be nonnegative and explicit max_height 1..720 required"))
	}
	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("previous Browser action completion unknown; close first"))
	}
	if regionRequested && !state.regionSupported {
		// A pre-upgrade runner would ignore the requested rectangle and send
		// a successful top-left screenshot. Reject without writing to it.
		resp := browserSessionError("browser_capture_region_unavailable", "compatibility", errors.New("Browser session runner does not support viewport region selection; reopen after upgrading the Agent"))
		resp.SessionID, resp.Status = entry.id, "not_captured"
		return resp
	}
	if p.FitQuality && !state.fitQualitySupported {
		// A pre-upgrade runner ignores the new input flag and would otherwise
		// silently return an unfitted one-shot capture. Reject BEFORE sending.
		resp := browserSessionError("browser_capture_fit_unavailable", "compatibility", errors.New("Browser session runner does not support fit_quality; reopen after upgrading the Agent"))
		resp.SessionID = entry.id
		resp.Status = "not_captured"
		return resp
	}
	uncertain := func() Response {
		state.uncertain = true
		r := browserSessionError("browser_session_action_uncertain", "state", errors.New("Screenshot completion or transport integrity unknown; inspect status and close before further actions"))
		r.SessionID = entry.id
		r.Status = "uncertain"
		return r
	}
	var n [16]byte
	if _, err = rand.Read(n[:]); err != nil {
		return browserSessionError("browser_nonce_unavailable", "security", err)
	}
	nonce := hex.EncodeToString(n[:])
	// Preserve the original default worker wire format across rolling
	// Agent/Executor upgrades. Only an explicit opt-in sends the new key.
	msg := fmt.Sprintf(`{"type":"capture","nonce":%q,"quality":%d,"max_width":%d}`+"\n", nonce, p.Quality, p.MaxWidth)
	if p.FitQuality {
		msg = fmt.Sprintf(`{"type":"capture","nonce":%q,"quality":%d,"max_width":%d,"fit_quality":true}`+"\n", nonce, p.Quality, p.MaxWidth)
	}
	if regionRequested {
		msg = fmt.Sprintf(`{"type":"capture","nonce":%q,"quality":%d,"max_width":%d,"fit_quality":%t,"crop_x":%d,"crop_y":%d,"max_height":%d}`+"\n",
			nonce, p.Quality, p.MaxWidth, p.FitQuality, p.CropX, p.CropY, p.MaxHeight)
	}
	if err = s.workerStdioWrite(req, entry.id, []byte(msg)); err != nil {
		return uncertain()
	}
	frameCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil {
		return uncertain()
	}
	if start.Event == "error" {
		code := "browser_capture_failed"
		class := "runtime"
		message := "Screenshot capture failed on the current page; check Browser session state before retrying"
		if start.Reason == "capture_too_large" {
			code = "too_large"
			class = "resource"
			message = "Screenshot exceeds the 32 KiB raw-byte cap; reduce quality/max_width or use the advanced Browser workflow"
		} else if start.Reason == "capture_invalid_region" && regionRequested {
			code = "browser_capture_invalid_region"
			class = "validation"
			message = "Requested crop is outside the current Browser viewport; choose an in-viewport crop_x/crop_y"
		} else if start.Reason != "capture_failed" {
			return uncertain()
		}
		resp := browserSessionError(code, class, errors.New(message))
		resp.SessionID = entry.id
		resp.Status = "not_captured"
		return resp
	}
	// Old Agent runners emit capture_meta without quality. Only runners
	// authenticated at session open as legacy and used in one-shot mode may
	// inherit the requested quality (the legacy screenshot protocol uses
	// exactly that value). A new runner omitting quality is a corrupt frame.
	var actualQuality int
	if len(start.Quality) == 0 {
		if !p.FitQuality && !state.fitQualitySupported {
			actualQuality = p.Quality
		}
	} else if err = json.Unmarshal(start.Quality, &actualQuality); err != nil {
		// Explicit quality:0/null/string is NOT an old missing field.
		return uncertain()
	}
	qualityValid := actualQuality == p.Quality
	if p.FitQuality {
		// Match the exact finite ladder used by the pinned runner, not any
		// value below the requested ceiling.
		qualityValid = qualityValid || actualQuality == max(15, (p.Quality+15)/2) || actualQuality == 15
	}
	expectedParts := (base64.StdEncoding.EncodedLen(start.Size) + browserCapturePartChars - 1) / browserCapturePartChars
	if start.Event != "capture_meta" || start.Mime != "image/jpeg" || start.Size < 4 || start.Size > browserCaptureMaxBytes ||
		!qualityValid || actualQuality < 15 || actualQuality > p.Quality ||
		start.Parts != expectedParts || expectedParts < 1 || expectedParts > 6 || len(start.SHA256) != 64 {
		return uncertain()
	}
	if _, err = hex.DecodeString(start.SHA256); err != nil {
		return uncertain()
	}
	var selected browserCaptureRegion
	if regionRequested {
		if len(start.Region) == 0 || bytes.Equal(start.Region, []byte("null")) {
			return uncertain()
		}
		dec := json.NewDecoder(bytes.NewReader(start.Region))
		dec.DisallowUnknownFields()
		if dec.Decode(&selected) != nil || dec.Decode(new(any)) != io.EOF ||
			selected.ViewportWidth < 1 || selected.ViewportHeight < 1 ||
			selected.X != p.CropX || selected.Y != p.CropY ||
			selected.X >= selected.ViewportWidth || selected.Y >= selected.ViewportHeight ||
			selected.Width != min(p.MaxWidth, selected.ViewportWidth-selected.X) ||
			selected.Height != min(p.MaxHeight, selected.ViewportHeight-selected.Y) {
			return uncertain()
		}
	} else if len(start.Region) > 0 {
		// Non-opt-in responses must preserve the original known wire format.
		return uncertain()
	}
	var b64 strings.Builder
	b64.Grow(base64.StdEncoding.EncodedLen(start.Size))
	for i := 0; i < start.Parts; i++ {
		part, er := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
		if er != nil || part.Event != "capture_part" || part.Index != i || len(part.Data) == 0 || len(part.Data) > browserCapturePartChars {
			return uncertain()
		}
		b64.WriteString(part.Data)
	}
	end, err := s.browserSessionReadFrame(frameCtx, req, state, entry.id, nonce)
	if err != nil || end.Event != "capture_done" || b64.Len() != base64.StdEncoding.EncodedLen(start.Size) {
		return uncertain()
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(b64.String())
	if err != nil || len(payload) != start.Size || !bytes.HasPrefix(payload, []byte{0xff, 0xd8, 0xff}) || !bytes.HasSuffix(payload, []byte{0xff, 0xd9}) {
		return uncertain()
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != start.SHA256 {
		return uncertain()
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(payload))
	if err != nil || config.Width < 1 || config.Width > 2048 || config.Height < 1 || config.Height > 1440 {
		return uncertain()
	}
	if regionRequested && (config.Width != selected.Width || config.Height != selected.Height) {
		return uncertain() // SHA alone does not attest which viewport tile arrived.
	}
	// DecodeConfig validates only the JPEG headers/dimensions. A malicious or
	// malfunctioning Browser worker can announce a matching SHA-256 for a
	// truncated or corrupt scan, so the digest alone does not prove that an
	// MCP client can actually decode the image. Bound dimensions *before*
	// allocating a full image, then consume the complete compressed stream.
	decoded, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		// All nonce-bound frames, size and digest have been verified: the
		// transfer completed, but the reported JPEG data is unusable. This is
		// a known invalid artifact, not an unknown Browser action outcome.
		resp := browserSessionError("browser_capture_invalid_image", "data", errors.New("captured JPEG cannot be fully decoded; no image delivered"))
		resp.SessionID = entry.id
		resp.Status = "not_captured"
		return resp
	}
	return Response{OK: true, Status: "captured", SessionID: entry.id,
		Output: b64.String(), OutputEncoding: "base64", MIMEType: "image/jpeg", CaptureQuality: actualQuality, BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload)),
		RegionVerified: regionRequested, CaptureCropX: selected.X, CaptureCropY: selected.Y,
		CaptureWidth: selected.Width, CaptureHeight: selected.Height,
		FileVersion: "sha256:" + start.SHA256}
}
