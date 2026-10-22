package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

func browserCaptureFixtureJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 48, 36))
	for y := 0; y < 36; y++ {
		for x := 0; x < 48; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 5), G: uint8(y * 7), B: 140, A: 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 45}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func makeCaptureScript(t *testing.T, raw []byte, mode string) string {
	t.Helper()
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	if mode == "wrong_digest" {
		sha = strings.Repeat("0", 64)
		if sha == hex.EncodeToString(sum[:]) {
			sha = strings.Repeat("1", 64)
		}
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	quality := 40
	if mode == "fit_low" {
		quality = 15
	} else if mode == "wrong_quality" {
		quality = 80
	} else if mode == "fit_non_ladder" {
		quality = 39 // below 40 but not one of 40, 27, 15
	}
	ready := `{"event":"ready","fit_quality":true}`
	if strings.HasPrefix(mode, "region_") {
		ready = `{"event":"ready","fit_quality":true,"capture_region":true}`
	}
	if mode == "legacy_meta" || mode == "legacy_zero_quality" || mode == "legacy_null_quality" {
		ready = `{"event":"ready"}` // exact pre-feature readiness protocol
	}
	qualityField := fmt.Sprintf(`,"quality":%d`, quality)
	if mode == "legacy_meta" || mode == "new_no_quality" {
		qualityField = "" // missing field, NOT an explicit quality:0
	} else if mode == "legacy_zero_quality" {
		qualityField = `,"quality":0`
	} else if mode == "legacy_null_quality" {
		qualityField = `,"quality":null`
	}
	region := ""
	if mode == "region_valid" {
		region = `,"region":{"x":8,"y":10,"width":48,"height":36,"viewport_width":56,"viewport_height":46}`
	} else if mode == "region_wrong_rect" {
		region = `,"region":{"x":9,"y":10,"width":48,"height":36,"viewport_width":56,"viewport_height":46}`
	} else if mode == "region_unrequested" {
		region = `,"region":{"x":0,"y":0,"width":48,"height":36,"viewport_width":56,"viewport_height":46}`
	}
	if mode == "region_outside_viewport" {
		return fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION %s\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%%s","reason":"capture_invalid_region"}\n' "$nonce"
done
`, ready)
	}
	if mode == "wrong_index" {
		return fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION %s\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"capture_meta","nonce":"%%s","mime":"image/jpeg"%s,"size":%d,"sha256":"%s","parts":1}\n' "$nonce"
  printf 'ASA_BROWSER_SESSION {"event":"capture_part","nonce":"%%s","index":1,"data":"%s"}\n' "$nonce"
done
`, ready, qualityField, len(raw), sha, b64)
	}
	if mode == "known_too_large" {
		return fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION %s\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%%s","reason":"capture_too_large"}\n' "$nonce"
done
`, ready)
	}
	return fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION %s\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"capture_meta","nonce":"%%s","mime":"image/jpeg"%s,"size":%d,"sha256":"%s","parts":1%s}\n' "$nonce"
  printf 'ASA_BROWSER_SESSION {"event":"capture_part","nonce":"%%s","index":0,"data":"%s"}\n' "$nonce"
  printf 'ASA_BROWSER_SESSION {"event":"capture_done","nonce":"%%s"}\n' "$nonce"
done
`, ready, qualityField, len(raw), sha, region, b64)
}

