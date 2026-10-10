package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
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

func TestMCPImplementationVersionBuildIdentity(t *testing.T) {
	revision := strings.Repeat("a", 40)
	cases := []struct {
		name    string
		marker  string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{name: "release without provenance", marker: "0.1.10", want: "0.1.10"},
		{name: "stamped release", marker: "0.1.10", stamped: revision, want: "0.1.10+g" + revision},
		{name: "stamped dirty release", marker: "0.1.10", stamped: revision + "-dirty", want: "0.1.10+g" + revision + ".dirty"},
		{name: "unknown source", marker: "0.1.0-dev", want: "source-unknown"},
		{name: "stamped source", marker: "0.1.0-dev", stamped: revision, want: "source-" + revision},
		{name: "stamped dirty source", marker: "0.1.0-dev", stamped: revision + "-dirty", want: "source-" + revision + "-dirty"},
		{name: "bad stamped source", marker: "0.1.0-dev", stamped: "invalid", want: "source-unknown"},
		{name: "clean source", marker: "0.1.0-dev", info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "false"}}}, want: "source-" + revision},
		{name: "dirty source", marker: "0.1.0-dev", info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "true"}}}, want: "source-" + revision + "-dirty"},
		{name: "missing dirty status", marker: "0.1.0-dev", info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}}}, want: "source-unknown"},
		{name: "invalid revision", marker: "0.1.0-dev", info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "untrusted"}, {Key: "vcs.modified", Value: "false"}}}, want: "source-unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionForBuild(tc.marker, tc.stamped, tc.info); got != tc.want {
				t.Fatalf("versionForBuild() = %q, want %q", got, tc.want)
			}
		})
	}
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
	if got := session.InitializeResult().ServerInfo.Version; got != mcpImplementationVersion() || (strings.HasSuffix(version, "-dev") && got == version) {
		t.Fatalf("MCP initialize serverInfo.Version = %q, want truthful build identity, not stale development marker %q", got, version)
	}

	res, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) < 10 {
		t.Fatalf("got %d tools, want at least 10", len(res.Tools))
	}
	foundEnvironment := false
	foundRunCommand := false
	foundRoot := false
	foundBrowser := false
	foundBrowserE2E := false
	foundBrowserSessions := map[string]bool{}
	foundBrowserStatus := false
	foundStartJob := false
	foundJobStatus := false
	foundReadFile := false
	foundWriteFile := false
	foundWorkerStat := false
	foundWorkerRead := false
	foundWorkerWrite := false
	foundWorkerApplyEdits := false
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
		case "run_command":
			foundRunCommand = true
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatal("run_command must advertise potentially destructive arbitrary Bash effects")
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
		case "workspace_stat":
			foundWorkerStat = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint ||
				tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("workspace_stat must advertise read-only worker metadata")
			}
			if !strings.Contains(tool.Description, "aiworker") || !strings.Contains(tool.Description, "without reading") {
				t.Fatal("workspace_stat must disclose worker authority and no-content semantics")
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"workspace", "path", "file_version"} {
				if !strings.Contains(string(in), `"`+field+`"`) {
					t.Fatalf("workspace_stat input missing %q: %s", field, in)
				}
			}
			if strings.Contains(string(in), `"limit"`) || strings.Contains(string(in), `"offset"`) ||
				strings.Contains(string(in), `"content"`) {
				t.Fatal("workspace_stat must not expose a content read/write parameter")
			}
		case "workspace_read":
			foundWorkerRead = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("workspace_read must be read-only and non-destructive")
			}
			if !strings.Contains(tool.Description, "aiworker") || !strings.Contains(tool.Description, "not root") {
				t.Fatal("workspace_read description must disclose worker authority")
			}
		case "workspace_apply_edits":
			foundWorkerApplyEdits = true
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatal("workspace_apply_edits must disclose mutation")
			}
			if !strings.Contains(tool.Description, "NOT an all-or-nothing transaction") {
				t.Fatal("multi-file tool must disclose partial outcomes")
			}
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"workspace", "edits", "file_version", "must_not_exist", "replacements"} {
				if !strings.Contains(string(b), `"`+field+`"`) {
					t.Fatalf("workspace_apply_edits schema missing %s: %s", field, b)
				}
			}
		case "workspace_write":
			foundWorkerWrite = true
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatal("workspace_write must disclose mutation")
			}
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"workspace", "path", "content", "file_version", "must_not_exist"} {
				if !strings.Contains(string(b), `"`+field+`"`) {
					t.Fatalf("workspace_write missing %s", field)
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
			if !strings.Contains(tool.Description, "never applies rewrites") || !strings.Contains(tool.Description, "rg/git grep") || !strings.Contains(tool.Description, "LSP") || !strings.Contains(tool.Description, "mode=text") || !strings.Contains(tool.Description, "mode=structural") {
				t.Fatalf("workspace_search description must preserve inspection responsibility ladder: %q", tool.Description)
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"mode", "workspace", "language", "pattern", "literal", "paths", "globs", "limit", "timeout_ms"} {
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
		case "browser_session_open", "browser_session_flow", "browser_session_capture", "browser_session_trace", "browser_session_status", "browser_session_close":
			foundBrowserSessions[tool.Name] = true
			if tool.Annotations == nil {
				t.Fatalf("%s lacks annotations", tool.Name)
			}
			if tool.Name == "browser_session_status" {
				if !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
					t.Fatal("Browser session status must be read-only/idempotent")
				}
			} else if tool.Name == "browser_session_capture" {
				if !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint || (tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint) {
					t.Fatal("Browser capture must be read-only, open-world and non-destructive")
				}
			} else if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatalf("%s can affect an open-world Browser session", tool.Name)
			}
			input, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"workspace"} {
				if !strings.Contains(string(input), `"`+field+`"`) {
					t.Fatalf("%s missing %s: %s", tool.Name, field, input)
				}
			}
			if tool.Name != "browser_session_open" && !strings.Contains(string(input), `"session_id"`) {
				t.Fatalf("%s lacks opaque session identity: %s", tool.Name, input)
			}
			if tool.Name == "browser_session_trace" {
				for _, field := range []string{"operation", "steps", "file_version", "offset", "session_id"} {
					if !strings.Contains(string(input), `"`+field+`"`) {
						t.Fatalf("trace schema missing %s: %s", field, input)
					}
				}
				for _, field := range []string{"512 KiB", "8192", "SHA256", "discard", "session"} {
					if !strings.Contains(tool.Description, field) {
						t.Fatalf("trace description missing %s", field)
					}
				}
			}
			if tool.Name == "browser_session_capture" {
				for _, field := range []string{"representation", "quality", "max_width", "session_id"} {
					if !strings.Contains(string(input), `"`+field+`"`) {
						t.Fatalf("screenshot schema missing %s: %s", field, input)
					}
				}
				if !strings.Contains(tool.Description, "32 KiB") || !strings.Contains(tool.Description, "SHA256") || !strings.Contains(tool.Description, "base64") {
					t.Fatal("screenshot missing limits, integrity, or text-only fallback description")
				}
			}
			if tool.Name == "browser_session_flow" {
				for _, field := range []string{"steps", "ref", "timeout_ms"} {
					if !strings.Contains(string(input), `"`+field+`"`) {
						t.Fatalf("session flow missing %s: %s", field, input)
					}
				}
				for _, description := range []string{"browser_e2e it is flow-only", "browser_session_flow it persists across calls"} {
					if !strings.Contains(string(input), description) {
						t.Fatalf("session flow ref schema contradicts tool lifetime: missing %q in %s", description, input)
					}
				}
			}
		case "browser_e2e":
			foundBrowserE2E = true
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint ||
				tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Fatal("browser_e2e must disclose open-world/action-capable behavior")
			}
			for _, label := range []string{"SAME managed Chromium/profile/admission/TLS/resource limits", "one browser execution", "not a cross-call live session", "refs expire on navigation"} {
				if !strings.Contains(tool.Description, label) {
					t.Fatalf("browser_e2e description missing %q", label)
				}
			}
			in, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"steps", "action", "url", "selector", "ref", "role", "name", "value", "expected", "timeout_ms", "ignore_https_errors"} {
				if !strings.Contains(string(in), `"`+field+`"`) {
					t.Fatalf("browser_e2e schema missing %q: %s", field, in)
				}
			}
			if !strings.Contains(string(in), "browser_e2e it is flow-only") {
				t.Fatalf("browser_e2e ref lifetime not clear: %s", in)
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
	for _, name := range []string{"browser_session_open", "browser_session_flow", "browser_session_capture", "browser_session_trace", "browser_session_status", "browser_session_close"} {
		if !foundBrowserSessions[name] {
			t.Errorf("missing managed Browser tool %s", name)
		}
	}
	if !foundEnvironment || !foundRunCommand || !foundRoot || !foundStartJob || !foundJobStatus || !foundReadFile || !foundWriteFile || !foundWorkerStat || !foundWorkerRead || !foundWorkerWrite || !foundWorkerApplyEdits || !foundWorkspaceSearch || !foundRepositoryEnvironment || !foundRepositoryDiscover || !foundRepositoryInspect || !foundWorktreeCreate || !foundWorktreeRemove || !foundBrowserStatus || !foundBrowser || !foundBrowserE2E {
		t.Fatalf("required tools missing: environment=%v root=%v start_job=%v job_status=%v read_file=%v write_file=%v workspace_search=%v repository_environment=%v repository_discover=%v repository_inspect=%v worktree_create=%v worktree_remove=%v browser_status=%v browser=%v browser_e2e=%v", foundEnvironment, foundRoot, foundStartJob, foundJobStatus, foundReadFile, foundWriteFile, foundWorkspaceSearch, foundRepositoryEnvironment, foundRepositoryDiscover, foundRepositoryInspect, foundWorktreeCreate, foundWorktreeRemove, foundBrowserStatus, foundBrowser, foundBrowserE2E)
	}
}

