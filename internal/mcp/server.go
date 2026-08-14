package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ach1992/ai-server-agent/internal/browser"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/manifest"
)

const version = "0.1.0-dev"

type Server struct {
	cfg           config.Config
	executorToken string
	bearerToken   string
	browser       *browser.Manager
}

func New(cfg config.Config) (*Server, error) {
	et, err := os.ReadFile(cfg.ExecutorToken)
	if err != nil {
		return nil, err
	}
	bt := ""
	if cfg.AuthMode == "bearer" {
		b, er := os.ReadFile(cfg.BearerTokenFile)
		if er != nil {
			return nil, er
		}
		bt = strings.TrimSpace(string(b))
	}
	return &Server{cfg: cfg, executorToken: strings.TrimSpace(string(et)), bearerToken: bt, browser: browser.New(cfg, strings.TrimSpace(string(et)))}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.HealthPath, s.health)
	mux.HandleFunc(s.cfg.MCPPath, s.auth(s.handleMCP))
	mux.HandleFunc("/agent-environment.json", s.auth(s.environmentHTTP))
	return mux
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"status":"ok","service":"ai-server-agent"}`)
}
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthMode == "none" {
			next(w, r)
			return
		}
		h := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if len(h) != len(s.bearerToken) || subtle.ConstantTimeCompare([]byte(h), []byte(s.bearerToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-server-agent"`)
			http.Error(w, "unauthorized", 401)
			return
		}
		next(w, r)
	}
}
func (s *Server) environmentHTTP(w http.ResponseWriter, r *http.Request) {
	m := manifest.Build(s.cfg)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, nil, -32700, "parse error", err.Error())
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.writeErr(w, req.ID, -32600, "invalid request", nil)
		return
	}
	switch req.Method {
	case "initialize":
		s.initialize(w, req)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "server/discover":
		s.discover(w, req)
	case "ping":
		s.writeResult(w, req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(w, req.ID, s.listResult(r))
	case "tools/call":
		s.call(w, r, req)
	default:
		s.writeErr(w, req.ID, -32601, "method not found", req.Method)
	}
}

func (s *Server) initialize(w http.ResponseWriter, req rpcRequest) {
	var p map[string]any
	_ = json.Unmarshal(req.Params, &p)
	pv := "2025-11-25"
	if x, ok := p["protocolVersion"].(string); ok && x != "" {
		pv = x
	}
	s.writeResult(w, req.ID, map[string]any{"protocolVersion": pv, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "ai-server-agent", "version": version}, "instructions": s.instructions()})
}
func (s *Server) discover(w http.ResponseWriter, req rpcRequest) {
	s.writeResult(w, req.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": s.instructions(), "ttlMs": 300000, "cacheScope": "private", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "ai-server-agent", "version": version}}})
}
func (s *Server) instructions() string {
	return "Dedicated AI test-server control plane. Before host-wide changes call agent_environment. Preserve the agent services, /etc/ai-server-agent, /var/lib/ai-server-agent, executor socket, and configured MCP endpoint. The agent does not own ports 80/443 and intentionally has no nginx/Apache/PHP/MySQL/Docker/Node/Python dependency. Use run_command for normal work and run_root_command only when host-level privileges are required. Commands that threaten connectivity or protected agent resources require explicit approval=true after user confirmation."
}

func schema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func (s *Server) tools() []Tool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	return []Tool{
		{Name: "agent_environment", Description: "Read the control-plane manifest and self-preservation rules. Call this before host-wide package, service, firewall, network, disk, user, web-stack, or control-panel changes.", InputSchema: schema(map[string]any{}), Annotations: map[string]any{"readOnlyHint": true, "idempotentHint": true}},
		{Name: "run_command", Description: "Run an arbitrary shell command as the unprivileged aiworker user in the dedicated workspace.", InputSchema: schema(map[string]any{"command": str("Bash command to execute")}, "command"), Annotations: map[string]any{"destructiveHint": false, "openWorldHint": true}},
		{Name: "run_root_command", Description: "Run an arbitrary shell command as root on the host. Use for packages, services, Docker, aaPanel, networking, system configuration, and tests that genuinely need root. If the command risks connectivity, destroys data, or touches agent-protected resources, first ask the user and then retry with approval=true.", InputSchema: schema(map[string]any{"command": str("Bash command to execute as root"), "approval": boolean("Set true only after explicit user approval when the server returns approval_required")}, "command"), Annotations: map[string]any{"destructiveHint": true, "openWorldHint": true}},
		{Name: "start_job", Description: "Start a persistent background command as a systemd transient job. Jobs continue if ChatGPT disconnects or the MCP service restarts.", InputSchema: schema(map[string]any{"command": str("Command to run"), "root": boolean("Run job as root instead of aiworker"), "approval": boolean("Set true only after explicit user approval when required")}, "command"), Annotations: map[string]any{"destructiveHint": true, "openWorldHint": true}},
		{Name: "job_status", Description: "Read persistent job state.", InputSchema: schema(map[string]any{"job_id": str("Job id")}, "job_id"), Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "job_output", Description: "Read a chunk of persistent job output.", InputSchema: schema(map[string]any{"job_id": str("Job id"), "offset": integer("Byte offset, default 0"), "limit": integer("Maximum bytes, default and max 1048576")}, "job_id"), Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "job_stop", Description: "Stop a persistent job.", InputSchema: schema(map[string]any{"job_id": str("Job id")}, "job_id"), Annotations: map[string]any{"destructiveHint": true}},
		{Name: "read_file", Description: "Read a file from the host through the privileged executor. This can read system configuration and logs; avoid reading secrets unless required.", InputSchema: schema(map[string]any{"path": str("Absolute path"), "approval": boolean("Set true only after explicit user approval when reading agent-protected resources")}, "path"), Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "write_file", Description: "Write a complete file through the privileged executor. Protected agent resources require approval.", InputSchema: schema(map[string]any{"path": str("Absolute path"), "content": str("Complete file content"), "mode": integer("Unix mode as decimal, e.g. 420 for 0644"), "approval": boolean("Set true after user approval when required")}, "path", "content"), Annotations: map[string]any{"destructiveHint": true}},
		{Name: "browser_setup", Description: "Install the optional browser runtime into the agent-owned state directory. Uses a private Node.js and Playwright runtime and does not replace system web servers or system Node.", InputSchema: schema(map[string]any{"approval": boolean("Set true to allow downloading and installing the optional browser runtime")}), Annotations: map[string]any{"destructiveHint": true, "openWorldHint": true}},
		{Name: "browser_run", Description: "Run a Playwright JavaScript body in an isolated headless Chromium session. Variables browser, context, and page are pre-created. Use console.log(JSON.stringify(...)) for structured observations.", InputSchema: schema(map[string]any{"script": str("JavaScript statements using Playwright page/context/browser")}, "script"), Annotations: map[string]any{"openWorldHint": true}},
	}
}
func (s *Server) listResult(r *http.Request) map[string]any {
	res := map[string]any{"tools": s.tools()}
	if r.Header.Get("Mcp-Protocol-Version") == "2026-07-28" {
		res["resultType"] = "complete"
		res["ttlMs"] = 300000
		res["cacheScope"] = "private"
	}
	return res
}

