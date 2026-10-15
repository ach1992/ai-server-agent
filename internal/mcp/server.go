package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/browser"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/manifest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The release packager substitutes its immutable release-version candidate into
// this marker in a temporary source tree. Normal source builds instead report
// their embedded VCS revision, never this old development placeholder.
const version = "0.1.0-dev"

// Synchronous command duration is operator-tunable; longer operations use jobs.

// Stamped by the existing release builder from the exact Git checkout. Go's
// automatic VCS build settings are optional (and absent in some -trimpath
// builds), so never infer a revision from the old development version marker.
var buildRevision string

func mcpImplementationVersion() string {
	info, _ := debug.ReadBuildInfo()
	return versionForBuild(version, buildRevision, info)
}

func versionForBuild(releaseMarker, stampedRevision string, info *debug.BuildInfo) string {
	var revision, modified string
	if stampedRevision != "" {
		revision, modified = stampedRevision, "false"
		if strings.HasSuffix(revision, "-dirty") {
			revision = strings.TrimSuffix(revision, "-dirty")
			modified = "true"
		}
	} else if info != nil {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value
			}
		}
	}
	validRevision := len(revision) == 40 && strings.Trim(revision, "0123456789abcdef") == "" &&
		(modified == "true" || modified == "false")
	if strings.HasSuffix(releaseMarker, "-dev") {
		if !validRevision {
			return "source-unknown"
		}
		if modified == "true" {
			return "source-" + revision + "-dirty"
		}
		return "source-" + revision
	}
	if !validRevision {
		return releaseMarker
	}
	identity := releaseMarker + "+g" + revision
	if modified == "true" {
		identity += ".dirty"
	}
	return identity
}

type Server struct {
	cfg             config.Config
	executorToken   string
	bearerToken     string
	credentialStore *credential.Store
	browser         *browser.Manager
	mcp             *mcpsdk.Server
}

type EmptyInput struct{}
type RunInput struct {
	Command   string `json:"command" jsonschema:"Bash command to execute"`
	Workspace string `json:"workspace,omitempty" jsonschema:"Optional existing worker directory within configured workspace root; defaults to workspace root; not a filesystem sandbox"`
}
type RootRunInput struct {
	Command  string `json:"command" jsonschema:"Bash command to execute as root"`
	Approval bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when a previous call returned approval_required"`
}
type StartJobInput struct {
	Workspace   string `json:"workspace,omitempty" jsonschema:"Optional existing worker CWD under workspace root; forbidden for root jobs"`
	Kind        string `json:"kind,omitempty" jsonschema:"task (default) or service; service is a long-running foreground process"`
	Label       string `json:"label,omitempty" jsonschema:"Short non-secret purpose; required for service, optional for task"`
	Command     string `json:"command" jsonschema:"Command to run as a persistent background job"`
	Root        bool   `json:"root,omitempty" jsonschema:"Run as root instead of aiworker"`
	Approval    bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when a previous call returned approval_required"`
	OperationID string `json:"operation_id,omitempty" jsonschema:"Optional caller-generated idempotency key; retry the same material request with the same key after a lost response"`
}
type RuntimeListInput struct {
	Workspace string `json:"workspace,omitempty" jsonschema:"Optional exact canonical existing worker directory filter"`
	Kind      string `json:"kind,omitempty" jsonschema:"Optional task, service or terminal filter"`
}
type JobInput struct {
	JobID string `json:"job_id" jsonschema:"Persistent job id"`
}
type JobOutputInput struct {
	JobID  string `json:"job_id" jsonschema:"Persistent job id"`
	Offset int64  `json:"offset,omitempty" jsonschema:"Byte offset to start reading at"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum bytes to read; maximum 1048576"`
}
type ReadFileInput struct {
	Path        string `json:"path" jsonschema:"Absolute host path"`
	Offset      int64  `json:"offset,omitempty" jsonschema:"Raw byte offset to start reading at"`
	Limit       int    `json:"limit,omitempty" jsonschema:"Maximum raw bytes to read; default and maximum 1048576"`
	FileVersion string `json:"file_version,omitempty" jsonschema:"Optional consistency token returned by an earlier read; mismatches fail with file_changed"`
	Approval    bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when reading an Agent-protected resource"`
}
type WriteFileInput struct {
	Path         string `json:"path" jsonschema:"Absolute host path; parent directory must already exist"`
	Content      string `json:"content" jsonschema:"Complete UTF-8 replacement file content; maximum 1048576 bytes"`
	Mode         uint32 `json:"mode,omitempty" jsonschema:"Optional Unix permission/special bits as decimal; existing mode is preserved when omitted, new files default to 0644"`
	FileVersion  string `json:"file_version,omitempty" jsonschema:"Optional optimistic precondition from an earlier read; mismatches fail with file_changed"`
	MustNotExist bool   `json:"must_not_exist,omitempty" jsonschema:"Require the destination not to exist; conflicts with file_version"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when writing an Agent-protected resource"`
}
type BrowserSetupInput struct {
	Approval bool `json:"approval,omitempty" jsonschema:"Set true to allow downloading the private browser runtime and installing required shared libraries"`
}
type BrowserRunInput struct {
	ProfileMode       string `json:"profile_mode,omitempty" jsonschema:"shared (default) or isolated ephemeral cookies/localStorage; same global exclusive Browser admission"`
	Script            string `json:"script" jsonschema:"JavaScript statements using the pre-created Playwright browser, context, and page variables; maximum 131072 raw bytes"`
	TimeoutMS         int64  `json:"timeout_ms,omitempty" jsonschema:"Browser execution timeout in milliseconds; default 90000, maximum 300000"`
	IgnoreHTTPSErrors bool   `json:"ignore_https_errors,omitempty" jsonschema:"Explicit scoped exception for local/self-signed development; default false preserves normal HTTPS certificate validation"`
}

type BrowserE2EInput struct {
	ProfileMode       string             `json:"profile_mode,omitempty" jsonschema:"shared (default) or isolated ephemeral Browser state"`
	Steps             []browser.FlowStep `json:"steps" jsonschema:"1..12 ordered steps: goto, snapshot (returns bounded eN refs), click/fill/assert_text by ref or role/name or CSS, assert_url, console, network. Refs are flow-only, never cross-call."`
	TimeoutMS         int64              `json:"timeout_ms,omitempty" jsonschema:"Total flow timeout milliseconds; default 90000, maximum 300000"`
	IgnoreHTTPSErrors bool               `json:"ignore_https_errors,omitempty" jsonschema:"Explicit scoped exception for self-signed development; default false"`
}

func New(cfg config.Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	et, err := os.ReadFile(cfg.ExecutorToken)
	if err != nil {
		return nil, fmt.Errorf("read executor token: %w", err)
	}
	executorToken := strings.TrimSpace(string(et))
	if executorToken == "" {
		return nil, errors.New("executor token is empty")
	}
	var bearerToken string
	var credentialStore *credential.Store
	if cfg.CredentialStoreFile != "" {
		credentialStore, err = credential.Load(cfg.CredentialStoreFile)
		if err != nil {
			return nil, err
		}
	} else {
		b, readErr := os.ReadFile(cfg.BearerTokenFile)
		if readErr != nil {
			return nil, fmt.Errorf("read MCP bearer token: %w", readErr)
		}
		bearerToken = strings.TrimSpace(string(b))
		if bearerToken == "" {
			return nil, errors.New("MCP bearer token is empty")
		}
	}

	s := &Server{
		cfg:             cfg,
		executorToken:   executorToken,
		bearerToken:     bearerToken,
		credentialStore: credentialStore,
		browser:         browser.New(cfg, executorToken),
	}
	s.mcp = mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "ai-server-agent", Version: mcpImplementationVersion()},
		&mcpsdk.ServerOptions{
			Instructions: instructions(cfg),
			Capabilities: &mcpsdk.ServerCapabilities{},
		},
	)
	s.mcp.AddReceivingMiddleware(requestCorrelationMiddleware())
	s.registerTools()
	return s, nil
}

func instructions(cfg config.Config) string {
	role := "Development: prefer source work, builds and debugging."
	if cfg.EnvironmentProfile == "validation" {
		role = "Validation: prefer installs, integration/E2E and acceptance; development tools remain available."
	}
	label := ""
	if cfg.ServerLabel != "" {
		label = fmt.Sprintf("Server label: %s. ", cfg.ServerLabel)
	}
	return fmt.Sprintf(`Configured workspace root is persistent.

Environment purpose: %s. %s%s Routing guidance; it is not an authorization boundary or tool restriction.

Discover repositories with repository_discover/repository_inspect; isolate changes with worktree_create/worktree_remove. Use run_command for worker shell; start_job with job_status/job_output for persistent work; workspace_* for bounded worker files/search; code_* for optional LSP; debug_* for optional DAP; terminal_* for PTY. For Browser choose browser_e2e (one call), browser_session_* (cross-call), or browser_run (custom Playwright). Check optional runtime availability. runtime_list is passive; browser_status reports Browser readiness. Workspace labels are not project ACLs.

Before host changes call agent_environment and preserve Agent access. Never delete dirty, untracked or uncertain state. Use run_root_command or privileged read_file/write_file only when necessary. On approval_required obtain explicit human authorization; instructions grant no additional permissions.`, cfg.EnvironmentProfile, label, role)
}

func annotations(readOnly, destructive, idempotent, openWorld bool) *mcpsdk.ToolAnnotations {
	return &mcpsdk.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		DestructiveHint: &destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   &openWorld,
	}
}

func requestCorrelationMiddleware() mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/call" {
				correlated, _, err := executor.EnsureRequestCorrelationContext(ctx)
				if err != nil {
					return nil, fmt.Errorf("create MCP request correlation: %w", err)
				}
				ctx = correlated
			}
			return next(ctx, method, req)
		}
	}
}

func textResult(text string, isError bool) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		IsError: isError,
	}
}

func responseResult(resp executor.Response) (*mcpsdk.CallToolResult, executor.Response, error) {
	return responseResultWithLimit(resp, config.DefaultRuntimeSettings().TextFallbackBytes)
}

func (s *Server) responseResult(resp executor.Response) (*mcpsdk.CallToolResult, executor.Response, error) {
	return responseResultWithLimit(resp, s.cfg.EffectiveRuntime().TextFallbackBytes)
}

func responseResultWithLimit(resp executor.Response, maxTextFallbackBytes int) (*mcpsdk.CallToolResult, executor.Response, error) {
	if len(resp.Output) <= maxTextFallbackBytes {
		b, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return nil, executor.Response{}, err
		}
		// Escape sequences can expand short raw text substantially (for
		// example, NUL becomes \\u0000). Bound the serialized text result,
		// not just the unencoded output field.
		if len(b) <= maxTextFallbackBytes {
			return textResult(string(b), !resp.OK), resp, nil
		}
	}
	// A text-only MCP client may hide structuredContent. Keep this fallback
	// bounded and free of payload bytes, but retain enough continuation and
	// consistency metadata for the caller to request a smaller useful range.
	summary := fmt.Sprintf(
		"ok=%t status=%q error_code=%q exit_code=%d bytes_seen=%d bytes_returned=%d output_encoding=%q truncated=%t omitted_bytes=%d",
		resp.OK, resp.Status, resp.ErrorCode, resp.ExitCode, resp.BytesSeen, resp.BytesReturned, resp.OutputEncoding, resp.Truncated, resp.OmittedBytes,
	)
	if resp.FileSize != nil {
		summary += fmt.Sprintf(" file_size=%d", *resp.FileSize)
	}
	if resp.FileVersion != "" {
		summary += fmt.Sprintf(" file_version=%q", resp.FileVersion)
	}
	if resp.JobID != "" {
		summary += fmt.Sprintf(" job_id=%q", resp.JobID)
	}
	if resp.NextOffset != nil {
		summary += fmt.Sprintf(" requested_offset=%d next_offset=%d", resp.RequestedOffset, *resp.NextOffset)
		if resp.EOF != nil {
			summary += fmt.Sprintf(" eof=%t", *resp.EOF)
		}
		if resp.JobID != "" {
			summary += fmt.Sprintf(" available_from_offset=%d current_end=%d retention_truncated=%t",
				resp.AvailableFromOffset, resp.CurrentEnd, resp.RetentionTruncated)
		}
		summary += "; output omitted from text fallback: use structuredContent, or re-request the needed range starting at requested_offset with limit<=4096, then continue via next_offset (keep file_version for files)"
	} else {
		summary += "; output omitted from text fallback: use structuredContent; this output is not resumable, request focused output or use start_job for future high-output commands"
	}
	return textResult(summary, !resp.OK), resp, nil
}

func executorTransportErrorResult(err error) (*mcpsdk.CallToolResult, executor.Response, error) {
	resp := executor.Response{
		Error:      err.Error(),
		ReasonCode: "executor_transport",
		ErrorCode:  "executor_transport",
		ErrorClass: "transport",
		Retryable:  true,
	}
	return textResult(err.Error(), true), resp, nil
}

func (s *Server) commandTimeoutMS() int64 {
	return int64(s.cfg.EffectiveRuntime().CommandTimeoutSeconds) * 1000
}

func (s *Server) commandTimeoutDescription() string {
	// Tool metadata is part of the client contract. The value advertised to
	// the model must match the timeout effective for this Agent instance.
	return fmt.Sprintf("Synchronous execution is limited to %d seconds for this Agent (operator-configured, default 300 seconds, supported 10..1800 seconds); use start_job for longer or high-output work.", s.cfg.EffectiveRuntime().CommandTimeoutSeconds)
}

func (s *Server) registerTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "agent_environment",
		Description: "Read the AI Server Agent self-preservation manifest. Call this before host-wide package, service, firewall, network, disk, user, web-stack, or control-panel changes.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input EmptyInput) (*mcpsdk.CallToolResult, any, error) {
		b, err := json.MarshalIndent(manifest.Build(s.cfg), "", "  ")
		if err != nil {
			return nil, nil, err
		}
		return textResult(string(b), false), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_command",
		Description: "Run Bash as aiworker (not root) with optional workspace CWD (default: configured workspace root); may modify files or external systems. " + s.commandTimeoutDescription(),
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RunInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: input.Command, Workspace: input.Workspace, TimeoutMS: s.commandTimeoutMS()})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_root_command",
		Description: "Run Bash as root for host-level actions (packages, services, networking). Can change access and running services. " + s.commandTimeoutDescription() + " High-risk or destructive commands may return approval_required; obtain explicit user consent and pass approval=true.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RootRunInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: input.Command, Root: true, Approval: input.Approval, TimeoutMS: s.commandTimeoutMS()})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "start_job",
		Description: "Start a persistent worker task or labeled service (optionally authorized root) through systemd; it survives MCP disconnect. Provide stable operation_id to recover uncertain launches via job_status; do not retry with a new ID. A successful launch does not prove service readiness. Root jobs cannot use worker CWD.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input StartJobInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "start_job", Command: input.Command, Workspace: input.Workspace, Kind: input.Kind, Label: input.Label, Root: input.Root, Approval: input.Approval, OperationID: input.OperationID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "runtime_list", Description: "Read-only listing of this principal's tasks/services and worker Terminals, optionally by workspace/kind. Includes truncated/unknown status; use browser_status for Browser.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input RuntimeListInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "runtime_list", Workspace: input.Workspace, Kind: input.Kind})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_status", Description: "Read and reconcile the current state and exit status of a persistent job. Reconciliation may persist an unknown-completion marker and retire a stale protected command handoff after the transient unit is gone.", Annotations: annotations(false, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_status", JobID: input.JobID})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_output", Description: "Read a chunk of persistent job stdout/stderr without requiring the original MCP connection to remain open.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobOutputInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_output", JobID: input.JobID, Offset: input.Offset, Limit: input.Limit})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_stop", Description: "Stop a persistent background job.", Annotations: annotations(false, true, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_stop", JobID: input.JobID})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "read_file", Description: "Read bounded host-file bytes with root-readable authority. Protected aliases require approval; binary response uses base64. Not a worker sandbox.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input ReadFileInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "read_file", Path: input.Path, Offset: input.Offset, Limit: input.Limit, FileVersion: input.FileVersion, Root: true, Approval: input.Approval})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "write_file", Description: "Atomically write a host regular file with root-capable authority. Parent must exist; optional file_version/must_not_exist guards lost updates; protected paths require approval.", Annotations: annotations(false, true, false, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input WriteFileInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "write_file", Path: input.Path, Content: input.Content, Root: true, Mode: input.Mode, FileVersion: input.FileVersion, MustNotExist: input.MustNotExist, Approval: input.Approval})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_status", Description: "Inspect bounded non-secret browser runtime readiness and pinned Node/Playwright/Chromium versions. The persistent browser profile is Agent-wide shared state, not per-client or per-user isolation.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input EmptyInput) (*mcpsdk.CallToolResult, browser.RuntimeStatus, error) {
			status := s.browser.Status(ctx)
			return textResult(fmt.Sprintf("installed=%t ready=%t busy=%t shared_profile=%t reason=%q", status.Installed, status.Ready, status.Busy, status.SharedProfile, status.Reason), false), status, nil
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_setup", Description: "Install/repair pinned optional root-owned Node/Playwright/Chromium Browser runtime; may install OS libraries. Preserves shared profile, system Node and ports 80/443.", Annotations: annotations(false, true, true, true)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserSetupInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := s.browser.Setup(ctx, input.Approval)
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_run", Description: "Run bounded Playwright JavaScript with pre-created browser/context/page; console.log reports observations. Uses shared persistent profile by default (cookies may persist) or profile_mode=isolated for disposable state. HTTPS certificate validation is enabled by default; ignore_https_errors is a per-call exception. Can cause page actions.", Annotations: annotations(false, true, false, true)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserRunInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := s.browser.Run(ctx, browser.RunOptions{Script: input.Script, TimeoutMS: input.TimeoutMS, IgnoreHTTPSErrors: input.IgnoreHTTPSErrors, ProfileMode: input.ProfileMode})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "browser_e2e", Description: "Execute bounded Browser steps (navigate/snapshot/click/fill/assert/console/network) in one browser execution, not a cross-call live session. Uses the same pinned Chromium, exclusive admission, TLS/resource limits and shared-by-default or isolated ephemeral profile. Snapshots are limited to 8192 UTF-8 bytes; stable eN refs work only within this call; refs expire on navigation, detachment or call end. Use browser_run for custom Playwright and browser_session_flow for cross-call sessions. No image/trace bytes.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserE2EInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.Flow(ctx, browser.FlowOptions{Steps: input.Steps, TimeoutMS: input.TimeoutMS, IgnoreHTTPSErrors: input.IgnoreHTTPSErrors, ProfileMode: input.ProfileMode})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	s.registerBrowserSessionTools()
	s.registerRepositoryEnvironmentTool()
	s.registerWorkspaceFileTools()
	s.registerTerminalTools()
	s.registerCodeTools()
	s.registerDebugTools()
	s.registerRepositoryTools()
	s.registerWorkspaceSearchTool()
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-server-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(h, prefix))
		principal := credential.Principal{ID: "direct-default", Class: "direct", Name: "direct/default"}
		authorized := false
		if s.credentialStore != nil {
			principal, authorized = s.credentialStore.Authenticate(got)
		} else {
			want := s.bearerToken
			authorized = len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-server-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := credential.WithPrincipal(r.Context(), principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"ai-server-agent"}`))
	})
	mux.Handle("/agent-environment.json", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifest.Build(s.cfg))
	})))

	streamable := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		return s.mcp
	}, &mcpsdk.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          8 << 20,
		PropagateRequestCancellation: true,
	})
	originProtection := http.NewCrossOriginProtection()
	mux.Handle(s.cfg.MCPPath, s.auth(originProtection.Handler(streamable)))
	return mux
}

// Both plain HTTP and native TLS use exactly the same validated timeout
// profile. These are network idle/header budgets, not MCP credential or
// executor session expiry.
func newHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	r := cfg.EffectiveRuntime()
	return &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(r.HTTPReadHeaderTimeoutSeconds) * time.Second,
		IdleTimeout:       time.Duration(r.HTTPIdleTimeoutSeconds) * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func Serve(cfg config.Config) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0750); err != nil {
		return err
	}
	if err := manifest.Write(filepath.Join(cfg.StateDir, "AI_ENVIRONMENT.json"), manifest.Build(cfg)); err != nil {
		return err
	}
	httpServer := newHTTPServer(cfg, s.Handler())
	fmt.Printf("ai-server-agent listening on %s%s\n", cfg.ListenAddress, cfg.MCPPath)
	return httpServer.ListenAndServe()
}
