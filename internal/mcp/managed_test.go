package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIssue65ToolsDiscoveredWithoutCredentialInput(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "issue65-tool-acceptance", Version: "0"}, nil)
	c := ts.Client()
	c.Transport = bearerRoundTripper{base: c.Transport, token: "mcp-token"}
	ss, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: ts.URL + cfg.MCPPath, HTTPClient: c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	result, err := ss.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"managed_environment": false, "service_access": false, "browser_session_credential_login": false}
	for _, tool := range result.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("%s: mutation warning missing", tool.Name)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"secret":`, `"password":`, `"credential_value":`} {
			if bytes.Contains(schema, []byte(field)) {
				t.Errorf("%s exposed raw secret parameter %s", tool.Name, field)
			}
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("Issue #65 tool not discoverable: %s", n)
		}
	}
}
