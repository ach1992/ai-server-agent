package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func environmentTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	return &Server{
		cfg:       config.Config{WorkspaceDir: root},
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
		runs:      newRunLimiterWith(2, 1),
	}, root
}

func environmentGitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Environment Test",
		"GIT_AUTHOR_EMAIL=environment-test@example.invalid",
		"GIT_COMMITTER_NAME=Environment Test",
		"GIT_COMMITTER_EMAIL=environment-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func initEnvironmentFixture(t *testing.T, root string, files map[string]string) (string, string) {
	t.Helper()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	environmentGitFixture(t, repo, "init")
	environmentGitFixture(t, repo, "branch", "-M", "main")
	for path, content := range files {
		full := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	environmentGitFixture(t, repo, "add", ".")
	environmentGitFixture(t, repo, "commit", "-m", "fixture")
	return repo, environmentGitFixture(t, repo, "rev-parse", "HEAD")
}

func installFakeEnvironmentTool(t *testing.T, root, group, name, output string) string {
	t.Helper()
	path := filepath.Join(root, ".toolchains", group, "bin", name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(output, "'", "'\\''") + "'\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func environmentToolByExecutable(summary RepositoryEnvironmentSummary, executable string) (EnvironmentTool, bool) {
	for _, tool := range summary.Tools {
		if tool.Executable == executable {
			return tool, true
		}
	}
	return EnvironmentTool{}, false
}

func hasEnvironmentLanguage(summary RepositoryEnvironmentSummary, name string) bool {
	for _, language := range summary.Languages {
		if language.Name == name {
			return true
		}
	}
	return false
}

func hasEnvironmentMechanism(summary RepositoryEnvironmentSummary, name string) (EnvironmentMechanism, bool) {
	for _, mechanism := range summary.Mechanisms {
		if mechanism.Name == name {
			return mechanism, true
		}
	}
	return EnvironmentMechanism{}, false
}

func TestRepositoryEnvironmentFreshGoRepoReusesWorkerCache(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, head := initEnvironmentFixture(t, root, map[string]string{
		"go.mod":   "module example.invalid/project\n\ngo 1.26.0\n",
		"Makefile": "test:\n\t@echo test\n",
	})
	fakeGo := installFakeEnvironmentTool(t, root, "go1.27.1", "go", "go version go1.27.1 linux/amd64")

	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	if summary.RepositoryHead != head || summary.HostStatus != "ready" || !hasEnvironmentLanguage(summary, "go") {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	tool, ok := environmentToolByExecutable(summary, "go")
	if !ok || !tool.Available || tool.Compatibility != "compatible" || tool.Source != "worker_cache" || tool.Path != fakeGo {
		t.Fatalf("Go cache was not safely reused: %+v", tool)
	}
	if len(summary.Entrypoints) == 0 || summary.Entrypoints[0].Command != "make test" {
		t.Fatalf("Makefile entrypoint not discovered: %+v", summary.Entrypoints)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("environment discovery created hidden local state: before=%d after=%d", len(before), len(after))
	}
}

func TestRepositoryEnvironmentReportsIncompatibleToolClearly(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/project\n\ngo 1.26.0\n",
	})
	installFakeEnvironmentTool(t, root, "go1.25.0", "go", "go version go1.25.0 linux/amd64")

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	tool, ok := environmentToolByExecutable(summary, "go")
	if !ok || tool.Compatibility != "incompatible" || summary.HostStatus != "unsatisfied" {
		t.Fatalf("incompatible Go was not explicit: tool=%+v summary=%+v", tool, summary)
	}
}

