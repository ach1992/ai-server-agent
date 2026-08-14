package executor

import "time"

type Request struct {
	Token    string `json:"token"`
	Action   string `json:"action"`
	Command  string `json:"command,omitempty"`
	Root     bool   `json:"root,omitempty"`
	Approval bool   `json:"approval,omitempty"`
	JobID    string `json:"job_id,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Path     string `json:"path,omitempty"`
	Content  string `json:"content,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
}

type Response struct {
	OK          bool        `json:"ok"`
	Error       string      `json:"error,omitempty"`
	Output      string      `json:"output,omitempty"`
	ExitCode    int         `json:"exit_code,omitempty"`
	Approval    interface{} `json:"approval,omitempty"`
	JobID       string      `json:"job_id,omitempty"`
	Status      string      `json:"status,omitempty"`
	PID         int         `json:"pid,omitempty"`
	NextOffset  int64       `json:"next_offset,omitempty"`
	GeneratedAt time.Time   `json:"generated_at,omitempty"`
}
