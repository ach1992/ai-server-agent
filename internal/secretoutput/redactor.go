package secretoutput

import (
	"bytes"
	"io"
	"sync"
)

// New returns a streaming redactor. Supply the plaintext credential and any
// additional encoded wire forms; it cannot detect arbitrary app transformations.
func New(to io.Writer, secret []byte, additional ...[]byte) *Redactor {
	return &Redactor{to: to, secret: secret, additional: additional}
}

// Redactor preserves command diagnostic output while removing exact protected
// credential byte patterns and explicitly known wire encodings.
// Keep the longest possible match across Write boundaries before emitting
// bytes into the inline preview or durable job-output transcript.
type Redactor struct {
	mu         sync.Mutex
	to         io.Writer
	secret     []byte
	additional [][]byte
	pending    []byte
}

func (r *Redactor) patterns() [][]byte {
	patterns := make([][]byte, 0, 1+len(r.additional))
	if len(r.secret) > 0 {
		patterns = append(patterns, r.secret)
	}
	for _, p := range r.additional {
		if len(p) > 0 {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

func (r *Redactor) emit(final bool) error {
	patterns := r.patterns()
	if len(patterns) == 0 {
		_, err := r.to.Write(r.pending)
		r.pending = nil
		return err
	}
	maxLen := 0
	for _, p := range patterns {
		maxLen = max(maxLen, len(p))
	}
	safeEnd := len(r.pending) - maxLen + 1
	if final {
		safeEnd = len(r.pending)
	}
	if safeEnd < 0 {
		safeEnd = 0
	}
	data := r.pending
	pos := 0
	for pos < safeEnd {
		start, matchLen := len(data), 0
		for _, p := range patterns {
			if i := bytes.Index(data[pos:], p); i >= 0 {
				i += pos
				if i < start || (i == start && len(p) > matchLen) {
					start, matchLen = i, len(p)
				}
			}
		}
		if matchLen == 0 || start >= safeEnd {
			if _, err := r.to.Write(data[pos:safeEnd]); err != nil {
				return err
			}
			pos = safeEnd
			break
		}
		if start > pos {
			if _, err := r.to.Write(data[pos:start]); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(r.to, "[REDACTED]"); err != nil {
			return err
		}
		pos = start + matchLen
	}
	r.pending = append(r.pending[:0], data[pos:]...)
	return nil
}

func (r *Redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, p...)
	if err := r.emit(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *Redactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.emit(true)
}