func openCaptureFixture(t *testing.T, mode string) (*Server, Request, string, []byte) {
	t.Helper()
	s, owner, _ := browserAuditFixture(t)
	raw := browserCaptureFixtureJPEG(t)
	if mode == "corrupt_scan" {
		// A complete-looking JPEG with correct SOI/EOI, parseable headers and
		// accurate *new* digest can still be missing compressed scan data.
		// The original implementation checked DecodeConfig but not Decode.
		start := bytes.Index(raw, []byte{0xff, 0xda}) // start of scan marker
		if start < 0 || start+4 >= len(raw) {
			t.Fatal("JPEG fixture missing scan marker")
		}
		// The SOS segment starts with its two-byte length, inclusive of
		// those bytes. Preserve all JPEG metadata and part of the compressed
		// scan, but replace the remaining scan with a believable EOI marker.
		scan := start + 2 + (int(raw[start+2])<<8 | int(raw[start+3]))
		if scan >= len(raw)-10 {
			t.Fatalf("JPEG fixture scan unexpectedly short: start=%d size=%d", scan, len(raw))
		}
		cut := scan + (len(raw)-scan-2)/2
		raw = append(append([]byte(nil), raw[:cut]...), 0xff, 0xd9)
		if _, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err != nil {
			t.Fatalf("corrupt JPEG must pass DecodeConfig to prove the regression: %v", err)
		}
		if _, err := jpeg.Decode(bytes.NewReader(raw)); err == nil {
			t.Fatal("corrupt JPEG must fail full Decode to prove the regression")
		}
	}
	if err := os.WriteFile(s.browserNodeBinary, []byte(makeCaptureScript(t, raw, mode)), 0755); err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "private-audit.jsonl")
	s.audit = audit.New(auditPath)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("capture test Browser open: %+v", opened)
	}
	return s, owner, opened.SessionID, raw
}
func browserCaptureRequest(owner Request, id string) Request {
	req := browserOwnerControl(owner, id, "browser_session_capture")
	req.Content = `{"quality":40,"max_width":640}`
	return req
}

func TestBrowserSessionCapturePrincipalAndBoundedIntegrity(t *testing.T) {
	s, owner, id, raw := openCaptureFixture(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	req := browserCaptureRequest(owner, id)
	badOwner := req
	badOwner.PrincipalID = "foreign-principal"
	if got := s.browserSessionAction(ctx, badOwner); got.OK || got.ErrorCode != "browser_session_not_found" || got.Output != "" {
		t.Fatalf("cross-principal screenshot leaked: %+v", got)
	}
	badWorkspace := req
	badWorkspace.Workspace = filepath.Dir(owner.Workspace)
	if got := s.browserSessionAction(ctx, badWorkspace); got.OK || got.ErrorCode != "browser_session_not_found" || got.Output != "" {
		t.Fatalf("cross-workspace screenshot leaked: %+v", got)
	}
	invalid := req
	invalid.Content = `{"quality":72,"max_width":640}`
	if got := s.browserSessionAction(ctx, invalid); got.OK || got.ErrorCode != "invalid_browser_capture" {
		t.Fatalf("invalid quality executed: %+v", got)
	}
	invalid.Content = `{"quality":40,"max_width":640,"url":"https://exfil.example"}`
	if got := s.browserSessionAction(ctx, invalid); got.OK || got.ErrorCode != "invalid_browser_capture" {
		t.Fatalf("extra field bypassed strict schema: %+v", got)
	}
	invalid.Content = `{"quality":40,"max_width":640,"fit_quality":"true"}`
	if got := s.browserSessionAction(ctx, invalid); got.OK || got.ErrorCode != "invalid_browser_capture" {
		t.Fatalf("nonboolean fit_quality bypassed strict schema: %+v", got)
	}
	capture := s.browserSessionAction(ctx, req)
	sum := sha256.Sum256(raw)
	if !capture.OK || capture.Status != "captured" || capture.MIMEType != "image/jpeg" || capture.CaptureQuality != 40 || capture.OutputEncoding != "base64" || capture.SessionID != id || capture.FileVersion != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("capture missing bounded metadata: %+v", capture)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(capture.Output)
	if err != nil || !bytes.Equal(decoded, raw) || capture.BytesReturned != int64(len(raw)) {
		t.Fatalf("capture content/integrity mismatch: %v", err)
	}
	// A completed image transfer does not invalidate session refs or lock
	// the Browser. It may be intentionally requested again by its owner.
	again := s.browserSessionAction(ctx, req)
	if !again.OK || again.FileVersion != capture.FileVersion {
		t.Fatalf("proved screenshot unexpectedly poisoned session: %+v", again)
	}
	if close := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")); !close.OK {
		t.Fatalf("capture session close: %+v", close)
	}
	if !s.browserAdmission.acquireRun() {
		t.Fatal("verified screenshot session close held Browser profile")
	}
	s.browserAdmission.releaseRun()
}