func TestInstructionsDescribePersistentWorkspace(t *testing.T) {
	got := instructions("/srv/ai-workspace")
	for _, want := range []string{
		"/srv/ai-workspace is persistent",
		"task environments",
		"prefer git worktree",
		"never delete dirty, untracked, ambiguous, or unknown workspace state",
		"workspace_stat to inspect size/version without content",
		"workspace_search mode=text for bounded literal/regex text occurrences",
		"workspace_search mode=structural for syntax-tree patterns",
		"LSP for semantic symbol/type/reference meaning",
		"rg/git grep through run_command as the advanced/unstructured CLI fallback",
		"Landlock ABI v2",
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

func TestNamedCredentialAuthAttachesServerDerivedPrincipal(t *testing.T) {
	cfg := testConfig(t, "bearer")
	token := strings.Repeat("a", credential.TokenHexLength)
	digest, err := credential.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(cfg.StateDir, "mcp-credentials.json")
	directToken := strings.Repeat("c", credential.TokenHexLength)
	directDigest, err := credential.VerifyToken(directToken)
	if err != nil {
		t.Fatal(err)
	}
	storeJSON := `{"version":1,"credentials":[{"principal":{"id":"direct-default","class":"direct","name":"direct/default"},"verifier_algorithm":"sha256-v1","verifier":"` + directDigest + `","created_at":"2026-10-08T00:00:00Z","enabled":true},{"principal":{"id":"mcp-gateway","class":"gateway","name":"mcp-gateway"},"verifier_algorithm":"sha256-v1","verifier":"` + digest + `","created_at":"2026-10-08T00:00:00Z","enabled":true}]}`
	if err := os.WriteFile(storePath, []byte(storeJSON), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.BearerTokenFile = ""
	cfg.CredentialStoreFile = storePath
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var got credential.Principal
	protected := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		got, ok = credential.PrincipalFromContext(r.Context())
		if !ok {
			t.Fatal("authenticated principal missing from request context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if got.ID != "mcp-gateway" || got.Class != "gateway" || got.Name != "mcp-gateway" {
		t.Fatalf("unexpected principal: %+v", got)
	}
}

func TestNamedCredentialStoreFailsClosedOnDuplicateVerifier(t *testing.T) {
	cfg := testConfig(t, "bearer")
	token := strings.Repeat("b", credential.TokenHexLength)
	digest, err := credential.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(cfg.StateDir, "mcp-credentials.json")
	storeJSON := `{"version":1,"credentials":[{"principal":{"id":"direct-default","class":"direct","name":"direct/default"},"verifier_algorithm":"sha256-v1","verifier":"` + digest + `","created_at":"2026-10-08T00:00:00Z","enabled":true},{"principal":{"id":"mcp-gateway","class":"gateway","name":"mcp-gateway"},"verifier_algorithm":"sha256-v1","verifier":"` + digest + `","created_at":"2026-10-08T00:00:00Z","enabled":true}]}`
	if err := os.WriteFile(storePath, []byte(storeJSON), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.BearerTokenFile = ""
	cfg.CredentialStoreFile = storePath
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "duplicate MCP credential verifier") {
		t.Fatalf("New() error = %v, want duplicate verifier rejection", err)
	}
}

func TestAuthenticatedAgentEnvironmentIncludesStableInstanceID(t *testing.T) {
	cfg := testConfig(t, "bearer")
	cfg.InstanceID = "asa_0123456789abcdef0123456789abcdef"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRequest(http.MethodGet, "/agent-environment.json", nil)
	unauthorizedRecorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated identity read returned HTTP %d", unauthorizedRecorder.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/agent-environment.json", nil)
	request.Header.Set("Authorization", "Bearer mcp-token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated identity read returned HTTP %d", response.Code)
	}
	var environment struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &environment); err != nil {
		t.Fatal(err)
	}
	if environment.InstanceID != cfg.InstanceID {
		t.Fatalf("authenticated instance_id = %q, want %q", environment.InstanceID, cfg.InstanceID)
	}
}
