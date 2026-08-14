package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/browser"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/manifest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0-dev"

type Server struct {
	cfg           config.Config
	executorToken string
	bearerToken   string
	browser       *browser.Manager
	mcp           *mcpsdk.Server
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
	Command  string `json:"command" jsonschema:"Command to run as a persistent background job"`
	Root     bool   `json:"root,omitempty" jsonschema:"Run as root instead of aiworker"`
	Approval bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when a previous call returned approval_required"`
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
	Path     string `json:"path" jsonschema:"Absolute host path"`
	Approval bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when reading an agent-protected resource"`
}
type WriteFileInput struct {
	Path     string `json:"path" jsonschema:"Absolute host path"`
	Content  string `json:"content" jsonschema:"Complete replacement file content"`
	Mode     uint32 `json:"mode,omitempty" jsonschema:"Unix file mode as decimal; 420 equals 0644"`
	Approval bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit user approval when writing an agent-protected resource"`
}
type BrowserSetupInput struct {
	Approval bool `json:"approval,omitempty" jsonschema:"Set true to allow downloading the private browser runtime and installing required shared libraries"`
}
type BrowserRunInput struct {
	Script string `json:"script" jsonschema:"JavaScript statements using the pre-created Playwright browser, context, and page variables"`
}

func New(cfg config.Config) (*Server, error) {
	et, err := os.ReadFile(cfg.ExecutorToken)
	if err != nil {
		return nil, fmt.Errorf("read executor token: %w", err)
	}
	bt := ""
	if cfg.AuthMode == "bearer" {
		b, err := os.ReadFile(cfg.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("read MCP bearer token: %w", err)
		}
		bt = strings.TrimSpace(string(b))
	}

	s := &Server{
		cfg:           cfg,
		executorToken: strings.TrimSpace(string(et)),
		bearerToken:   bt,
		browser:       browser.New(cfg, strings.TrimSpace(string(et))),
	}
	s.mcp = mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "ai-server-agent", Version: version},
		&mcpsdk.ServerOptions{
			Instructions: instructions(),
			Capabilities: &mcpsdk.ServerCapabilities{},
		},
	)
	s.registerTools()
	return s, nil
}

func instructions() string {
	return "Dedicated AI-operated test-server control plane. Before host-wide package, firewall, network, service, disk, user, web-stack, or control-panel changes, call agent_environment and preserve all critical components it reports. The control plane intentionally does not own ports 80/443 and does not require nginx, Apache, PHP, MySQL, Docker, Node.js, Python, or aaPanel. Use run_command for ordinary work and run_root_command only when host-level privileges are required. If a tool returns approval_required, explain the exact risk to the user and retry with approval=true only after explicit confirmation. Use start_job for long-running work so it survives MCP/ChatGPT disconnects. Optional interactive terminal workflows may install and use tmux through root shell without making tmux a core dependency."
}

func annotations(readOnly, destructive, idempotent, openWorld bool) *mcpsdk.ToolAnnotations {
	return &mcpsdk.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		DestructiveHint: destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   openWorld,
	}
}

func textResult(text string, isError bool) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		IsError: isError,
	}
}

func responseResult(resp executor.Response) (*mcpsdk.CallToolResult, any, error) {
	b, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(b), !resp.OK), nil, nil
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
		Description: "Run an arbitrary Bash command as the unprivileged aiworker user in the dedicated workspace. Use for normal project work, builds, tests, Git, package managers inside the project, and diagnostics that do not require host privileges.",
		Annotations: annotations(false, false, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RunInput) (*mcpsdk.CallToolResult, any, error) {
		resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: input.Command})
		if err != nil {
			return textResult(err.Error(), true), nil, nil
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_root_command",
		Description: "Run an arbitrary Bash command as root. Use for apt packages, services, Docker, aaPanel, networking, system configuration, deployment setup, and tests that genuinely need root. Connection-risk and destructive commands return approval_required until the user explicitly confirms and approval=true is supplied.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RootRunInput) (*mcpsdk.CallToolResult, any, error) {
		resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: input.Command, Root: true, Approval: input.Approval})
		if err != nil {
			return textResult(err.Error(), true), nil, nil
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "start_job",
		Description: "Start a persistent background Bash command using a transient systemd unit. The job and its output continue if ChatGPT disconnects or the MCP service restarts.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input StartJobInput) (*mcpsdk.CallToolResult, any, error) {
		resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "start_job", Command: input.Command, Root: input.Root, Approval: input.Approval})
		if err != nil {
			return textResult(err.Error(), true), nil, nil
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_status", Description: "Read the current state and exit status of a persistent job.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_status", JobID: input.JobID})
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_output", Description: "Read a chunk of persistent job stdout/stderr without requiring the original MCP connection to remain open.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobOutputInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_output", JobID: input.JobID, Offset: input.Offset, Limit: input.Limit})
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "job_stop", Description: "Stop a persistent background job.", Annotations: annotations(false, true, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input JobInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_stop", JobID: input.JobID})
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "read_file", Description: "Read a host file through the privileged executor. Agent credentials/config/state are protected and require explicit approval.", Annotations: annotations(true, false, true, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input ReadFileInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "read_file", Path: input.Path, Root: true, Approval: input.Approval})
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "write_file", Description: "Write a complete host file through the privileged executor. Writes to protected agent resources require explicit approval.", Annotations: annotations(false, true, false, false)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input WriteFileInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "write_file", Path: input.Path, Content: input.Content, Root: true, Mode: input.Mode, Approval: input.Approval})
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_setup", Description: "Install an optional private Node.js + Playwright + Chromium runtime below the agent state directory. It does not replace system Node or take over ports 80/443.", Annotations: annotations(false, true, true, true)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserSetupInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := s.browser.Setup(input.Approval)
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_run", Description: "Run Playwright JavaScript in headless Chromium using a persistent browser profile. Variables browser, context, and page are pre-created; use console.log for observations.", Annotations: annotations(false, false, false, true)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, input BrowserRunInput) (*mcpsdk.CallToolResult, any, error) {
			resp, err := s.browser.Run(input.Script)
			if err != nil {
				return textResult(err.Error(), true), nil, nil
			}
			return responseResult(resp)
		})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthMode == "none" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-server-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(h, prefix))
		want := s.bearerToken
		if len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-server-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
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
