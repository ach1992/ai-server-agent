package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func testConfig(t *testing.T, authMode string) config.Config {
	t.Helper()
	d := t.TempDir()
	et := filepath.Join(d, "exec")
	bt := filepath.Join(d, "mcp")
	if err := os.WriteFile(et, []byte("exec-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bt, []byte("mcp-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.ExecutorToken = et
	c.BearerTokenFile = bt
	c.StateDir = d
	c.LogDir = d
	c.WorkspaceDir = d
	c.AuthMode = authMode
	return c
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func TestOfficialSDKCanDiscoverTools(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx := context.Background()
	httpClient := ts.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "ai-server-agent-test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: ts.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) < 10 {
		t.Fatalf("got %d tools, want at least 10", len(res.Tools))
	}
	foundEnvironment := false
	foundRoot := false
	foundBrowser := false
	foundBrowserStatus := false
	foundStartJob := false
	foundJobStatus := false
	foundReadFile := false
	foundWriteFile := false
	foundRepositoryEnvironment := false
	foundRepositoryDiscover := false
	foundRepositoryInspect := false
	foundWorktreeCreate := false
	foundWorktreeRemove := false
	foundWorkspaceSearch := false
	for _, tool := range res.Tools {
		switch tool.Name {
		case "agent_environment":
			foundEnvironment = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatal("agent_environment must advertise readOnlyHint")
			}
		case "run_root_command":
			foundRoot = true
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatal("run_root_command must advertise destructiveHint")
			}
			if tool.OutputSchema == nil {
				t.Fatal("run_root_command must advertise a typed output schema")
			}
			b, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			schema := string(b)
			for _, field := range []string{"error_code", "error_class", "output", "output_encoding", "bytes_seen", "bytes_returned", "truncated", "omitted_bytes", "duration_ms", "timed_out", "exit_code"} {
				if !strings.Contains(schema, `"`+field+`"`) {
					t.Fatalf("run_root_command output schema missing %q: %s", field, schema)
				}
			}
		case "start_job":
			foundStartJob = true
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `"operation_id"`) {
				t.Fatalf("start_job input schema missing operation_id: %s", b)
			}
		case "job_status":
			foundJobStatus = true
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
				t.Fatal("job_status must not advertise readOnlyHint because interrupted-state reconciliation may persist bounded local recovery state")
			}
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("job_status reconciliation is non-destructive")
			}
			if !tool.Annotations.IdempotentHint {
				t.Fatal("job_status reconciliation must remain idempotent")
			}
		case "read_file":
			foundReadFile = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatal("read_file must advertise readOnlyHint")
			}
			if !strings.Contains(tool.Description, "root-readable") {
				t.Fatalf("read_file description must state root-readable authority: %q", tool.Description)
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"offset", "limit", "file_version"} {
				if !strings.Contains(string(in), `"`+field+`"`) {
					t.Fatalf("read_file input schema missing %q: %s", field, in)
				}
			}
			out, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"file_size", "file_version", "requested_offset", "next_offset", "eof", "output_encoding", "bytes_returned"} {
				if !strings.Contains(string(out), `"`+field+`"`) {
					t.Fatalf("read_file output schema missing %q: %s", field, out)
				}
			}
		case "write_file":
			foundWriteFile = true
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatal("write_file must advertise destructiveHint")
			}
			if !strings.Contains(tool.Description, "root-capable") {
				t.Fatalf("write_file description must state root-capable authority: %q", tool.Description)
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"file_version", "must_not_exist"} {
				if !strings.Contains(string(in), `"`+field+`"`) {
					t.Fatalf("write_file input schema missing %q: %s", field, in)
				}
			}
		case "workspace_search":
			foundWorkspaceSearch = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("workspace_search must advertise read-only/idempotent hints")
			}
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("workspace_search must remain non-destructive")
			}
			if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
				t.Fatal("workspace_search must remain local-only")
			}
			if !strings.Contains(tool.Description, "never applies rewrites") || !strings.Contains(tool.Description, "rg/git grep") || !strings.Contains(tool.Description, "LSP") {
				t.Fatalf("workspace_search description must preserve inspection responsibility ladder: %q", tool.Description)
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"mode", "workspace", "language", "pattern", "paths", "globs", "limit", "timeout_ms"} {
				if !strings.Contains(string(in), `"`+field+`"`) {
					t.Fatalf("workspace_search input schema missing %q: %s", field, in)
				}
			}
			if strings.Contains(string(in), `"rewrite"`) || strings.Contains(string(in), `"update_all"`) {
				t.Fatalf("workspace_search must not expose automatic rewrite/apply inputs: %s", in)
			}
			out, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"engine_version", "matches", "complete", "truncated", "truncation_reason", "timed_out", "duration_ms", "file", "range", "captures"} {
				if !strings.Contains(string(out), `"`+field+`"`) {
					t.Fatalf("workspace_search output schema missing %q: %s", field, out)
				}
			}
		case "repository_discover":
			foundRepositoryDiscover = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("repository_discover must advertise read-only/idempotent hints")
			}
			if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
				t.Fatal("repository_discover must remain local-only")
			}
			in, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(in), "\"remote_identity\"") {
				t.Fatalf("repository_discover input schema missing remote_identity: %s", in)
			}
		case "repository_inspect":
			foundRepositoryInspect = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("repository_inspect must advertise read-only/idempotent hints")
			}
			if tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Fatal("repository_inspect must disclose optional remote verification")
			}
			in, _ := json.Marshal(tool.InputSchema)
			for _, field := range []string{"path", "verify_remote", "remote", "remote_branch"} {
				if !strings.Contains(string(in), "\""+field+"\"") {
					t.Fatalf("repository_inspect input schema missing %q: %s", field, in)
				}
			}
		case "worktree_create":
			foundWorktreeCreate = true
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("worktree_create must advertise mutating/idempotent semantics")
			}
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("worktree_create is reversible and must not advertise destructiveHint")
			}
			in, _ := json.Marshal(tool.InputSchema)
			for _, field := range []string{"repository_path", "worktree_path", "branch", "start_ref", "expected_start_sha"} {
				if !strings.Contains(string(in), "\""+field+"\"") {
					t.Fatalf("worktree_create input schema missing %q: %s", field, in)
				}
			}
		case "worktree_remove":
			foundWorktreeRemove = true
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("worktree_remove must advertise mutating/idempotent semantics")
			}
			if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Fatal("worktree_remove must disclose destructive/remote-verification semantics")
			}
			in, _ := json.Marshal(tool.InputSchema)
			for _, field := range []string{"repository_path", "worktree_path", "expected_head", "remote", "remote_branch", "disposable"} {
				if !strings.Contains(string(in), "\""+field+"\"") {
					t.Fatalf("worktree_remove input schema missing %q: %s", field, in)
				}
			}
		case "repository_environment":
			foundRepositoryEnvironment = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("repository_environment must advertise read-only/idempotent hints")
			}
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("repository_environment must remain non-destructive")
			}
			if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
				t.Fatal("repository_environment discovery must remain local-only")
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(in), `"path"`) {
				t.Fatalf("repository_environment input schema missing explicit path: %s", in)
			}
			out, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"repository_root", "repository_head", "declarations", "languages", "mechanisms", "tools", "entrypoints", "host_status", "selection_required", "isolation_requirement"} {
				if !strings.Contains(string(out), `"`+field+`"`) {
					t.Fatalf("repository_environment output schema missing %q: %s", field, out)
				}
			}
		case "browser_status":
			foundBrowserStatus = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("browser_status must advertise read-only/idempotent hints")
			}
			if !strings.Contains(tool.Description, "shared") {
				t.Fatalf("browser_status must disclose shared-profile semantics: %q", tool.Description)
			}
			out, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"inspection_complete", "installed", "ready", "busy", "shared_profile", "desired", "installed_state"} {
				if !strings.Contains(string(out), `"`+field+`"`) {
					t.Fatalf("browser_status output schema missing %q: %s", field, out)
				}
			}
		case "browser_run":
			foundBrowser = true
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Fatal("browser_run must advertise destructive/open-world hints")
			}
			if !strings.Contains(tool.Description, "HTTPS certificate validation is enabled by default") || !strings.Contains(tool.Description, "shared persistent browser profile") {
				t.Fatalf("browser_run must disclose TLS/shared-profile semantics: %q", tool.Description)
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"script", "timeout_ms", "ignore_https_errors"} {
				if !strings.Contains(string(in), `"`+field+`"`) {
					t.Fatalf("browser_run input schema missing %q: %s", field, in)
				}
			}
		}
	}
	if !foundEnvironment || !foundRoot || !foundStartJob || !foundJobStatus || !foundReadFile || !foundWriteFile || !foundWorkspaceSearch || !foundRepositoryEnvironment || !foundRepositoryDiscover || !foundRepositoryInspect || !foundWorktreeCreate || !foundWorktreeRemove || !foundBrowserStatus || !foundBrowser {
		t.Fatalf("required tools missing: environment=%v root=%v start_job=%v job_status=%v read_file=%v write_file=%v workspace_search=%v repository_environment=%v repository_discover=%v repository_inspect=%v worktree_create=%v worktree_remove=%v browser_status=%v browser=%v", foundEnvironment, foundRoot, foundStartJob, foundJobStatus, foundReadFile, foundWriteFile, foundWorkspaceSearch, foundRepositoryEnvironment, foundRepositoryDiscover, foundRepositoryInspect, foundWorktreeCreate, foundWorktreeRemove, foundBrowserStatus, foundBrowser)
	}
}

