package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPEnvironmentPurposeKeepsToolInventory(t *testing.T) {
	var reference []string
	for _, role := range []string{"development", "validation"} {
		t.Run(role, func(t *testing.T) {
			cfg := testConfig(t, "bearer")
			cfg.EnvironmentProfile = role
			cfg.ServerLabel = "Agent QA آزمایش"
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			httpClient := ts.Client()
			httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
			client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "purpose-profile-test", Version: "v1"}, nil)
			session, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: ts.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			instructions := session.InitializeResult().Instructions
			if !strings.Contains(instructions, "Environment purpose: "+role) ||
				!strings.Contains(instructions, "guidance; it is not an authorization") {
				t.Fatalf("MCP role instructions missing: %q", instructions)
			}
			env, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "agent_environment", Arguments: map[string]any{}})
			if err != nil || env.IsError || len(env.Content) == 0 {
				t.Fatalf("environment call: %+v %v", env, err)
			}
			out, ok := env.Content[0].(*mcpsdk.TextContent)
			if !ok {
				t.Fatalf("environment result not a text block: %T", env.Content[0])
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(out.Text), &data); err != nil {
				t.Fatal(err)
			}
			if data["environment_profile"] != role || data["server_label"] != "Agent QA آزمایش" {
				t.Fatalf("role and label missing from manifest: %+v", data)
			}
			list, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(list.Tools))
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
			}
			sort.Strings(names)
			if reference == nil {
				reference = names
			} else if !reflect.DeepEqual(reference, names) {
				t.Fatalf("profile changed available MCP tool list")
			}
		})
	}
}
