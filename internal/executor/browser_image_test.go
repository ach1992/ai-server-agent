package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openImageFixture(t *testing.T, mode string) (*Server, Request, string, []byte) {
	t.Helper()
	s, owner, _ := browserAuditFixture(t)
	raw := browserCaptureFixtureJPEG(t)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	part := sha256.Sum256(raw)
	partDigest := hex.EncodeToString(part[:])
	if mode == "bad_chunk_digest" {
		partDigest = strings.Repeat("f", 64)
	}
	ready := `{"event":"ready","image_staging":true}`
	if mode == "old_runner" {
		ready = `{"event":"ready"}`
	}
	metadata := fmt.Sprintf(`{"event":"image_meta","nonce":"%%s","mime":"image/jpeg","size":%d,"sha256":"%s","quality":70,"region":{"x":0,"y":0,"width":48,"height":36,"viewport_width":48,"viewport_height":36}}`, len(raw), digest)
	if mode == "too_large" {
		metadata = `{"event":"error","nonce":"%s","reason":"image_too_large"}`
	}
	if mode == "bad_region" {
		metadata = fmt.Sprintf(`{"event":"image_meta","nonce":"%%s","mime":"image/jpeg","size":%d,"sha256":"%s","quality":70,"region":{"x":9,"y":0,"width":48,"height":36,"viewport_width":48,"viewport_height":36}}`, len(raw), digest)
	}
	shell := fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION %s\n'
available=0
while IFS= read -r line; do
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  case "$line" in
    *'"type":"close"'*) exit 0 ;;
    *'"type":"image_capture"'*)
      available=1
      printf 'ASA_BROWSER_SESSION %s\n' "$nonce" ;;
    *'"type":"image_read"'*)
      if [ "$available" -eq 0 ]; then
        printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%%s","reason":"image_unavailable"}\n' "$nonce"
      elif ! printf '%%s' "$line" | grep -q 'sha256:%s'; then
        printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%%s","reason":"image_stale"}\n' "$nonce"
      else
        printf 'ASA_BROWSER_SESSION {"event":"image_chunk","nonce":"%%s","mime":"image/jpeg","size":%d,"offset":0,"sha256":"%s","chunk_sha256":"%s","data":"%s"}\n' "$nonce"
      fi ;;
    *'"type":"image_discard"'*)
      available=0
      printf 'ASA_BROWSER_SESSION {"event":"image_discarded","nonce":"%%s"}\n' "$nonce" ;;
  esac
