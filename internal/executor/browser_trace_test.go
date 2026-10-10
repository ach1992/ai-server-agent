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

func openTraceFixture(t *testing.T, mode string) (*Server, Request, string, []byte) {
	t.Helper()
	s, owner, _ := browserAuditFixture(t)
	raw := append([]byte{'P', 'K', 3, 4}, bytes.Repeat([]byte("bounded-playwright-zip-window"), 40)...)
	full := sha256.Sum256(raw)
	chunk := sha256.Sum256(raw)
	chunkHash := hex.EncodeToString(chunk[:])
	if mode == "bad_digest" {
		chunkHash = strings.Repeat("0", 64)
	}
	version := hex.EncodeToString(full[:])
	recordFrame := fmt.Sprintf(`{"event":"trace_meta","nonce":"%%s","mime":"application/zip","size":%d,"sha256":"%s","result":{"ok":true}}`, len(raw), version)
	switch mode {
	case "oversize":
		recordFrame = `{"event":"error","nonce":"%s","reason":"trace_too_large","result":{"ok":true}}`
	case "unknown_oversize":
		recordFrame = `{"event":"error","nonce":"%s","reason":"trace_too_large"}`
	case "flow_failed":
		recordFrame = fmt.Sprintf(`{"event":"trace_meta","nonce":"%%s","mime":"application/zip","size":%d,"sha256":"%s","result":{"ok":false,"failed_step":0}}`, len(raw), version)
	}
	shell := fmt.Sprintf(`#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
available=0
while IFS= read -r line; do
  nonce=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  case "$line" in
    *'"type":"close"'*) exit 0 ;;
    *'"type":"trace_record"'*)
      available=1
      printf 'ASA_BROWSER_SESSION %s\n' "$nonce" ;;
    *'"type":"trace_read"'*)
      if [ "$available" -eq 0 ]; then
        printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%%s","reason":"trace_unavailable"}\n' "$nonce"
      elif ! printf '%%s' "$line" | grep -q 'sha256:%s'; then
        printf 'ASA_BROWSER_SESSION {"event":"error","nonce":"%%s","reason":"trace_stale"}\n' "$nonce"
      else
        printf 'ASA_BROWSER_SESSION {"event":"trace_chunk","nonce":"%%s","mime":"application/zip","size":%d,"offset":0,"sha256":"%s","chunk_sha256":"%s","data":"%s"}\n' "$nonce"
      fi ;;
    *'"type":"trace_discard"'*)
      available=0
      printf 'ASA_BROWSER_SESSION {"event":"trace_discarded","nonce":"%%s"}\n' "$nonce" ;;
  esac
done
`, recordFrame, version, len(raw), version, chunkHash, base64.StdEncoding.EncodeToString(raw))
	if err := os.WriteFile(s.browserNodeBinary, []byte(shell), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("open trace fixture: %+v", opened)
	}
	return s, owner, opened.SessionID, raw
}

