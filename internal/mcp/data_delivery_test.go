package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func fallbackText(t *testing.T, resp executor.Response) (string, executor.Response) {
	t.Helper()
	result, structured, err := responseResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("text fallback content count = %d", len(result.Content))
	}
	content, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("text fallback type = %T", result.Content[0])
	}
	if result.IsError != !resp.OK {
		t.Fatalf("IsError = %t for ok = %t", result.IsError, resp.OK)
	}
	return content.Text, structured
}

func TestLargeTextFallbackKeepsFileContinuation(t *testing.T) {
	next, size, eof := int64(65536), int64(512<<20), false
	raw := strings.Repeat("SENSITIVE_FILE_CONTENT_", 4096)
	resp := executor.Response{
		OK: true, Status: "read", Output: raw, OutputEncoding: "utf-8",
		BytesSeen: 65536, BytesReturned: 65536, FileSize: &size,
		FileVersion: "f1-0123456789", RequestedOffset: 0,
		NextOffset: &next, EOF: &eof, Truncated: true, OmittedBytes: size - next,
	}
	summary, structured := fallbackText(t, resp)
	for _, token := range []string{
		"file_size=536870912", `file_version="f1-0123456789"`,
		"requested_offset=0", "next_offset=65536", "eof=false", "re-request",
		`output_encoding="utf-8"`, "limit<=4096",
		"structuredContent",
	} {
		if !strings.Contains(summary, token) {
			t.Errorf("fallback is missing %q: %s", token, summary)
		}
	}
	if strings.Contains(summary, "SENSITIVE_FILE_CONTENT_") || len(summary) > 1024 {
		t.Fatalf("large fallback must omit payload bytes and remain small, got len=%d", len(summary))
	}
	if structured.Output != raw {
		t.Fatal("structured content must preserve the existing complete bounded output")
	}
}

func TestLargeTextFallbackKeepsJobRetentionMetadata(t *testing.T) {
	next, eof := int64(16384), false
	resp := executor.Response{
		OK: true, JobID: "1234567890",
		Output: strings.Repeat("LOG_BINARY", 9000), OutputEncoding: "base64",
		BytesReturned: 8192, RequestedOffset: 0, NextOffset: &next,
		AvailableFromOffset: 8192, CurrentEnd: 25000,
		RetentionTruncated: true, EOF: &eof,
	}
	summary, _ := fallbackText(t, resp)
	for _, token := range []string{
		`job_id="1234567890"`, "requested_offset=0",
		"next_offset=16384", "available_from_offset=8192",
		"current_end=25000", "retention_truncated=true",
		`output_encoding="base64"`,
	} {
		if !strings.Contains(summary, token) {
			t.Errorf("fallback missing %q: %s", token, summary)
		}
	}
	if strings.Contains(summary, "LOG_BINARY") || len(summary) > 1024 {
		t.Fatal("job text fallback unexpectedly contains output or is too large")
	}
}

func TestLargeCommandOutputNotAdvertisedAsResumable(t *testing.T) {
	resp := executor.Response{
		OK: true, Output: strings.Repeat("command-output", 4096),
		OutputEncoding: "utf-8", BytesSeen: 100000, BytesReturned: 65536,
		Truncated: true, OmittedBytes: 34464, ExitCode: 0,
	}
	summary, _ := fallbackText(t, resp)
	for _, token := range []string{"bytes_seen=100000", "omitted_bytes=34464", "not resumable", "start_job"} {
		if !strings.Contains(summary, token) {
			t.Errorf("command fallback missing %q", token)
		}
	}
	if strings.Contains(summary, "next_offset=") {
		t.Fatal("non-resumable command output must not expose a synthetic cursor")
	}
}

func TestSmallTextFallbackKeepsExistingResponseJSON(t *testing.T) {
	resp := executor.Response{OK: true, Output: "small full content", OutputEncoding: "utf-8", BytesReturned: 18}
	body, structured := fallbackText(t, resp)
	var decoded executor.Response
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("small fallback no longer returns response JSON: %v", err)
	}
	if decoded.Output != resp.Output || structured.Output != resp.Output {
		t.Fatalf("small fallback lost output: %+v", decoded)
	}
}

func TestTypicalChunkFitsTextOnlyFallback(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		encoding string
	}{
		{name: "utf8", output: strings.Repeat("x", 16384), encoding: "utf-8"},
		{name: "binary", output: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 16384)), encoding: "base64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := executor.Response{OK: true, Output: tc.output, OutputEncoding: tc.encoding, BytesReturned: 16384}
			body, _ := fallbackText(t, resp)
			var decoded executor.Response
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("recommended chunk omitted from text-only fallback: %v", err)
			}
			if decoded.Output != tc.output {
				t.Fatal("recommended chunk missing or corrupted")
			}
		})
	}
}

func TestTextFallbackBoundsJSONEscapeExpansion(t *testing.T) {
	// 16 KiB of UTF-8 control bytes would expand to more than 96 KiB
	// once escaped as JSON; no client should get that entire text fallback.
	raw := strings.Repeat(string([]byte{0}), 16<<10)
	next, size := int64(len(raw)), int64(len(raw)*2)
	summary, structured := fallbackText(t, executor.Response{
		OK: true, Output: raw, OutputEncoding: "utf-8",
		BytesReturned: int64(len(raw)), NextOffset: &next, FileSize: &size,
		FileVersion: "f1-test",
	})
	if len(summary) > 1024 || strings.Contains(summary, "\\u0000") {
		t.Fatalf("expanded raw bytes escaped into text fallback, len=%d", len(summary))
	}
	if !strings.Contains(summary, "limit<=4096") || structured.Output != raw {
		t.Fatal("escaped result lost structured payload or text-only recovery guidance")
	}

	// Even worst-case JSON escaping of a deliberately small 4 KiB range
	// stays inside the presentation budget and remains fully recoverable.
	chunk := strings.Repeat(string([]byte{0}), 4096)
	text, _ := fallbackText(t, executor.Response{OK: true, Output: chunk, OutputEncoding: "utf-8", BytesReturned: 4096})
	if len(text) > 32<<10 {
		t.Fatalf("serialized small range exceeds fallback budget: %d", len(text))
	}
	var decoded executor.Response
	if err := json.Unmarshal([]byte(text), &decoded); err != nil || decoded.Output != chunk {
		t.Fatalf("small escaped range not fully recoverable: %v", err)
	}
}
