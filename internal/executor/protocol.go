package executor

import "time"

type Request struct {
	Token       string `json:"token"`
	Action      string `json:"action"`
	Command     string `json:"command,omitempty"`
	Root        bool   `json:"root,omitempty"`
	Approval    bool   `json:"approval,omitempty"`
	TimeoutMS   int64  `json:"timeout_ms,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Path        string `json:"path,omitempty"`
	Content     string `json:"content,omitempty"`
	Mode        uint32 `json:"mode,omitempty"`
}

type Response struct {
	OK                  bool        `json:"ok"`
	Error               string      `json:"error,omitempty"`
	ReasonCode          string      `json:"reason_code,omitempty"`
	ErrorCode           string      `json:"error_code,omitempty"`
	ErrorClass          string      `json:"error_class,omitempty"`
	Retryable           bool        `json:"retryable,omitempty"`
	Output              string      `json:"output,omitempty"`
	OutputEncoding      string      `json:"output_encoding,omitempty"`
	BytesSeen           int64       `json:"bytes_seen,omitempty"`
	BytesReturned       int64       `json:"bytes_returned,omitempty"`
	Truncated           bool        `json:"truncated,omitempty"`
	OmittedBytes        int64       `json:"omitted_bytes,omitempty"`
	HeadBytes           int64       `json:"head_bytes,omitempty"`
	TailBytes           int64       `json:"tail_bytes,omitempty"`
	DurationMS          int64       `json:"duration_ms,omitempty"`
	TimedOut            bool        `json:"timed_out,omitempty"`
	ExitCode            int         `json:"exit_code,omitempty"`
	Approval            interface{} `json:"approval,omitempty"`
	JobID               string      `json:"job_id,omitempty"`
	IdempotentReplay    bool        `json:"idempotent_replay,omitempty"`
	Status              string      `json:"status,omitempty"`
	PID                 int         `json:"pid,omitempty"`
	RequestedOffset     int64       `json:"requested_offset,omitempty"`
	AvailableFromOffset int64       `json:"available_from_offset,omitempty"`
	NextOffset          int64       `json:"next_offset,omitempty"`
	CurrentEnd          int64       `json:"current_end,omitempty"`
	EOF                 bool        `json:"eof,omitempty"`
	RetentionTruncated  bool        `json:"retention_truncated,omitempty"`
	GeneratedAt         time.Time   `json:"generated_at,omitempty"`
}
