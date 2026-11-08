package mcp

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/secretoutput"
)

// diagnosticTextFallback is a deliberately small textual view of an unchanged
// structured Executor response. Its budget is measured AFTER quoting/escaping,
// and a reserved truncation marker never gets silently squeezed out.
func diagnosticTextFallback(resp executor.Response, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	const truncatedMarker = " diagnostic_truncated=true"
	budget := maxBytes - len(truncatedMarker)
	if budget < 0 {
		return truncatedMarker[:min(len(truncatedMarker), maxBytes)]
	}
	var b strings.Builder
	shortened := false
	add := func(s string) {
		if len(s) > budget-b.Len() {
			shortened = true
			return
		}
		b.WriteString(s)
	}
	addQuoted := func(key, value string, maxRaw int) {
		value = secretoutput.RedactDiagnosticText(value)
		if len(value) > maxRaw {
			value = value[:maxRaw]
			for !utf8.ValidString(value) {
				value = value[:len(value)-1]
			}
			shortened = true
		}
		prefix := " " + key + "="
		// Limit each quoted field's *encoded* footprint as well as its
		// source length. Otherwise control-byte escape expansion in a
		// single early field could displace later correlation/diagnostic
		// fields even with the continuation envelope already prioritized.
		remaining := min(budget-b.Len()-len(prefix), maxRaw*2+2)
		if remaining < 2 {
			shortened = true
			return
		}
		encoded := strconv.Quote(value)
		if len(encoded) > remaining {
			shortened = true
			// Quote expansion can be up to six bytes per source byte. Shrink
			// safely on a UTF-8 boundary instead of cutting a quoted field.
			for len(encoded) > remaining && len(value) > 0 {
				_, size := utf8.DecodeLastRuneInString(value)
				value = value[:len(value)-size]
				encoded = strconv.Quote(value)
			}
		}
		add(prefix + encoded)
	}
	// Place the actionable continuation envelope before expandable provider
	// text. The minimum 4096-byte budget can fit these bounded identifiers,
	// numeric windows and recovery instructions even with quoted control bytes.
	add(fmt.Sprintf("ok=%t audit_degraded=%t", resp.OK, resp.AuditDegraded))
	add(fmt.Sprintf(" retryable=%t timed_out=%t exit_code=%d", resp.Retryable, resp.TimedOut, resp.ExitCode))
	if resp.JobID != "" {
		addQuoted("job_id", resp.JobID, 64)
	}
	if resp.FileSize != nil {
		add(fmt.Sprintf(" file_size=%d", *resp.FileSize))
	}
	if resp.FileVersion != "" {
		addQuoted("file_version", resp.FileVersion, 120)
	}
	if resp.NextOffset != nil {
		add(fmt.Sprintf(" requested_offset=%d next_offset=%d", resp.RequestedOffset, *resp.NextOffset))
		if resp.EOF != nil {
			add(fmt.Sprintf(" eof=%t", *resp.EOF))
		}
		if resp.JobID != "" {
			add(fmt.Sprintf(" available_from_offset=%d current_end=%d retention_truncated=%t",
				resp.AvailableFromOffset, resp.CurrentEnd, resp.RetentionTruncated))
		}
		// The output budget is raw bytes BEFORE Base64 and JSON encoding.
		recoveryLimit := 4096
		if resp.OutputEncoding == "base64" {
			recoveryLimit = 2048
		}
		add(fmt.Sprintf("; output omitted from text fallback: use structuredContent, or re-request the needed range starting at requested_offset with limit<=%d, then continue via next_offset (keep file_version for files)", recoveryLimit))
	} else if resp.JobID != "" {
		add("; output omitted from text fallback: retrieve the complete command output using job_output(job_id, offset=0, limit<=4096), continuing with next_offset (retention is bounded)")
	} else {
		add("; output omitted from text fallback: use structuredContent; no output recovery ID was returned")
	}
	addQuoted("status", resp.Status, 80)
	addQuoted("error_code", resp.ErrorCode, 80)
	addQuoted("reason_code", resp.ReasonCode, 80)
	addQuoted("error_class", resp.ErrorClass, 80)
	addQuoted("request_id", resp.RequestID, 96)
	addQuoted("operation_id", resp.OperationID, 96)
	add(fmt.Sprintf(" bytes_seen=%d bytes_returned=%d", resp.BytesSeen, resp.BytesReturned))
	addQuoted("output_encoding", resp.OutputEncoding, 64)
	add(fmt.Sprintf(" truncated=%t omitted_bytes=%d", resp.Truncated, resp.OmittedBytes))
	if resp.Error != "" {
		addQuoted("error", resp.Error, 240)
	}
	if resp.AuditDegraded {
		addQuoted("audit_error", resp.AuditError, 160)
		if resp.AuditError == "" {
			add(" audit_recovery=inspect_audit_and_operation_status")
		}
	}
	if shortened {
		b.WriteString(truncatedMarker)
	}
	return b.String()
}