// Legacy Agent-provided Browser runners lack both the ready capability and
// quality metadata. The original one-shot path is still byte-accurate; an
// opt-in fitting request is rejected BEFORE writing to the legacy runner and
// does not leave unread frames or poison a valid session.
func TestBrowserSessionCaptureLegacyRunnerOneShotCompatibility(t *testing.T) {
	s, owner, id, raw := openCaptureFixture(t, "legacy_meta")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	oneShot := browserCaptureRequest(owner, id)
	for n := 0; n < 2; n++ {
		got := s.browserSessionAction(ctx, oneShot)
		payload, err := base64.StdEncoding.Strict().DecodeString(got.Output)
		if !got.OK || got.Status != "captured" || got.CaptureQuality != 40 ||
			got.BytesReturned != int64(len(raw)) || err != nil || !bytes.Equal(payload, raw) {
			t.Fatalf("BASE runner -> HEAD Executor legacy one-shot %d failed: %+v decode=%v", n, got, err)
		}
		if n == 0 {
			fitted := oneShot
			fitted.Content = `{"quality":40,"max_width":640,"fit_quality":true}`
			refusal := s.browserSessionAction(ctx, fitted)
			if refusal.OK || refusal.ErrorCode != "browser_capture_fit_unavailable" ||
				refusal.Status != "not_captured" || refusal.Output != "" || refusal.SessionID != id {
				t.Fatalf("legacy runner wrongly advertised fitted screenshot: %+v", refusal)
			}
		}
	}
	status := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_status"))
	if !status.OK || status.Status != "running" {
		t.Fatalf("a known unsupported opt-in stranded legacy Browser session: %+v", status)
	}
}

// Only ABSENT quality metadata belongs to the old runner contract. An
// explicit zero/null (even from a runner without feature support) is malformed
// and must not be mapped to the requested quality.
func TestBrowserSessionCaptureLegacyMetadataRequiresMissingQuality(t *testing.T) {
	for _, mode := range []string{"legacy_zero_quality", "legacy_null_quality"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, id, _ := openCaptureFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
			got := s.browserSessionAction(ctx, browserCaptureRequest(owner, id))
			if got.OK || got.ErrorCode != "browser_session_action_uncertain" || got.Output != "" {
				t.Fatalf("malformed explicit legacy quality was inferred as success: %+v", got)
			}
		})
	}
}

