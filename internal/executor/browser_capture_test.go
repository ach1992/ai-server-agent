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
	if mode == "wrong_index" {
		return fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"capture_meta","nonce":"%%s","mime":"image/jpeg","size":%d,"sha256":"%s","parts":1}\n' "$nonce"
  printf 'ASA_BROWSER_SESSION {"event":"capture_part","nonce":"%%s","index":1,"data":"%s"}\n' "$nonce"
done
`, len(raw), sha, b64)
	}
	if mode == "known_too_large" {
		return `#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%s","reason":"capture_too_large"}\n' "$nonce"
done
`
	}
	return fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0;; esac
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"capture_meta","nonce":"%%s","mime":"image/jpeg","size":%d,"sha256":"%s","parts":1}\n' "$nonce"
  printf 'ASA_BROWSER_SESSION {"event":"capture_part","nonce":"%%s","index":0,"data":"%s"}\n' "$nonce"
  printf 'ASA_BROWSER_SESSION {"event":"capture_done","nonce":"%%s"}\n' "$nonce"
done
`, len(raw), sha, b64)
}

func openCaptureFixture(t *testing.T, mode string) (*Server, Request, string, []byte) {
	t.Helper()
	s, owner, _ := browserAuditFixture(t)
	raw := browserCaptureFixtureJPEG(t)
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
	capture := s.browserSessionAction(ctx, req)
	sum := sha256.Sum256(raw)
	if !capture.OK || capture.Status != "captured" || capture.MIMEType != "image/jpeg" || capture.OutputEncoding != "base64" || capture.SessionID != id || capture.FileVersion != "sha256:"+hex.EncodeToString(sum[:]) {
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
