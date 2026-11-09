package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/browser"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Runtime inspection and Executor admission are separate evidence. A failed
// admission probe must not erase an already-verified installed/ready runtime.
func TestBrowserStatusHintsKeepIndependentReadinessEvidence(t *testing.T) {
	tests := []struct {
		name   string
		status browser.RuntimeStatus
		want   string
		avoid  string
	}{
		{
			name: "runtime inspected but admission unverified",
			status: browser.RuntimeStatus{
				Installed: true, Ready: true, Busy: true,
				Reason: "shared Browser profile admission could not be verified",
			},
			want:  "runtime is installed and ready, but session admission could not be verified",
			avoid: "no installation state can be inferred",
		},
		{
			name: "installed but not ready and admission unverified",
			status: browser.RuntimeStatus{Installed: true, Busy: true,
				Reason: "shared Browser profile admission could not be verified"},
			want:  "runtime was found, but readiness or session admission is unverified",
			avoid: "runtime was not found",
		},
		{
			name: "runtime inspection never completed",
			status: browser.RuntimeStatus{Busy: true,
				Reason: "browser operation in progress; readiness was not inspected"},
			want:  "runtime inspection was incomplete",
			avoid: "runtime was not found",
		},
		{
			name: "runtime absent after completed inspection",
			status: browser.RuntimeStatus{InspectionComplete: true,
				Reason: "browser runtime is not installed"},
			want:  "runtime was not found",
			avoid: "runtime is installed and ready",
		},
		{
			name: "shared busy but isolated admission independently available",
			status: browser.RuntimeStatus{InspectionComplete: true, Installed: true, Ready: true,
				Busy: true, IsolatedSlotsAvailable: 1},
			want:  "independently reported isolated slots are available",
			avoid: "admission could not be verified",
		},
		{
			name: "shared busy without verified isolated availability",
			status: browser.RuntimeStatus{InspectionComplete: true, Installed: true,
				Ready: true, Busy: true},
			want:  "isolated admission availability is reported separately",
			avoid: "independently reported isolated slots are available",
		},
		{
			name:   "fully ready",
			status: browser.RuntimeStatus{InspectionComplete: true, Installed: true, Ready: true},
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hint := browserStatusOperatorHint(tc.status)
			if !strings.Contains(hint, tc.want) || (tc.avoid != "" && strings.Contains(hint, tc.avoid)) {
				t.Fatalf("contradictory Browser hint %q for %+v", hint, tc.status)
			}
			if tc.want == "" && hint != "" {
				t.Fatalf("invented unnecessary Browser guidance: %q", hint)
			}
		})
	}
}

// The names, descriptions and input schemas actually sent by authenticated
// MCP tools/list must agree with their native Executor semantics.
func TestAuthenticatedMCPManagedAndLSPDescriptionsMatchOperationScope(t *testing.T) {
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
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "description-review-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: ts.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcpsdk.Tool{}
	for _, tool := range list.Tools {
		byName[tool.Name] = tool
	}
	managed := byName["managed_environment"]
	if managed == nil {
		t.Fatal("managed_environment missing")
	}
	for _, fragment := range []string{"pending_creates", "native guest state", "operation_status", "snapshot/restore", "approval"} {
		if !strings.Contains(managed.Description, fragment) {
			t.Fatalf("managed description missing %q: %s", fragment, managed.Description)
		}
	}
	if strings.Contains(managed.Description, "inspect operation_status before resubmitting") {
		t.Fatal("create still incorrectly points to snapshot/restore operation_status")
	}
	schema, err := json.Marshal(managed.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"pending_creates", "snapshot/restore journal", "native UUID", "operation_id"} {
		if !strings.Contains(string(schema), fragment) {
			t.Fatalf("managed input schema missing %q: %s", fragment, schema)
		}
	}

	for _, name := range []string{"code_definition", "code_references", "code_symbols", "code_diagnostics"} {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("%s missing from authenticated MCP tools/list", name)
		}
		for _, fragment := range []string{"exact file_version", "sql_dialect only for SQL source files", "omit it for non-SQL files"} {
			if !strings.Contains(tool.Description, fragment) {
				t.Fatalf("%s description missing %q: %s", name, fragment, tool.Description)
			}
		}
		if strings.Contains(tool.Description, "requires exact file_version and explicit SQL dialect") {
			t.Fatalf("%s incorrectly requires SQL dialect for non-SQL source", name)
		}
		input, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range []string{"file_version", "sql_dialect", "Required for SQL files"} {
			if !strings.Contains(string(input), fragment) {
				t.Fatalf("%s schema missing %q: %s", name, fragment, input)
			}
		}
	}
}