func TestInstructionsDescribePersistentWorkspace(t *testing.T) {
	got := instructions("/srv/ai-workspace")
	for _, want := range []string{
		"/srv/ai-workspace is persistent",
		"task environments",
		"prefer git worktree",
		"never delete dirty, untracked, ambiguous, or unknown workspace state",
		"rg/git grep through run_command for text occurrences",
		"workspace_search for structural syntax-tree patterns",
		"do not treat structural matches as semantic symbol/type/reference resolution",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q", want)
		}
	}
}

func TestNewRejectsUnsupportedAuthMode(t *testing.T) {
	cfg := testConfig(t, "none")
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "bearer authentication is required") {
		t.Fatalf("New() error = %v, want unsupported auth mode rejection", err)
	}
}

func TestNewRejectsEmptyBearerToken(t *testing.T) {
	cfg := testConfig(t, "bearer")
	if err := os.WriteFile(cfg.BearerTokenFile, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "MCP bearer token is empty") {
		t.Fatalf("New() error = %v, want empty bearer-token rejection", err)
	}
}

func TestNewRejectsEmptyExecutorToken(t *testing.T) {
	cfg := testConfig(t, "bearer")
	if err := os.WriteFile(cfg.ExecutorToken, []byte("\n\t"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "executor token is empty") {
		t.Fatalf("New() error = %v, want empty executor-token rejection", err)
	}
}

func TestBearerAuthRejectsMissingToken(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, cfg.MCPPath, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuthRejectsInvalidToken(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, cfg.MCPPath, nil)
	r.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuthAcceptsValidToken(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/agent-environment.json", nil)
	r.Header.Set("Authorization", "Bearer mcp-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHealthDoesNotRequireMCPAuth(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, cfg.HealthPath, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", w.Code, http.StatusOK)
	}
}
