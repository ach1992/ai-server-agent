package executor

import (
	"encoding/base64"
	"fmt"
	"sync"
	"unicode/utf8"
)

const (
	maxCommandBytes       = 256 << 10
	maxSyncOutputBytes    = 1 << 20
	maxJobOutputReadBytes = 1 << 20
)

type boundedOutputCollector struct {
	mu      sync.Mutex
	limit   int
	headCap int
	tailCap int
	head    []byte
	tail    []byte
	seen    int64
}

type boundedOutputResult struct {
	Output        string
	Encoding      string
	BytesSeen     int64
	BytesReturned int64
	Truncated     bool
	OmittedBytes  int64
	HeadBytes     int64
	TailBytes     int64
}

func newBoundedOutputCollector(limit int) *boundedOutputCollector {
	if limit < 2 {
		limit = 2
	}
	headCap := limit / 2
	return &boundedOutputCollector{
		limit:   limit,
		headCap: headCap,
		tailCap: limit - headCap,
		head:    make([]byte, 0, headCap),
		tail:    make([]byte, 0, limit-headCap),
	}
}

func (w *boundedOutputCollector) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(p)
	w.seen += int64(n)

	if len(w.head) < w.headCap {
		take := w.headCap - len(w.head)
		if take > len(p) {
			take = len(p)
		}
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}

	if len(p) >= w.tailCap {
		w.tail = append(w.tail[:0], p[len(p)-w.tailCap:]...)
		return n, nil
	}
	if len(w.tail)+len(p) <= w.tailCap {
		w.tail = append(w.tail, p...)
		return n, nil
	}
	drop := len(w.tail) + len(p) - w.tailCap
	copy(w.tail, w.tail[drop:])
	w.tail = w.tail[:len(w.tail)-drop]
	w.tail = append(w.tail, p...)
	return n, nil
}

func (w *boundedOutputCollector) Result() boundedOutputResult {
	w.mu.Lock()
	defer w.mu.Unlock()

	head := append([]byte(nil), w.head...)
	tail := append([]byte(nil), w.tail...)
	return encodeBoundedOutput(head, tail, w.seen, int64(w.limit))
}

func encodeBoundedOutput(head, tail []byte, seen, limit int64) boundedOutputResult {
	returned := int64(len(head) + len(tail))
	truncated := seen > limit
	if !truncated {
		raw := append(append([]byte(nil), head...), tail...)
		output, encoding := encodeOutputBytes(raw)
		return boundedOutputResult{
			Output:        output,
			Encoding:      encoding,
			BytesSeen:     seen,
			BytesReturned: int64(len(raw)),
			HeadBytes:     int64(len(raw)),
		}
	}

	omitted := seen - returned
	if utf8.Valid(head) && utf8.Valid(tail) {
		return boundedOutputResult{
			Output:        string(head) + fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted) + string(tail),
			Encoding:      "utf-8",
			BytesSeen:     seen,
			BytesReturned: returned,
			Truncated:     true,
			OmittedBytes:  omitted,
			HeadBytes:     int64(len(head)),
			TailBytes:     int64(len(tail)),
		}
	}

	raw := append(append([]byte(nil), head...), tail...)
	return boundedOutputResult{
		Output:        base64.StdEncoding.EncodeToString(raw),
		Encoding:      "base64",
		BytesSeen:     seen,
		BytesReturned: returned,
		Truncated:     true,
		OmittedBytes:  omitted,
		HeadBytes:     int64(len(head)),
		TailBytes:     int64(len(tail)),
	}
}

func encodeOutputBytes(raw []byte) (string, string) {
	if utf8.Valid(raw) {
		return string(raw), "utf-8"
	}
	return base64.StdEncoding.EncodeToString(raw), "base64"
}

func applyOutputResult(resp *Response, out boundedOutputResult) {
	resp.Output = out.Output
	resp.OutputEncoding = out.Encoding
	resp.BytesSeen = out.BytesSeen
	resp.BytesReturned = out.BytesReturned
	resp.Truncated = out.Truncated
	resp.OmittedBytes = out.OmittedBytes
	resp.HeadBytes = out.HeadBytes
	resp.TailBytes = out.TailBytes
}

func commandInputError(command string) *Response {
	if len(command) <= maxCommandBytes {
		return nil
	}
	return &Response{
		Error:      fmt.Sprintf("command exceeds the %d-byte tool limit", maxCommandBytes),
		ReasonCode: "input_too_large",
		ErrorCode:  "input_too_large",
		ErrorClass: "validation",
	}
}
