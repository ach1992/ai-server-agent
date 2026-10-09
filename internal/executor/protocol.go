package executor

import "time"

type Request struct {
	Token          string              `json:"token"`
	Action         string              `json:"action"`
	Command        string              `json:"command,omitempty"`
	Root           bool                `json:"root,omitempty"`
	Approval       bool                `json:"approval,omitempty"`
	TimeoutMS      int64               `json:"timeout_ms,omitempty"`
	RequestID      string              `json:"request_id,omitempty"`
	OperationID    string              `json:"operation_id,omitempty"`
	ApprovalID     string              `json:"approval_id,omitempty"`
	AuditAction    string              `json:"audit_action,omitempty"`
	PrincipalID    string              `json:"principal_id,omitempty"`
	PrincipalClass string              `json:"principal_class,omitempty"`
	PrincipalName  string              `json:"principal_name,omitempty"`
	JobID          string              `json:"job_id,omitempty"`
	SessionID      string              `json:"session_id,omitempty"`
	SessionEpoch   string              `json:"session_epoch,omitempty"`
	Cursor         uint64              `json:"cursor,omitempty"`
	Columns        int                 `json:"columns,omitempty"`
	Rows           int                 `json:"rows,omitempty"`
	Offset         int64               `json:"offset,omitempty"`
	Limit          int                 `json:"limit,omitempty"`
	Path           string              `json:"path,omitempty"`
	Content        string              `json:"content,omitempty"`
	Mode           uint32              `json:"mode,omitempty"`
	FileVersion    string              `json:"file_version,omitempty"`
	MustNotExist   bool                `json:"must_not_exist,omitempty"`
	RepositoryPath string              `json:"repository_path,omitempty"`
	WorktreePath   string              `json:"worktree_path,omitempty"`
	Branch         string              `json:"branch,omitempty"`
	StartRef       string              `json:"start_ref,omitempty"`
	ExpectedSHA    string              `json:"expected_sha,omitempty"`
	RemoteIdentity string              `json:"remote_identity,omitempty"`
	RemoteName     string              `json:"remote_name,omitempty"`
	RemoteBranch   string              `json:"remote_branch,omitempty"`
	VerifyRemote   bool                `json:"verify_remote,omitempty"`
	Disposable     bool                `json:"disposable,omitempty"`
	CodeMethod     string              `json:"code_method,omitempty"`
	Line           int                 `json:"line,omitempty"`
	Character      int                 `json:"character,omitempty"`
	SearchMode     string              `json:"search_mode,omitempty"`
	Language       string              `json:"language,omitempty"`
	Pattern        string              `json:"pattern,omitempty"`
	Workspace      string              `json:"workspace,omitempty"`
	SearchPaths    []string            `json:"search_paths,omitempty"`
	Globs          []string            `json:"globs,omitempty"`
	SearchLimit    int                 `json:"search_limit,omitempty"`
	Literal        bool                `json:"literal,omitempty"`
	WorkspaceEdits []WorkspaceFileEdit `json:"workspace_edits,omitempty"`
}

type Response struct {
	OK                  bool                       `json:"ok"`
	Error               string                     `json:"error,omitempty"`
	ReasonCode          string                     `json:"reason_code,omitempty"`
	ErrorCode           string                     `json:"error_code"`
	ErrorClass          string                     `json:"error_class"`
	Retryable           bool                       `json:"retryable,omitempty"`
	AuditDegraded       bool                       `json:"audit_degraded,omitempty"`
	AuditError          string                     `json:"audit_error,omitempty"`
	RequestID           string                     `json:"request_id,omitempty"`
	OperationID         string                     `json:"operation_id,omitempty"`
	Output              string                     `json:"output"`
	OutputEncoding      string                     `json:"output_encoding"`
	BytesSeen           int64                      `json:"bytes_seen"`
	BytesReturned       int64                      `json:"bytes_returned"`
	Truncated           bool                       `json:"truncated"`
	OmittedBytes        int64                      `json:"omitted_bytes"`
	HeadBytes           int64                      `json:"head_bytes,omitempty"`
	TailBytes           int64                      `json:"tail_bytes,omitempty"`
	DurationMS          int64                      `json:"duration_ms"`
	TimedOut            bool                       `json:"timed_out"`
	ExitCode            int                        `json:"exit_code"`
	Approval            interface{}                `json:"approval,omitempty"`
	JobID               string                     `json:"job_id,omitempty"`
	SessionID           string                     `json:"session_id,omitempty"`
	SessionEpoch        string                     `json:"session_epoch,omitempty"`
	TerminalEvents      []TerminalOutputEvent      `json:"terminal_events,omitempty"`
	Earliest            uint64                     `json:"earliest,omitempty"`
	Latest              uint64                     `json:"latest,omitempty"`
	NextCursor          uint64                     `json:"next_cursor,omitempty"`
	Running             *bool                      `json:"running,omitempty"`
	Columns             int                        `json:"columns,omitempty"`
	Rows                int                        `json:"rows,omitempty"`
	IdempotentReplay    bool                       `json:"idempotent_replay,omitempty"`
	Status              string                     `json:"status,omitempty"`
	PID                 int                        `json:"pid,omitempty"`
	RequestedOffset     int64                      `json:"requested_offset,omitempty"`
	Offset              *int64                     `json:"offset,omitempty"`
	AvailableFromOffset int64                      `json:"available_from_offset,omitempty"`
	NextOffset          *int64                     `json:"next_offset,omitempty"`
	CurrentEnd          int64                      `json:"current_end,omitempty"`
	FileSize            *int64                     `json:"file_size,omitempty"`
	FileVersion         string                     `json:"file_version,omitempty"`
	EOF                 *bool                      `json:"eof,omitempty"`
	RetentionTruncated  bool                       `json:"retention_truncated,omitempty"`
	Repository          *RepositoryState           `json:"repository,omitempty"`
	Repositories        []RepositoryState          `json:"repositories,omitempty"`
	RepositoryErrors    []RepositoryDiscoveryError `json:"repository_errors,omitempty"`
	DiscoveryTruncated  bool                       `json:"discovery_truncated,omitempty"`
	GeneratedAt         time.Time                  `json:"generated_at,omitempty"`
}

func int64Ptr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }
