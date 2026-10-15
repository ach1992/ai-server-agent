package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A source map adjacent to JS is not a sufficient authority: it must be
// linked by the exact executable, have the correct schema, stay within the
// same worker workspace and version-pin all original source files.
func TestTypeScriptDAPSourceMapVersionsAndUnsafeTargets(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.workspaceHelperBinary = buildNodeProofWorkspaceHelper(t)
	owner.Workspace = workspace
	original := filepath.Join(workspace, "main.ts")
	compiled := filepath.Join(workspace, "main.js")
	mapped := filepath.Join(workspace, "main.js.map")
	if err := os.WriteFile(original, []byte("const count: number = 7;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	js := "const count = 7;\n//# sourceMappingURL=main.js.map\n"
	if err := os.WriteFile(compiled, []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	mapVersion := func(file, root string, sources []string) {
		t.Helper()
		b, e := json.Marshal(map[string]any{
			"version": 3, "file": file, "sourceRoot": root, "sources": sources, "names": []string{}, "mappings": "AAAA",
		})
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(mapped, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	stat := func(rel string) string {
		t.Helper()
		r := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: rel})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("safe workspace stat failed for %s: %+v", rel, r)
		}
		return r.FileVersion
	}
	mapVersion("main.js", "", []string{"main.ts"})
	version := stat("main.js")
	versions, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", version)
	if err != nil || len(versions) != 3 || versions["main.ts"] != stat("main.ts") ||
		versions["main.js.map"] != stat("main.js.map") || versions["main.js"] != version {
		t.Fatalf("valid compiled JS/.map/.ts graph not pinned: %+v %v", versions, err)
	}
	if err := os.WriteFile(original, []byte("const count: number = 9;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if versions["main.ts"] == stat("main.ts") {
		t.Fatal("modified original TypeScript kept old file_version")
	}
	for _, tc := range []struct {
		name, file, root string
		sources          []string
	}{
		{"wrong_file", "other.js", "", []string{"main.ts"}},
		{"remote_target", "main.js", "", []string{"https://example.com/evil.ts"}},
		{"external_root", "main.js", "../../", []string{"main.ts"}},
		{"absolute_target", "main.js", "", []string{"/etc/secret.ts"}},
		{"escape_workspace", "main.js", "", []string{"../outside.ts"}},
		{"unsupported_source", "main.js", "", []string{"main.php"}},
		{"missing_source", "main.js", "", []string{"not-there.ts"}},
		{"too_many_files", "main.js", "", func() []string {
			a := make([]string, 17)
			for i := range a {
				a[i] = "main.ts"
			}
			return a
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapVersion(tc.file, tc.root, tc.sources)
			if _, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", version); err == nil {
				t.Fatal("invalid source map was accepted")
			}
		})
	}
	mapVersion("main.js", "", []string{"main.ts"})
	if err := os.WriteFile(compiled, []byte("const count = 7;\n//# sourceMappingURL=https://example.com/map.js.map\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wrong := stat("main.js")
	if _, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", wrong); err == nil {
		t.Fatal("remote/mismatched sourcemap link accepted")
	}
	if _, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", version); err == nil {
		t.Fatal("stale compiled JS file_version accepted")
	}
	if err := os.WriteFile(compiled, []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	version = stat("main.js")
	if err := os.Remove(mapped); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, mapped); err != nil {
		t.Fatal(err)
	}
	if _, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", version); err == nil {
		t.Fatal("symlinked source map accepted")
	}
	if err := os.Remove(mapped); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mapped, []byte(strings.Repeat("x", tsDAPMaxSourceMapBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.inspectPrecompiledTypeScriptMap(context.Background(), owner, "main.js", version); err == nil {
		t.Fatal("oversized source map accepted")
	}
}