func traceReq(owner Request, id, content string) Request {
	req := browserOwnerControl(owner, id, "browser_session_trace")
	req.Content = content
	return req
}
func TestBrowserTraceScopedBoundedChunkAndLifecycle(t *testing.T) {
	s, owner, id, raw := openTraceFixture(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	record := traceReq(owner, id, `{"operation":"record","steps":[{"action":"snapshot"}]}`)
	attacker := record
	attacker.PrincipalID = "unrelated-user"
	if got := s.browserSessionAction(ctx, attacker); got.OK || got.ErrorCode != "browser_session_not_found" || got.Output != "" {
		t.Fatalf("foreign principal trace access: %+v", got)
	}
	attacker = record
	attacker.Workspace = filepath.Dir(owner.Workspace)
	if got := s.browserSessionAction(ctx, attacker); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("foreign workspace trace access: %+v", got)
	}
	for _, content := range []string{
		`{"operation":"record","steps":[]}`,
		`{"operation":"record","steps":[{"action":"snapshot"}],"extra":"bad"}`,
		`{"operation":"read","offset":-1,"file_version":"bad"}`,
		`{"operation":"nonsense","steps":[{"action":"snapshot"}]}`,
	} {
		req := traceReq(owner, id, content)
		if got := s.browserSessionAction(ctx, req); got.OK || got.ErrorCode != "invalid_browser_trace" {
			t.Fatalf("invalid trace parameters admitted: %+v", got)
		}
	}
	created := s.browserSessionAction(ctx, record)
	full := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(full[:])
	if !created.OK || created.Status != "recorded" || created.MIMEType != "application/zip" || created.FileVersion != version ||
		created.FileSize == nil || *created.FileSize != int64(len(raw)) || created.BytesReturned != 0 || created.OutputEncoding != "json" {
		t.Fatalf("trace recorded metadata: %+v", created)
	}
	read := traceReq(owner, id, fmt.Sprintf(`{"operation":"read","offset":0,"file_version":%q}`, version))
	got := s.browserSessionAction(ctx, read)
	payload, err := base64.StdEncoding.Strict().DecodeString(got.Output)
	if err != nil || !got.OK || !bytes.Equal(payload, raw) || got.MIMEType != "application/zip" ||
		got.FileVersion != version || got.NextOffset == nil || *got.NextOffset != int64(len(raw)) ||
		got.EOF == nil || !*got.EOF || got.BytesReturned != int64(len(raw)) {
		t.Fatalf("trace window content/version/offset: %+v err=%v", got, err)
	}
	stale := read
	stale.Content = fmt.Sprintf(`{"operation":"read","offset":0,"file_version":"sha256:%s"}`, strings.Repeat("f", 64))
	if denied := s.browserSessionAction(ctx, stale); denied.OK || denied.ErrorCode != "trace_stale" || denied.Output != "" {
		t.Fatalf("stale trace version returned bytes: %+v", denied)
	}
	if after := s.browserSessionAction(ctx, read); !after.OK {
		t.Fatalf("known stale read poisoned session: %+v", after)
	}
	discard := traceReq(owner, id, fmt.Sprintf(`{"operation":"discard","file_version":%q}`, version))
	if discarded := s.browserSessionAction(ctx, discard); !discarded.OK || discarded.Status != "discarded" {
		t.Fatalf("trace discard: %+v", discarded)
	}
	if missing := s.browserSessionAction(ctx, read); missing.OK || missing.ErrorCode != "trace_unavailable" || missing.Output != "" {
		t.Fatalf("discarded trace still delivered: %+v", missing)
	}
}
func TestBrowserTraceMismatchedChunkDigestPoisonsSession(t *testing.T) {
	s, owner, id, raw := openTraceFixture(t, "bad_digest")
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	if created := s.browserSessionAction(ctx, traceReq(owner, id, `{"operation":"record","steps":[{"action":"snapshot"}]}`)); !created.OK {
		t.Fatalf("setup trace: %+v", created)
	}
	h := sha256.Sum256(raw)
	v := "sha256:" + hex.EncodeToString(h[:])
	read := traceReq(owner, id, fmt.Sprintf(`{"operation":"read","offset":0,"file_version":%q}`, v))
	if denied := s.browserSessionAction(ctx, read); denied.OK || denied.ErrorCode != "browser_session_action_uncertain" || denied.Output != "" {
		t.Fatalf("wrong digest accepted: %+v", denied)
	}
	if again := s.browserSessionAction(ctx, read); again.OK || again.ErrorCode != "browser_session_uncertain" {
		t.Fatalf("unknown completion replayable: %+v", again)
	}
	if s.browserAdmission.acquireRun() {
		t.Fatal("poisoned Browser released profile admission")
	}
}

func TestBrowserTraceOversizeCannotBeReplayedOrMistakenForDelivery(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		known bool
	}{
		{"oversize", true},
		{"unknown_oversize", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s, owner, id, _ := openTraceFixture(t, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
			record := traceReq(owner, id, `{"operation":"record","steps":[{"action":"snapshot"}]}`)
			got := s.browserSessionAction(ctx, record)
			if tc.known {
				if got.OK || got.ErrorCode != "too_large" || got.Status != "flow_executed_trace_unavailable" ||
					!strings.Contains(got.Output, `"ok":true`) || got.BytesReturned != 0 {
					t.Fatalf("known oversize incorrectly reported as successful recording: %+v", got)
				}
				state := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_status"))
				if !state.OK || state.Status != "running" {
					t.Fatalf("known size refusal poisoned session: %+v", state)
				}
			} else {
				if got.OK || got.ErrorCode != "browser_session_action_uncertain" || got.Status != "uncertain" {
					t.Fatalf("unverified flow completion must poison session: %+v", got)
				}
				again := s.browserSessionAction(ctx, record)
				if again.OK || again.ErrorCode != "browser_session_uncertain" {
					t.Fatalf("unknown browser flow should not be rerunnable: %+v", again)
				}
			}
		})
	}
}

func TestBrowserTraceKnownFlowFailureRetainsEvidence(t *testing.T) {
	s, owner, id, _ := openTraceFixture(t, "flow_failed")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	recorded := s.browserSessionAction(ctx, traceReq(owner, id, `{"operation":"record","steps":[{"action":"assert_text"}]}`))
	if recorded.OK || recorded.ErrorCode != "browser_flow_failed" || recorded.Status != "recorded_flow_failed" ||
		recorded.FileVersion == "" || !strings.Contains(recorded.Output, `"failed_step":0`) {
		t.Fatalf("failed browser assertion lost retrievable trace: %+v", recorded)
	}
	read := traceReq(owner, id, fmt.Sprintf(`{"operation":"read","offset":0,"file_version":%q}`, recorded.FileVersion))
	if got := s.browserSessionAction(ctx, read); !got.OK || got.BytesReturned <= 0 {
		t.Fatalf("trace of known failed action was lost: %+v", got)
	}
}
