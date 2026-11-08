package executor

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func newSyncOutputTestServer(t *testing.T) *Server {
	t.Helper()
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "jobs"), 0711); err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: config.Config{StateDir: state}, workerUID: uint32(os.Geteuid())}
}

func TestSyncOutputRecoversMiddleViaExistingJobOffsetReader(t *testing.T) {
	s := newSyncOutputTestServer(t)
	raw := append([]byte("HEAD\x00\xff"), bytes.Repeat([]byte("middle!"), 200000)...)
	raw = append(raw, []byte("\xfeTAIL")...)
	capture := newSyncOutputCapture(s)
	for off := 0; off < len(raw); off += 17777 {
		end := off + 17777
		if end > len(raw) {
			end = len(raw)
		}
		if _, err := capture.Write(raw[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	var resp Response
	applyOutputResult(&resp, capture.preview.Result())
	if err := capture.Finish(&resp, 32768); err != nil {
		t.Fatal(err)
	}
	if resp.JobID == "" || !resp.Truncated || resp.BytesReturned != maxSyncOutputBytes || resp.BytesSeen != int64(len(raw)) {
		t.Fatalf("no recoverable preview: job_id=%s bytes_seen=%d returned=%d truncated=%t", resp.JobID, resp.BytesSeen, resp.BytesReturned, resp.Truncated)
	}
	var restored []byte
	offset := int64(0)
	for offset < int64(len(raw)) {
		part := s.jobOutputBounded(Request{JobID: resp.JobID, Offset: offset, Limit: 2048})
		if !part.OK || part.NextOffset == nil || *part.NextOffset <= offset || part.RetentionTruncated {
			t.Fatalf("read at %d: %+v", offset, part)
		}
		decoded := []byte(part.Output)
		if part.OutputEncoding == "base64" {
			var err error
			decoded, err = base64.StdEncoding.DecodeString(part.Output)
			if err != nil {
				t.Fatal(err)
			}
		}
		restored = append(restored, decoded...)
		offset = *part.NextOffset
	}
	if !bytes.Equal(restored, raw) {
		t.Fatalf("middle output lost: got %d bytes, want %d", len(restored), len(raw))
	}
}

func TestSyncOutputSpoolsTextOnlyFallbackWithoutTruncating(t *testing.T) {
	s := newSyncOutputTestServer(t)
	capture := newSyncOutputCapture(s)
	raw := []byte(strings.Repeat("BEGIN-", 9000))
	if _, err := capture.Write(raw); err != nil {
		t.Fatal(err)
	}
	var resp Response
	applyOutputResult(&resp, capture.preview.Result())
	if resp.Truncated {
		t.Fatal("small output unexpectedly truncated")
	}
	if err := capture.Finish(&resp, 32768); err != nil {
		t.Fatal(err)
	}
	if resp.JobID == "" {
		t.Fatal("no recovery ref for oversized text-only response")
	}
	part := s.jobOutputBounded(Request{JobID: resp.JobID, Limit: len(raw)})
	if !part.OK || part.Output != string(raw) || part.EOF == nil || !*part.EOF {
		t.Fatalf("recovery incomplete: ok=%t bytes=%d", part.OK, part.BytesReturned)
	}
}

func TestSyncOutputKeepsSmallResultInline(t *testing.T) {
	s := newSyncOutputTestServer(t)
	capture := newSyncOutputCapture(s)
	if _, err := capture.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	var resp Response
	applyOutputResult(&resp, capture.preview.Result())
	if err := capture.Finish(&resp, 32768); err != nil || resp.JobID != "" || resp.Output != "hello" {
		t.Fatalf("small response must remain inline: %+v %v", resp, err)
	}
}

func TestSyncOutputOverCapacityLeavesNoPartialRecovery(t *testing.T) {
	s := newSyncOutputTestServer(t)
	runtime := config.DefaultRuntimeSettings()
	runtime.SyncOutputSpoolMiB = 8
	s.cfg.Runtime = &runtime
	capture := newSyncOutputCapture(s)
	if _, err := capture.Write(bytes.Repeat([]byte("A"), 2<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Write(bytes.Repeat([]byte("B"), 7<<20)); !errors.Is(err, errSyncOutputLimit) {
		t.Fatalf("expected explicit output cap error, got %v", err)
	}
	var resp Response
	applyOutputResult(&resp, capture.preview.Result())
	if err := capture.Finish(&resp, 32768); !errors.Is(err, errSyncOutputLimit) || resp.JobID != "" {
		t.Fatalf("partial output advertised as complete: %+v %v", resp, err)
	}
	items, err := os.ReadDir(filepath.Join(s.cfg.StateDir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasSuffix(item.Name(), ".log") || strings.HasSuffix(item.Name(), ".status") {
			t.Fatalf("partial output artifact left behind: %s", item.Name())
		}
	}
}
