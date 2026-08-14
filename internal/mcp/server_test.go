package mcp

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	d := t.TempDir()
	et := filepath.Join(d, "exec")
	bt := filepath.Join(d, "mcp")
	os.WriteFile(et, []byte("exec-token\n"), 0600)
	os.WriteFile(bt, []byte("mcp-token\n"), 0600)
	c := config.Default()
	c.ExecutorToken = et
	c.BearerTokenFile = bt
	c.StateDir = d
	c.LogDir = d
	c.WorkspaceDir = d
	c.AuthMode = "bearer"
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestToolsList(t *testing.T) {
	s := testServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer mcp-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["result"] == nil {
		t.Fatal("missing result")
	}
}
func TestUnauthorized(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
}