done
`, ready, metadata, digest, len(raw), digest, partDigest, base64.StdEncoding.EncodeToString(raw))
	if err := os.WriteFile(s.browserNodeBinary, []byte(shell), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("open fake Browser: %+v", opened)
	}
	return s, owner, opened.SessionID, raw
}
func imageReq(owner Request, id, content string) Request {
	req := browserOwnerControl(owner, id, "browser_session_image")
	req.Content = content
	return req
}

func TestBrowserImagePrincipalScopedMetadataAndWindows(t *testing.T) {
	s, owner, id, raw := openImageFixture(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	capture := imageReq(owner, id, `{"operation":"capture","quality":70,"max_width":640,"max_height":720}`)
	wrong := capture
	wrong.PrincipalID = "other-principal"
	if got := s.browserSessionAction(ctx, wrong); got.OK || got.ErrorCode != "browser_session_not_found" || got.Output != "" {
		t.Fatalf("image leaked across principals: %+v", got)
	}
	wrong = capture
	wrong.Workspace = filepath.Dir(owner.Workspace)
	if got := s.browserSessionAction(ctx, wrong); got.OK || got.ErrorCode != "browser_session_not_found" || got.Output != "" {
		t.Fatalf("image leaked across workspaces: %+v", got)
	}
	for _, input := range []string{
		`{"operation":"capture","quality":75,"max_width":640,"max_height":720}`,
		`{"operation":"capture","quality":70,"max_width":640,"max_height":720,"url":"https://evil.example"}`,
		`{"operation":"capture","quality":70,"max_width":640,"max_height":720,"offset":1}`,
		`{"operation":"read","file_version":"bad","offset":0}`,
		`{"operation":"read","file_version":"sha256:` + strings.Repeat("0", 64) + `","limit":99999}`,
		`{"operation":"discard","file_version":"sha256:` + strings.Repeat("0", 64) + `","offset":1}`,
	} {
		if got := s.browserSessionAction(ctx, imageReq(owner, id, input)); got.OK || got.ErrorCode != "invalid_browser_image" {
			t.Fatalf("invalid operation admitted: %s, %+v", input, got)
		}
	}
	staged := s.browserSessionAction(ctx, capture)
	sum := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(sum[:])
	if !staged.OK || staged.Status != "staged" || staged.Output != "" || staged.FileVersion != version ||
		staged.FileSize == nil || *staged.FileSize != int64(len(raw)) ||
		staged.CaptureQuality != 70 || !staged.RegionVerified || staged.CaptureWidth != 48 || staged.CaptureHeight != 36 {
		t.Fatalf("image metadata leaked or failed provenance: %+v", staged)
	}
	read := imageReq(owner, id, fmt.Sprintf(`{"operation":"read","offset":0,"file_version":%q}`, version))
	got := s.browserSessionAction(ctx, read)
	part, err := base64.StdEncoding.Strict().DecodeString(got.Output)
	if err != nil || !got.OK || got.Status != "window" || got.MIMEType != "image/jpeg" ||
		got.FileVersion != version || got.BytesReturned != int64(len(raw)) || !bytes.Equal(part, raw) ||
		got.NextOffset == nil || *got.NextOffset != int64(len(raw)) || got.EOF == nil || !*got.EOF {
		t.Fatalf("corrupt/missing image continuation: %+v err=%v", got, err)
	}
	stale := read
	stale.Content = fmt.Sprintf(`{"operation":"read","offset":0,"file_version":"sha256:%s"}`, strings.Repeat("0", 64))
	if denied := s.browserSessionAction(ctx, stale); denied.OK || denied.ErrorCode != "image_stale" || denied.Output != "" {
		t.Fatalf("stale image returned bytes: %+v", denied)
	}
	if again := s.browserSessionAction(ctx, read); !again.OK {
		t.Fatalf("known stale version poisoned valid Browser session: %+v", again)
	}
	discard := imageReq(owner, id, fmt.Sprintf(`{"operation":"discard","file_version":%q}`, version))
	if got := s.browserSessionAction(ctx, discard); !got.OK || got.Status != "discarded" {
		t.Fatalf("image discard not verified: %+v", got)
	}
	if denied := s.browserSessionAction(ctx, read); denied.OK || denied.ErrorCode != "image_unavailable" {
		t.Fatalf("discarded JPEG still available: %+v", denied)
	}
}

func TestBrowserImageInvalidFramePoisonsSessionAndPreservesLease(t *testing.T) {
	for _, mode := range []string{"bad_chunk_digest", "bad_region"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, id, raw := openImageFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
			staged := s.browserSessionAction(ctx, imageReq(owner, id, `{"operation":"capture","quality":70,"max_width":640,"max_height":720}`))
			if mode == "bad_region" {
				if staged.OK || staged.ErrorCode != "browser_session_action_uncertain" {
					t.Fatalf("malformed region accepted: %+v", staged)
				}
			} else {
				sum := sha256.Sum256(raw)
				if !staged.OK {
					t.Fatalf("fixture capture failed: %+v", staged)
				}
				req := imageReq(owner, id, fmt.Sprintf(`{"operation":"read","offset":0,"file_version":"sha256:%s"}`, hex.EncodeToString(sum[:])))
				if got := s.browserSessionAction(ctx, req); got.OK || got.ErrorCode != "browser_session_action_uncertain" || got.Output != "" {
					t.Fatalf("bad chunk digest delivered bytes: %+v", got)
				}
			}
			if !s.browserAdmission.matches(id) {
				t.Fatal("unverified Browser action released its admission slot")
			}
			if got := s.browserSessionAction(ctx, imageReq(owner, id, `{"operation":"capture","quality":70,"max_width":640,"max_height":720}`)); got.OK || got.ErrorCode != "browser_session_uncertain" {
				t.Fatalf("unverified session allowed further actions: %+v", got)
			}
		})
	}
}

func TestBrowserImageOlderRunnerAndKnownTooLarge(t *testing.T) {
	for _, mode := range []string{"old_runner", "too_large"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, id, _ := openImageFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
			req := imageReq(owner, id, `{"operation":"capture","quality":70,"max_width":640,"max_height":720}`)
			got := s.browserSessionAction(ctx, req)
			expected := "too_large"
			if mode == "old_runner" {
				expected = "browser_image_unavailable"
			}
			if got.OK || got.ErrorCode != expected || got.Output != "" {
				t.Fatalf("unavailable/oversize falsely delivered: %+v", got)
			}
			if status := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_status")); !status.OK || status.Status != "running" {
				t.Fatalf("known refusal poisoned session: %+v", status)
			}
		})
	}
}
