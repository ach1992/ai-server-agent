package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteIncludesNonSecretPrincipalContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := New(path)
	if err := logger.Write(Entry{
		Action:         "run",
		Mode:           "worker",
		Success:        true,
		PrincipalID:    "mcp-gateway",
		PrincipalClass: "gateway",
		PrincipalName:  "mcp-gateway",
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &got); err != nil {
		t.Fatal(err)
	}
	if got.PrincipalID != "mcp-gateway" || got.PrincipalClass != "gateway" || got.PrincipalName != "mcp-gateway" {
		t.Fatalf("unexpected principal context: %+v", got)
	}
}
