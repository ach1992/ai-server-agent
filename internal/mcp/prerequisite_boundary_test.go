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
			!strings.Contains(structured.OperatorHint, "native client") ||
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

// The production worker helper emits workspace_sandbox_unavailable with
// ErrorClass=security, not dependency. MCP must keep the security refusal while
// exposing a useful alternative, without bypassing Landlock.
func TestSecurityWorkspacePrerequisiteSurvivesMCPResponse(t *testing.T) {
	in := executor.Response{
		OK: false, ErrorCode: "workspace_sandbox_unavailable",
		ReasonCode: "workspace_sandbox_unavailable", ErrorClass: "security",
		Error: "Landlock ABI unavailable", RequestID: "req-landlock-245",
		Output: "native worker diagnostic", OutputEncoding: "utf-8",
	}
	wire, structured, err := responseResultWithLimit(in, 4096)
	if err != nil || wire == nil || !wire.IsError {
		t.Fatalf("security failure not delivered via MCP: %+v %v", wire, err)
	}
	if structured.ErrorClass != "security" ||
		structured.ErrorCode != "workspace_sandbox_unavailable" ||
		structured.PrerequisiteCode != "workspace_sandbox_unavailable" ||
		!strings.Contains(structured.OperatorHint, "Landlock") ||
		!strings.Contains(structured.OperatorHint, "no bypass") ||
		structured.RequestID != in.RequestID ||
		structured.Error != in.Error || structured.Output != in.Output {
		t.Fatalf("MCP security path changed provider refusal or lost recovery details: %+v", structured)
	}
	text := wire.Content[0].(*mcpsdk.TextContent).Text
	for _, marker := range []string{"req-landlock-245", "operator_hint", "security", "Landlock"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("text-only MCP dropped %q: %s", marker, text)
		}
	}
}

func TestUnsupportedSemanticOperationIsNotAPackagePrerequisite(t *testing.T) {
	in := executor.Response{OK: false, ErrorCode: "code_capability_unsupported",
		ErrorClass: "validation", ReasonCode: "code_capability_unsupported",
		Error: "selected language server lacks this method"}
	wire, structured, err := responseResultWithLimit(in, 4096)
	if err != nil || wire == nil || !wire.IsError ||
		structured.ErrorCode != in.ErrorCode || structured.OperatorHint != "" ||
		structured.PrerequisiteCode != "" || structured.Error != in.Error {
		t.Fatalf("unsupported semantic method was mislabeled as a repairable package: %+v %v", structured, err)
	}
}