func TestRepositoryEnvironmentDetectsMechanismsWithoutChoosingPrecedence(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"mise.toml":                       "[tools]\ngo = \"1.26\"\n",
		"devenv.nix":                      "{ pkgs, ... }: {}\n",
		".devcontainer/devcontainer.json": "{\"name\":\"dev\"}\n",
		"dagger.json":                     "{\"name\":\"project\"}\n",
	})
	installFakeEnvironmentTool(t, root, "mise-2026", "mise", "2026.10.0")
	installFakeEnvironmentTool(t, root, "go1.26", "go", "go version go1.26.4 linux/amd64")

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	if !summary.SelectionRequired || !summary.IsolationDeclared || summary.IsolationRequirement != "unknown_from_declarations" || len(summary.Mechanisms) != 4 {
		t.Fatalf("multiple mechanisms were not surfaced without precedence: %+v", summary)
	}
	mise, ok := hasEnvironmentMechanism(summary, "mise")
	if !ok || !mise.Available {
		t.Fatalf("declared existing mise was not reused: %+v", mise)
	}
	if _, ok := hasEnvironmentMechanism(summary, "devenv"); !ok {
		t.Fatal("devenv declaration missing")
	}
	if _, ok := hasEnvironmentMechanism(summary, "devcontainer"); !ok {
		t.Fatal("devcontainer declaration missing")
	}
	if _, ok := hasEnvironmentMechanism(summary, "dagger"); !ok {
		t.Fatal("dagger declaration missing")
	}
}

func TestRepositoryEnvironmentReportsPackageManagerAmbiguityAndScriptsWithoutExecuting(t *testing.T) {
	s, root := environmentTestServer(t)
	marker := filepath.Join(root, "script-ran")
	packageJSON := `{"engines":{"node":">=20"},"scripts":{"test":"touch ` + marker + `","build":"echo build"}}`
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"package.json":      packageJSON,
		"package-lock.json": "{}\n",
		"pnpm-lock.yaml":    "lockfileVersion: '9.0'\n",
	})

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	if !summary.SelectionRequired || !strings.Contains(summary.SelectionReason, "package-manager") {
		t.Fatalf("lockfile ambiguity was not explicit: %+v", summary)
	}
	if len(summary.Entrypoints) != 2 {
		t.Fatalf("package scripts were not reported: %+v", summary.Entrypoints)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository script executed during discovery: %v", err)
	}
}

func TestRepositoryEnvironmentMarksUntrackedDeclarationAsNonDurable(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/project\n\ngo 1.26.0\n",
	})
	installFakeEnvironmentTool(t, root, "go1.26", "go", "go version go1.26.1 linux/amd64")
	if err := os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("go 1.26.1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	found := false
	for _, declaration := range summary.Declarations {
		if declaration.Path == ".tool-versions" {
			found = true
			if declaration.Tracked {
				t.Fatal("untracked declaration was reported as durable")
			}
		}
	}
	if !found {
		t.Fatal("untracked declaration was not surfaced")
	}
	joined := strings.Join(summary.Warnings, "\n")
	if !strings.Contains(joined, "not Git-tracked") {
		t.Fatalf("durability warning missing: %v", summary.Warnings)
	}
}

func TestRepositoryEnvironmentConflictingExactPinsFailClearly(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		".tool-versions": "go 1.26.0\n",
		"mise.toml":      "[tools]\ngo = \"1.27.0\"\n",
	})
	installFakeEnvironmentTool(t, root, "go1.27", "go", "go version go1.27.0 linux/amd64")
	installFakeEnvironmentTool(t, root, "mise", "mise", "2026.10.0")

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	tool, ok := environmentToolByExecutable(summary, "go")
	if !ok || tool.Compatibility != "conflict" || summary.HostStatus != "unsatisfied" {
		t.Fatalf("conflicting repository pins were not fail-closed: tool=%+v summary=%+v", tool, summary)
	}
}

func TestRepositoryEnvironmentRejectsPathEscape(t *testing.T) {
	s, root := environmentTestServer(t)
	_, _ = initEnvironmentFixture(t, root, map[string]string{"go.mod": "module example.invalid/project\n\ngo 1.26.0\n"})
	outside := t.TempDir()
	if _, code, err := s.inspectRepositoryEnvironment(context.Background(), outside); err == nil || code != "invalid_repository_path" {
		t.Fatalf("outside workspace repository accepted: code=%s err=%v", code, err)
	}
}

func TestRepositoryEnvironmentIgnoresDeclarationThroughSymlinkParent(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/project\n\ngo 1.26.0\n",
	})
	installFakeEnvironmentTool(t, root, "go1.26", "go", "go version go1.26.1 linux/amd64")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "devcontainer.json"), []byte(`{"name":"outside"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".devcontainer")); err != nil {
		t.Fatal(err)
	}

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	if _, ok := hasEnvironmentMechanism(summary, "devcontainer"); ok {
		t.Fatal("declaration through symlink parent was trusted")
	}
	if !strings.Contains(strings.Join(summary.Warnings, "\n"), "symlink/path indirection") {
		t.Fatalf("symlink rejection warning missing: %v", summary.Warnings)
	}
}

func TestEnvironmentToolVersionBoundsOutput(t *testing.T) {
	s, root := environmentTestServer(t)
	path := filepath.Join(root, ".toolchains", "go-flood", "bin", "go")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 70000 ]; do printf x; i=$((i+1)); done\nprintf '\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.environmentToolVersion(context.Background(), path, "go"); err == nil {
		t.Fatal("oversized version output was accepted")
	}
}

func TestEnvironmentExactRequirementTreatsMissingPatchAsSameVersionFamily(t *testing.T) {
	requirements := []EnvironmentRequirement{
		{Value: "1.26", Mode: "exact", DeclaredBy: "mise.toml", Ownership: "repository"},
		{Value: "1.26.0", Mode: "exact", DeclaredBy: ".tool-versions", Ownership: "repository"},
	}
	compatibility, reason := evaluateEnvironmentRequirements("go version go1.26.0 linux/amd64", requirements)
	if compatibility != "compatible" {
		t.Fatalf("equivalent exact pins conflicted: compatibility=%s reason=%s", compatibility, reason)
	}
}

func TestRepositoryEnvironmentSameNodeManagerLocksDoNotInventConflict(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"package.json":        `{}`,
		"package-lock.json":   `{}`,
		"npm-shrinkwrap.json": `{}`,
	})
	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	if summary.SelectionRequired {
		t.Fatalf("same package manager lockfiles caused false selection conflict: %+v", summary)
	}
}

func TestRepositoryEnvironmentKeepsGoMinimumAndToolchainRequirements(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/project\n\ngo 1.26.0\ntoolchain go1.27.1\n",
	})
	installFakeEnvironmentTool(t, root, "go1.27.1", "go", "go version go1.27.1 linux/amd64")
	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	tool, ok := environmentToolByExecutable(summary, "go")
	if !ok || len(tool.Requirements) != 2 {
		t.Fatalf("Go minimum/toolchain requirements were not both retained: %+v", tool)
	}
}

func TestRepositoryEnvironmentWithoutKnownDeclarationsIsUnknown(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{"README.md": "fixture\n"})
	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	if summary.HostStatus != "unknown" || !strings.Contains(strings.Join(summary.Warnings, "\n"), "no recognized") {
		t.Fatalf("empty environment intent was falsely treated as ready: %+v", summary)
	}
}

func TestRepositoryEnvironmentSelectsCompatibleWorkerCacheWithoutExecutingIt(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		".python-version": "99.0.0\n",
	})
	marker := filepath.Join(root, "worker-cache-executed")
	path := filepath.Join(root, ".toolchains", "python99.0.0", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf 'Python 99.0.0\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	tool, ok := environmentToolByExecutable(summary, "python3")
	if !ok || tool.Source != "worker_cache" || tool.Version != "99.0.0" || tool.Compatibility != "compatible" {
		t.Fatalf("compatible worker cache was not selected: %+v", tool)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker-cache executable ran during read-only discovery: %v", err)
	}
}

func TestRepositoryEnvironmentMarksTrackedModifiedDeclarationAsNonDurable(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/project\n\ngo 1.26.0\n",
	})
	installFakeEnvironmentTool(t, root, "go1.27", "go", "go version go1.27.0 linux/amd64")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.invalid/project\n\ngo 1.27.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	found := false
	for _, declaration := range summary.Declarations {
		if declaration.Path != "go.mod" {
			continue
		}
		found = true
		if !declaration.Tracked || declaration.MatchesHead {
			t.Fatalf("modified tracked declaration durability wrong: %+v", declaration)
		}
	}
	if !found {
		t.Fatal("go.mod declaration missing")
	}
	if !strings.Contains(strings.Join(summary.Warnings, "\n"), "differs from HEAD") {
		t.Fatalf("tracked modification durability warning missing: %v", summary.Warnings)
	}
}

func TestTrustedSystemExecutableRejectsWorkerOwnedBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if trustedSystemExecutable(path) {
		t.Fatal("worker-owned executable was treated as trusted system tooling")
	}
}
