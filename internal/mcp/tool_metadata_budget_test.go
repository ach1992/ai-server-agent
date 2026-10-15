package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPToolMetadataFootprint(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "metadata-budget-test", Version: "v0"}, nil)
	h := ts.Client()
	h.Transport = bearerRoundTripper{base: h.Transport, token: "mcp-token"}
	session, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: ts.URL + cfg.MCPPath, HTTPClient: h}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name                                     string
		descriptionBytes, schemaBytes, fullBytes int
	}
	var rows []row
	totalDescription, totalMetadata, totalSchema, totalInput, totalOutput := 0, 0, 0, 0, 0
	for _, tool := range res.Tools {
		full, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		in, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		schema := len(in) + len(out)
		totalInput += len(in)
		totalOutput += len(out)
		rows = append(rows, row{tool.Name, len(tool.Description), schema, len(full)})
		totalDescription += len(tool.Description)
		totalMetadata += len(full)
		totalSchema += schema
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].descriptionBytes > rows[j].descriptionBytes })
	// Model-facing descriptions should identify the action, critical constraints,
	// and recovery behavior; long implementation tutorials belong in docs.
	const maxDescriptionBytes = 750
	const maxAverageDescriptionBytes = 260
	// This snapshot is tied to the published v0.1.13 catalog. Update the
	// versioned tool-reference count when intentionally changing the inventory.
	const expectedCatalogCount = 59
	if len(rows) != expectedCatalogCount {
		t.Fatalf("MCP tools/list count = %d; expected %d (update docs/TOOL-REFERENCE.md on intentional change)", len(rows), expectedCatalogCount)
	}
	if rows[0].descriptionBytes > maxDescriptionBytes {
		t.Errorf("longest MCP tool description %q: %d > %d bytes", rows[0].name, rows[0].descriptionBytes, maxDescriptionBytes)
	}
	if totalDescription > len(rows)*maxAverageDescriptionBytes {
		t.Errorf("MCP tool descriptions average %.1f > %d bytes", float64(totalDescription)/float64(len(rows)), maxAverageDescriptionBytes)
	}

	t.Logf("TOOLS=%d TOOL_DESC_BYTES=%d TOOL_SCHEMA_BYTES=%d INPUT_BYTES=%d OUTPUT_BYTES=%d TOOL_METADATA_JSON_BYTES=%d INITIALIZE_INSTRUCTIONS_BYTES=%d", len(rows), totalDescription, totalSchema, totalInput, totalOutput, totalMetadata, len(session.InitializeResult().Instructions))
	for i, record := range rows {
		if i >= 5 {
			break
		}
		t.Logf("DESCRIPTION_TOP %s bytes=%d schema=%d full=%d", record.name, record.descriptionBytes, record.schemaBytes, record.fullBytes)
	}
}
