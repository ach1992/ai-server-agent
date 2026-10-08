package executor

import "time"

type Request struct {
	Token          string `json:"token"`
	Action         string `json:"action"`
	Command        string `json:"command,omitempty"`
	Root           bool   `json:"root,omitempty"`
	Approval       bool   `json:"approval,omitempty"`
	TimeoutMS      int64  `json:"timeout_ms,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
	PrincipalID    string `json:"principal_id,omitempty"`
	PrincipalClass string `json:"principal_class,omitempty"`
	PrincipalName  string `json:"principal_name,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	Offset         int64  `json:"offset,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Path           string `json:"path,omitempty"`
	Content        string `json:"content,omitempty"`
	Mode           uint32 `json:"mode,omitempty"`
	FileVersion    string `json:"file_version,omitempty"`
	MustNotExist   bool   `json:"must_not_exist,omitempty"`
}ackage executor

import "time"

type Request struct {
	Token        string `json:"token"`
	Action       string `json:"action"`
	Command      string `json:"command,omitempty"`
	Root         bool   `json:"root,omitempty"`
	Approval     bool   `json:"approval,omitempty"`
	TimeoutMS    int64  `json:"timeout_ms,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
	PrincipalID   string `json:"principal_id,omitempty"`
	PrincipalClass string `json:"principal_class,omitempty"`
	PrincipalName string `json:"principal_name,omitempty"`
	JobID        string `json:"job_id,omitempty"`
	Offset       int64  `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	Path         string `json:"path,omitempty"`
	Content      string `json:"content,omitempty"`
	Mode         uint32 `json:"mode,omitempty"`
	FileVersion  string `json:"file_version,omitempty"`
	MustNotExist bool   `json:"must_not_exist,omitempty"`
}

type Response struct {
	OK                  bool        `json:"ok"`
	Error               string      `json:"error,omitempty"`
	ReasonCode          string      `json:"reason_code,omitempty"`
	ErrorCode           string      `json:"error_code"`
	ErrorClass          string      `json:"error_class"`
	Retryable           bool        `json:"retryable,omitempty"`
	Output              string      `json:"output"`
	OutputEncoding      string      `json:"output_encoding"`
	BytesSeen           int64       `json:"bytes_seen"`
	BytesReturned       int64       `json:"bytes_returned"`
	Truncated           bool        `json:"truncated"`
	OmittedBytes        int64       `json:"omitted_bytes"`
	HeadBytes           int64       `json:"head_bytes,omitempty"`
	TailBytes           int64       `json:"tail_bytes,omitempty"`
	DurationMS          int64       `json:"duration_ms"`
	TimedOut            bool        `json:"timed_out"`
	ExitCode            int         `json:"exit_code"`
	Approval            interface{} `json:"approval,omitempty"`
	JobID               string      `json:"job_id,omitempty"`
	IdempotentReplay    bool        `json:"idempotent_replay,omitempty"`
	Status              string      `json:"status,omitempty"`
	PID                 int         `json:"pid,omitempty"`
	RequestedOffset     int64       `json:"requested_offset,omitempty"`
	Offset              *int64      `json:"offset,omitempty"`
	AvailableFromOffset int64       `json:"available_from_offset,omitempty"`
	NextOffset          *int64      `json:"next_offset,omitempty"`
	CurrentEnd          int64       `json:"current_end,omitempty"`
	FileSize            *int64      `json:"file_size,omitempty"`
	FileVersion         string      `json:"file_version,omitempty"`
	EOF                 *bool       `json:"eof,omitempty"`
	RetentionTruncated  bool        `json:"retention_truncated,omitempty"`
	GeneratedAt         time.Time   `json:"generated_at,omitempty"`
}

func int64Ptr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }
