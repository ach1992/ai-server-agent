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
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/browser"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/manifest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0-dev"
const synchronousCommandTimeout = 5 * time.Minute

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
	Command string `json:"command" jsonschema:"Bash command to execute"`
}
type RootRunInput struct {
	Command  string `json:"command" jsonschema:"Bash command to execute as root"`
	Approval bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when a previous call returned approval_required"`
}
type StartJobInput struct {
	Command     string `json:"command" jsonschema:"Command to run as a persistent background job"`
	Root        bool   `json:"root,omitempty" jsonschema:"Run as root instead of aiworker"`
	Approval    bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when a previous call returned approval_required"`
	OperationID string `json:"operation_id,omitempty" jsonschema:"Optional caller-generated idempotency key; retry the same material request with the same key after a lost response"`
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
	Script            string `json:"script" jsonschema:"JavaScript statements using the pre-created Playwright browser, context, and page variables; maximum 131072 raw bytes"`
	TimeoutMS         int64  `json:"timeout_ms,omitempty" jsonschema:"Browser execution timeout in milliseconds; default 90000, maximum 300000"`
	IgnoreHTTPSErrors bool   `json:"ignore_https_errors,omitempty" jsonschema:"Explicit scoped exception for local/self-signed development; default false preserves normal HTTPS certificate validation"`
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
		&mcpsdk.Implementation{Name: "ai-server-agent", Version: version},
		&mcpsdk.ServerOptions{
			Instructions: instructions(cfg.WorkspaceDir),
			Capabilities: &mcpsdk.ServerCapabilities{},
		},
	)
	s.mcp.AddReceivingMiddleware(requestCorrelationMiddleware())
	s.registerTools()
	return s, nil
}

func instructions(workspaceDir string) string {
	return fmt.Sprintf("Dedicated AI-operated test-server control plane. Before host-wide package, firewall, network, service, disk, user, web-stack, or control-panel changes, call agent_environment and preserve all critical components it reports. The workspace at %s is persistent: use repository_discover/repository_inspect for repository identity, prefer git worktree for task isolation and use worktree_create/worktree_remove when structured lifecycle safety adds value, keep ordinary Git verbs in run_command/PTY, reuse existing repositories, worktrees, and task environments before creating duplicates, and never delete dirty, untracked, ambiguous, or unknown workspace state. The control plane intentionally does not own ports 80/443 and does not require nginx, Apache, PHP, MySQL, Docker, Node.js, Python, or aaPanel. Use run_command for ordinary bounded work and run_root_command only when host-level privileges are required. For unknown/large source files, use workspace_stat to inspect size/version without content, then workspace_read with a small explicit limit and matching file_version; metadata is not file content. For code inspection, use workspace_search mode=text for bounded literal/regex text occurrences when trusted ripgrep and Landlock ABI v2 are available, workspace_search mode=structural for syntax-tree patterns, and LSP for semantic symbol/type/reference meaning. Use rg/git grep through run_command as the advanced/unstructured CLI fallback; run_command retains normal aiworker authority rather than the structured helper Landlock boundary. Use start_job from the beginning for installs, large builds/test suites, migrations, crawls, or other work expected to run long or produce substantial output, then continue with job_status/job_output. If a tool returns approval_required, explain the exact risk to the user and retry with approval=true only after explicit confirmation. Persistent jobs survive MCP/ChatGPT disconnects. Optional interactive terminal workflows may install and use tmux through root shell without making tmux a core dependency.", workspaceDir)
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
	const maxTextFallbackBytes = 32 << 10
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
		Description: "Run an arbitrary Bash command as the unprivileged aiworker user in the dedicated workspace. Use for normal project work, builds, tests, Git, package managers inside the project, and diagnostics that do not require host privileges. Synchronous execution is bounded to five minutes; use start_job for work expected to run longer or produce high output.",
		Annotations: annotations(false, false, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RunInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: input.Command, TimeoutMS: int64(synchronousCommandTimeout / time.Millisecond)})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_root_command",
		Description: "Run an arbitrary Bash command as root. Use for apt packages, services, Docker, aaPanel, networking, system configuration, deployment setup, and tests that genuinely need root. Synchronous execution is bounded to five minutes; use a root persistent job for work expected to run longer or produce high output. Connection-risk and destructive commands return approval_required until the user explicitly confirms and approval=true is supplied.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RootRunInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: input.Command, Root: true, Approval: input.Approval, TimeoutMS: int64(synchronousCommandTimeout / time.Millisecond)})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "start_job",
		Description: "Start a persistent background Bash command using a transient systemd unit. Use for installs, large builds/test suites, migrations, crawls, and other work expected to run long or produce substantial output. The job and bounded retained output continue if ChatGPT disconnects or the MCP service restarts. Supply operation_id when a lost response may be retried so the same material request returns the same job handle instead of starting a duplicate.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input StartJobInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "start_job", Command: input.Command, Root: input.Root, Approval: input.Approval, OperationID: input.OperationID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_status", Description: "Read and reconcile the current state and exit status of a persistent job. Reconciliation may persist an unknown-completion marker and retire a stale protected command handoff after the transient unit is gone.", Annotations: annotations(false, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_status", JobID: input.JobID})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_output", Description: "Read a chunk of persistent job stdout/stderr without requiring the original MCP connection to remain open.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobOutputInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_output", JobID: input.JobID, Offset: input.Offset, Limit: input.Limit})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_stop", Description: "Stop a persistent background job.", Annotations: annotations(false, true, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_stop", JobID: input.JobID})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "read_file", Description: "Read a bounded raw-byte range from a regular host file through the root executor. This is broad root-readable host-file access, not a low-privilege sandbox. Binary data is returned with explicit base64 encoding; Agent-protected aliases still require approval.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input ReadFileInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "read_file", Path: input.Path, Offset: input.Offset, Limit: input.Limit, FileVersion: input.FileVersion, Root: true, Approval: input.Approval})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "write_file", Description: "Atomically replace a bounded regular host file through the root executor. This is root-capable host mutation, not a safer substitute for run_root_command. The parent must already exist; optional file_version/must_not_exist preconditions prevent accidental lost updates; Agent-protected aliases require approval.", Annotations: annotations(false, true, false, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input WriteFileInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "write_file", Path: input.Path, Content: input.Content, Root: true, Mode: input.Mode, FileVersion: input.FileVersion, MustNotExist: input.MustNotExist, Approval: input.Approval})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_status", Description: "Inspect bounded non-secret browser runtime readiness and pinned Node/Playwright/Chromium versions. The persistent browser profile is Agent-wide shared state, not per-client or per-user isolation.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input EmptyInput) (*mcpsdk.CallToolResult, browser.RuntimeStatus, error) {
			status := s.browser.Status(ctx)
			return textResult(fmt.Sprintf("installed=%t ready=%t busy=%t shared_profile=%t reason=%q", status.Installed, status.Ready, status.Busy, status.SharedProfile, status.Reason), false), status, nil
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_setup", Description: "Converge the optional root-owned Node.js + Playwright + Chromium runtime under /opt/ai-server-agent/browser to the Agent-pinned manifest. Setup is bounded and fail-fast one-at-a-time, preserves the durable Agent-wide browser profile, and may install required Chromium OS libraries. It does not replace system Node or take over ports 80/443.", Annotations: annotations(false, true, true, true)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserSetupInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := s.browser.Setup(ctx, input.Approval)
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_run", Description: "Run bounded Playwright JavaScript in headless Chromium using the Agent-wide shared persistent browser profile. Variables browser, context, and page are pre-created; use console.log for observations. HTTPS certificate validation is enabled by default; ignore_https_errors is an explicit request-scoped development exception. Browser execution is action-capable and may reuse cookies/local-storage/session state created by other authorized browser callers.", Annotations: annotations(false, true, false, true)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserRunInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := s.browser.Run(ctx, browser.RunOptions{Script: input.Script, TimeoutMS: input.TimeoutMS, IgnoreHTTPSErrors: input.IgnoreHTTPSErrors})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})

	s.registerRepositoryEnvironmentTool()
	s.registerWorkspaceFileTools()
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
	httpServer := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	fmt.Printf("ai-server-agent listening on %s%s\n", cfg.ListenAddress, cfg.MCPPath)
	return httpServer.ListenAndServe()
}