func argString(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}
func argBool(a map[string]any, k string) bool { v, _ := a[k].(bool); return v }
func argInt(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
func (s *Server) call(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeErr(w, req.ID, -32602, "invalid params", err.Error())
		return
	}
	a := p.Arguments
	var resp executor.Response
	var err error
	switch p.Name {
	case "agent_environment":
		b, _ := json.MarshalIndent(manifest.Build(s.cfg), "", "  ")
		s.toolResult(w, r, req.ID, string(b), false)
		return
	case "run_command":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: argString(a, "command"), Root: false})
	case "run_root_command":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "run", Command: argString(a, "command"), Root: true, Approval: argBool(a, "approval")})
	case "start_job":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "start_job", Command: argString(a, "command"), Root: argBool(a, "root"), Approval: argBool(a, "approval")})
	case "job_status":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_status", JobID: argString(a, "job_id")})
	case "job_output":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_output", JobID: argString(a, "job_id"), Offset: int64(argInt(a, "offset")), Limit: argInt(a, "limit")})
	case "job_stop":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "job_stop", JobID: argString(a, "job_id")})
	case "read_file":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "read_file", Path: argString(a, "path"), Root: true, Approval: argBool(a, "approval")})
	case "write_file":
		resp, err = executor.ClientCall(s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "write_file", Path: argString(a, "path"), Content: argString(a, "content"), Root: true, Mode: uint32(argInt(a, "mode")), Approval: argBool(a, "approval")})
	case "browser_setup":
		resp, err = s.browser.Setup(argBool(a, "approval"))
	case "browser_run":
		resp, err = s.browser.Run(argString(a, "script"))
	default:
		s.writeErr(w, req.ID, -32602, "unknown tool", p.Name)
		return
	}
	if err != nil {
		s.toolResult(w, r, req.ID, err.Error(), true)
		return
	}
	b, _ := json.MarshalIndent(resp, "", "  ")
	s.toolResult(w, r, req.ID, string(b), !resp.OK)
}
func (s *Server) toolResult(w http.ResponseWriter, r *http.Request, id json.RawMessage, text string, isErr bool) {
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	if r.Header.Get("Mcp-Protocol-Version") == "2026-07-28" {
		res["resultType"] = "complete"
		res["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "ai-server-agent", "version": version}}
	}
	s.writeResult(w, id, res)
}
func (s *Server) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}
func (s *Server) writeErr(w http.ResponseWriter, id json.RawMessage, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}})
}

func Serve(cfg config.Config) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(cfg.StateDir, 0750)
	_ = manifest.Write(filepath.Join(cfg.StateDir, "AI_ENVIRONMENT.json"), manifest.Build(cfg))
	httpServer := &http.Server{Addr: cfg.ListenAddress, Handler: s.Handler(), ReadHeaderTimeout: 10_000_000_000, MaxHeaderBytes: 1 << 20}
	fmt.Printf("ai-server-agent listening on %s%s\n", cfg.ListenAddress, cfg.MCPPath)
	return httpServer.ListenAndServe()
}

var _ = strconv.Itoa
