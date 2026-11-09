package mcp

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPPrerequisiteGuidanceReachesStructuredAndTextOnlyClients(t *testing.T) {
	input := executor.Response{
		OK: false, ErrorClass: "dependency", ErrorCode: "service_client_unavailable",
		ReasonCode: "service_client_unavailable", RequestID: "req-prereq-245",
		Error: "sshpass optional native executable unavailable", Output: "native stderr evidence",
		OutputEncoding: "utf-8", BytesSeen: 22, BytesReturned: 22,
	}
	for _, large := range []bool{false, true} {
		source := input
		if large {
			source.Output = strings.Repeat("N", 8192)
			source.BytesSeen = int64(len(source.Output))
			source.BytesReturned = source.BytesSeen
		}
		wire, structured, err := responseResultWithLimit(source, 4096)
		if err != nil || wire == nil || !wire.IsError || len(wire.Content) != 1 {
			t.Fatalf("MCP dependency response failed for large=%t: %+v %v", large, wire, err)
		}
		txt, ok := wire.Content[0].(*mcpsdk.TextContent)
		if !ok {
			t.Fatalf("MCP fallback did not provide text: %T", wire.Content[0])
		}
		if structured.PrerequisiteCode != "service_client_unavailable" ||
			!strings.Contains(structured.OperatorHint, "service_use") ||
			structured.Error != source.Error || structured.Output != source.Output ||
			structured.RequestID != source.RequestID || structured.BytesSeen != source.BytesSeen ||
			!strings.Contains(txt.Text, "operator_hint") ||
			!strings.Contains(txt.Text, "prerequisite_code") ||
			!strings.Contains(txt.Text, "req-prereq-245") {
			t.Fatalf("MCP response lost prerequisite or original evidence for large=%t: structured=%+v text=%q", large, structured, txt.Text)
		}
		if large && strings.Contains(txt.Text, source.Output) {
			t.Fatal("large native output should remain in structuredContent, not unbounded text fallback")
		}
	}
}
