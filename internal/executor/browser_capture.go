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
	Quality  int `json:"quality"`
	MaxWidth int `json:"max_width"`
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
	state := entry.browser
	if !state.mu.TryLock() {
		return browserSessionBusy()
	}
	defer state.mu.Unlock()
	if state.uncertain {
		return browserSessionError("browser_session_uncertain", "state", errors.New("previous Browser action completion unknown; close first"))
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
	msg := fmt.Sprintf(`{"type":"capture","nonce":%q,"quality":%d,"max_width":%d}`+"\n", nonce, p.Quality, p.MaxWidth)
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
		} else if start.Reason != "capture_failed" {
			return uncertain()
		}
		resp := browserSessionError(code, class, errors.New(message))
		resp.SessionID = entry.id
		resp.Status = "not_captured"
		return resp
	}
	expectedParts := (base64.StdEncoding.EncodedLen(start.Size) + browserCapturePartChars - 1) / browserCapturePartChars
	if start.Event != "capture_meta" || start.Mime != "image/jpeg" || start.Size < 4 || start.Size > browserCaptureMaxBytes ||
		start.Parts != expectedParts || expectedParts < 1 || expectedParts > 6 || len(start.SHA256) != 64 {
		return uncertain()
	}
	if _, err = hex.DecodeString(start.SHA256); err != nil {
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
	return Response{OK: true, Status: "captured", SessionID: entry.id,
		Output: b64.String(), OutputEncoding: "base64", MIMEType: "image/jpeg", BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload)),
		FileVersion: "sha256:" + start.SHA256}
}