// A lower-quality JPEG is accepted only after the authenticated caller opted
// into quality fitting; the actual output quality is never inferred from the
// requested upper bound. Malformed/unexpected worker claims fail closed.
func TestBrowserSessionCaptureFitQualityExplicitAndBounded(t *testing.T) {
	t.Run("explicit_opt_in", func(t *testing.T) {
		s, owner, id, raw := openCaptureFixture(t, "fit_low")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
		req := browserCaptureRequest(owner, id)
		req.Content = `{"quality":40,"max_width":640,"fit_quality":true}`
		got := s.browserSessionAction(ctx, req)
		if !got.OK || got.CaptureQuality != 15 || got.BytesReturned != int64(len(raw)) {
			t.Fatalf("explicit quality fit not acknowledged: %+v", got)
		}
		data, err := base64.StdEncoding.DecodeString(got.Output)
		if err != nil || !bytes.Equal(data, raw) {
			t.Fatalf("fitted screenshot payload changed: %v", err)
		}
	})
	for _, tc := range []struct {
		name    string
		mode    string
		content string
	}{
		{"not_opted_in", "fit_low", `{"quality":40,"max_width":640}`},
		{"outside_upper_bound", "wrong_quality", `{"quality":40,"max_width":640,"fit_quality":true}`},
		{"in_range_but_not_ladder", "fit_non_ladder", `{"quality":40,"max_width":640,"fit_quality":true}`},
		{"new_runner_missing_quality", "new_no_quality", `{"quality":40,"max_width":640}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, owner, id, _ := openCaptureFixture(t, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
			req := browserCaptureRequest(owner, id)
			req.Content = tc.content
			got := s.browserSessionAction(ctx, req)
			if got.OK || got.ErrorCode != "browser_session_action_uncertain" || got.Output != "" {
				t.Fatalf("unexpected capture quality claim accepted: %+v", got)
			}
		})
	}
}

func TestBrowserSessionCaptureSelfConsistentCorruptJPEGRejected(t *testing.T) {
	s, owner, id, corrupt := openCaptureFixture(t, "corrupt_scan")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()

	// The fixture sends the entire announced payload with matching size,
	// SHA-256, valid JPEG markers and parseable image configuration, then
	// capture_done. Only a complete scan decode can reject it.
	if !bytes.HasPrefix(corrupt, []byte{0xff, 0xd8, 0xff}) || !bytes.HasSuffix(corrupt, []byte{0xff, 0xd9}) {
		t.Fatal("invalid regression fixture JPEG marker bytes")
	}
	request := browserCaptureRequest(owner, id)
	for attempt := 0; attempt < 2; attempt++ {
		got := s.browserSessionAction(ctx, request)
		if got.OK || got.ErrorCode != "browser_capture_invalid_image" || got.Status != "not_captured" ||
			got.Output != "" || got.MIMEType != "" || got.SessionID != id || got.BytesReturned != 0 {
			t.Fatalf("self-consistent corrupt JPEG cannot be reported as delivered (attempt %d): %+v", attempt, got)
		}
	}
	status := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_status"))
	if !status.OK || status.Status != "running" {
		t.Fatalf("complete-but-invalid JPEG must remain a known failure, not a stranded/uncertain session: %+v", status)
	}
}

func TestBrowserSessionCaptureFrameFailuresAreFailClosed(t *testing.T) {
	for _, mode := range []string{"wrong_digest", "wrong_index"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, id, _ := openCaptureFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
			req := browserCaptureRequest(owner, id)
			first := s.browserSessionAction(ctx, req)
			if first.OK || first.ErrorCode != "browser_session_action_uncertain" || first.Output != "" || first.SessionID != id {
				t.Fatalf("corrupt screenshot not refused: %+v", first)
			}
			second := s.browserSessionAction(ctx, req)
			if second.OK || second.ErrorCode != "browser_session_uncertain" || second.Output != "" {
				t.Fatalf("corrupt capture replayed: %+v", second)
			}
			if s.browserAdmission.acquireRun() {
				t.Fatal("corrupt transfer prematurely released profile")
			}
		})
	}
}

func TestBrowserSessionCaptureKnownTooLargeDoesNotPoisonSession(t *testing.T) {
	s, owner, id, _ := openCaptureFixture(t, "known_too_large")
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	req := browserCaptureRequest(owner, id)
	for i := 0; i < 2; i++ {
		got := s.browserSessionAction(ctx, req)
		if got.OK || got.ErrorCode != "too_large" || got.Status != "not_captured" || got.Output != "" || got.SessionID != id {
			t.Fatalf("known bounded screenshot refusal %d: %+v", i, got)
		}
	}
}

// Viewport selection is an opt-in extension of the existing authenticated
// Browser capture path. It must not be silently ignored by a legacy runner.
func TestBrowserSessionCaptureRegionStrictAndLegacyRunner(t *testing.T) {
	requestJSON := `{"quality":40,"max_width":640,"crop_x":8,"crop_y":10,"max_height":64}`
	t.Run("legacy_refused_without_poisoning", func(t *testing.T) {
		s, owner, id, _ := openCaptureFixture(t, "")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
		req := browserCaptureRequest(owner, id)
		req.Content = requestJSON
		got := s.browserSessionAction(ctx, req)
		if got.OK || got.ErrorCode != "browser_capture_region_unavailable" || got.Status != "not_captured" || got.Output != "" {
			t.Fatalf("legacy runner silently accepted region: %+v", got)
		}
		if old := s.browserSessionAction(ctx, browserCaptureRequest(owner, id)); !old.OK || old.RegionVerified {
			t.Fatalf("known unsupported region poisoned old one-shot: %+v", old)
		}
	})
	t.Run("verified_offset_and_dimensions", func(t *testing.T) {
		s, owner, id, raw := openCaptureFixture(t, "region_valid")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
		req := browserCaptureRequest(owner, id)
		req.Content = requestJSON
		got := s.browserSessionAction(ctx, req)
		data, err := base64.StdEncoding.Strict().DecodeString(got.Output)
		if !got.OK || !got.RegionVerified || got.CaptureCropX != 8 || got.CaptureCropY != 10 ||
			got.CaptureWidth != 48 || got.CaptureHeight != 36 || err != nil || !bytes.Equal(data, raw) {
			t.Fatalf("selected bounded viewport not verified: %+v decode=%v", got, err)
		}
		for _, bad := range []string{
			`{"quality":40,"max_width":640,"crop_x":-1,"crop_y":10,"max_height":64}`,
			`{"quality":40,"max_width":640,"crop_x":8,"crop_y":10,"max_height":721}`,
			`{"quality":40,"max_width":640,"crop_x":8,"crop_y":10}`,
			`{"quality":40,"max_width":640,"crop_x":8,"crop_y":10,"max_height":64,"foreign":1}`,
		} {
			req.Content = bad
			if refused := s.browserSessionAction(ctx, req); refused.OK || refused.ErrorCode != "invalid_browser_capture" {
				t.Fatalf("invalid region accepted or poisoned: %+v", refused)
			}
		}
		req.Content = requestJSON
		if next := s.browserSessionAction(ctx, req); !next.OK {
			t.Fatalf("invalid input poisoned valid Browser region: %+v", next)
		}
	})
	t.Run("known_outside_viewport_refused_without_uncertainty", func(t *testing.T) {
		s, owner, id, _ := openCaptureFixture(t, "region_outside_viewport")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
		req := browserCaptureRequest(owner, id)
		req.Content = requestJSON
		got := s.browserSessionAction(ctx, req)
		if got.OK || got.ErrorCode != "browser_capture_invalid_region" || got.Status != "not_captured" || got.Output != "" {
			t.Fatalf("known out-of-viewport region not classified: %+v", got)
		}
		if status := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_status")); !status.OK || status.Status != "running" {
			t.Fatalf("known region input rejection poisoned Browser session: %+v", status)
		}
	})
	t.Run("wrong_rectangle_retains_uncertain_owner", func(t *testing.T) {
		s, owner, id, _ := openCaptureFixture(t, "region_wrong_rect")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
		req := browserCaptureRequest(owner, id)
		req.Content = requestJSON
		got := s.browserSessionAction(ctx, req)
		if got.OK || got.ErrorCode != "browser_session_action_uncertain" || got.Output != "" {
			t.Fatalf("unverified rectangle delivered pixels: %+v", got)
		}
		if next := s.browserSessionAction(ctx, req); next.OK || next.ErrorCode != "browser_session_uncertain" {
			t.Fatalf("unverified region silently replayed: %+v", next)
		}
	})
	t.Run("unrequested_region_is_not_trusted", func(t *testing.T) {
		s, owner, id, _ := openCaptureFixture(t, "region_unrequested")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
		if got := s.browserSessionAction(ctx, browserCaptureRequest(owner, id)); got.OK || got.ErrorCode != "browser_session_action_uncertain" {
			t.Fatalf("unsolicited viewport metadata accepted: %+v", got)
		}
	})
}
