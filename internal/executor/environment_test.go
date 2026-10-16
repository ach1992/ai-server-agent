package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRepositoryEnvironmentFreshGoRepoSurfacesWorkerCacheWithoutTrustingIt(t *testing.T) {
	s, root := environmentTestServer(t)
	// Isolate only Go discovery; host Make remains available for entrypoint tests.
	s.environmentGoSystemPath = filepath.Join(root, "absent-system-tools")
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
	if summary.RepositoryHead != head || summary.HostStatus != "unknown" || !hasEnvironmentLanguage(summary, "go") {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	tool, ok := environmentToolByExecutable(summary, "go")
	if !ok || !tool.Available || tool.Compatibility != "unknown" || tool.Reason != "worker_cache_version_hint_not_verified" || tool.Source != "worker_cache" || tool.Path != fakeGo || tool.Version != "1.27.1" {
		t.Fatalf("Go cache candidate was not surfaced safely: %+v", tool)
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

func TestRepositoryEnvironmentWorkerCacheHintRemainsUnverified(t *testing.T) {
	s, root := environmentTestServer(t)
	// A trusted host Go must not shadow this worker-cache-only fixture.
	s.environmentGoSystemPath = filepath.Join(root, "absent-system-tools")
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/project\n\ngo 1.26.0\n",
	})
	installFakeEnvironmentTool(t, root, "go1.25.0", "go", "go version go1.25.0 linux/amd64")

	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	tool, ok := environmentToolByExecutable(summary, "go")
	if !ok || tool.Compatibility != "unknown" || tool.Reason != "worker_cache_version_hint_not_verified" || tool.Version != "1.25.0" || summary.HostStatus != "unknown" {
		t.Fatalf("worker-cache hint was over-trusted: tool=%+v summary=%+v", tool, summary)
	}
	compatibility, reason := evaluateEnvironmentRequirements("go version go1.25.0 linux/amd64", tool.Requirements)
	if compatibility != "incompatible" || reason != "host_version_does_not_satisfy_repository_requirement" {
		t.Fatalf("known version incompatibility was not explicit: compatibility=%s reason=%s", compatibility, reason)
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
	for _, entrypoint := range summary.Entrypoints {
		if entrypoint.Command != "" {
			t.Fatalf("package script body leaked into environment summary: %+v", entrypoint)
		}
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
	if !ok || tool.Compatibility != "conflict" || summary.HostStatus != "unsatisfied" || !summary.SelectionRequired {
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

func TestRepositoryEnvironmentSurfacesWorkerCacheHintWithoutExecutingIt(t *testing.T) {
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
	if !ok || tool.Source != "worker_cache" || tool.Version != "99.0.0" || tool.Compatibility != "unknown" || tool.Reason != "worker_cache_version_hint_not_verified" {
		t.Fatalf("worker cache hint was not surfaced safely: %+v", tool)
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

func TestRepositoryEnvironmentDoesNotExposeOpaqueRequirementValue(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"mise.toml": "[tools]\nnode = \"https://user:secret@example.invalid/node.tar.gz?token=hidden\"\n",
	})
	summary, code, err := s.inspectRepositoryEnvironment(context.Background(), repo)
	if err != nil {
		t.Fatalf("inspect: code=%s err=%v", code, err)
	}
	tool, ok := environmentToolByExecutable(summary, "node")
	if !ok || len(tool.Requirements) != 1 || tool.Requirements[0].Value != "[not_exposed]" || tool.Requirements[0].Mode != "opaque" {
		t.Fatalf("opaque requirement was not safely represented: %+v", tool)
	}
	joined := strings.Join([]string{summary.SelectionReason, strings.Join(summary.Warnings, "\n"), tool.Requirements[0].Value}, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "token=hidden") {
		t.Fatalf("opaque requirement leaked sensitive-looking source value: %s", joined)
	}
}

// A real Go version probe must not leave telemetry writers able to recreate
// the disposable probe HOME after the synchronous command returns. This is
// intentionally exercised with the installed Go binary rather than a fake
// process that never starts Go telemetry.
func TestEnvironmentToolVersionGoRealProcessDoesNotLeakTempProbe(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go is unavailable for the real-process telemetry regression")
	}
	s, _ := environmentTestServer(t)
	probeRoot := t.TempDir()
	t.Setenv("TMPDIR", probeRoot)
	for i := 0; i < 18; i++ {
		version, err := s.environmentToolVersion(context.Background(), goBinary, "go")
		if err != nil {
			t.Fatalf("real Go version probe %d: %v", i, err)
		}
		if !strings.HasPrefix(version, "go version go") {
			t.Fatalf("unexpected Go version response %d: %q", i, version)
		}
	}
	// A Go telemetry flush may outlive go version; detect files/directories
	// recreated after the ordinary RemoveAll has already returned.
	time.Sleep(150 * time.Millisecond)
	entries, err := os.ReadDir(probeRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ai-server-agent-env-probe-") {
			t.Fatalf("real Go probe left a telemetry HOME after completion: %s", entry.Name())
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 5; i++ {
		if _, err := s.environmentToolVersion(ctx, goBinary, "go"); err == nil {
			t.Fatalf("cancelled version probe %d unexpectedly succeeded", i)
		}
	}
	entries, err = os.ReadDir(probeRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ai-server-agent-env-probe-") {
			t.Fatalf("cancelled probe left a disposable HOME: %s", entry.Name())
		}
	}
}

// The disposable Go telemetry mode must be visible to the worker subprocess,
// without mutating the invoking user's actual HOME or Go telemetry mode.
func TestEnvironmentToolVersionGoProbeModeIsScoped(t *testing.T) {
	s, root := environmentTestServer(t)
	probeRoot := t.TempDir()
	t.Setenv("TMPDIR", probeRoot)
	outsideConfig := filepath.Join(root, "real-config")
	t.Setenv("XDG_CONFIG_HOME", outsideConfig)
	fakeGo := filepath.Join(root, "fake-go")
	script := "#!/bin/sh\n" +
		"test -r \"$XDG_CONFIG_HOME/go/telemetry/mode\" || exit 82\n" +
		"grep -Eq '^off [0-9]{4}-[0-9]{2}-[0-9]{2}$' \"$XDG_CONFIG_HOME/go/telemetry/mode\" || exit 83\n" +
		"printf 'go version go1.27.1 linux/amd64\\n'\n"
	if err := os.WriteFile(fakeGo, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	version, err := s.environmentToolVersion(context.Background(), fakeGo, "go")
	if err != nil || version != "go version go1.27.1 linux/amd64" {
		t.Fatalf("disposable worker mode was unavailable to the probe: version=%q err=%v", version, err)
	}
	if _, err := os.Stat(filepath.Join(outsideConfig, "go", "telemetry", "mode")); !os.IsNotExist(err) {
		t.Fatalf("Go telemetry mode escaped isolated probe HOME: %v", err)
	}
	entries, err := os.ReadDir(probeRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("fake Go left a probe HOME: entries=%v err=%v", entries, err)
	}
}

func TestEnvironmentToolVersionInFlightCancellationCleansProbe(t *testing.T) {
	s, root := environmentTestServer(t)
	probeRoot := t.TempDir()
	t.Setenv("TMPDIR", probeRoot)
	slowGo := filepath.Join(root, "slow-go")
	started := filepath.Join(root, "slow-go-started")
	script := "#!/bin/sh\nprintf 'started\\n' > '" + started + "'\nexec /bin/sleep 10\n"
	if err := os.WriteFile(slowGo, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slowGo, 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(60*time.Millisecond, cancel)
	start := time.Now()
	if _, err := s.environmentToolVersion(ctx, slowGo, "go"); err == nil {
		t.Fatal("in-flight cancelled probe unexpectedly completed")
	}
	if time.Since(start) > environmentProbeTimeout+time.Second {
		t.Fatal("cancelled version probe exceeded its execution budget")
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("cancellation test did not actually launch the slow subprocess: %v", err)
	}
	entries, err := os.ReadDir(probeRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled version probe left a disposable HOME: entries=%v err=%v", entries, err)
	}
}

// Simulate an OS cleanup error *after* safely removing the fixture. Only a
// fixed reason code may cross the repository_environment response boundary.
func TestRepositoryEnvironmentReportsProbeCleanupFailureWithoutLeak(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{
		"Makefile": "test:\n\t@echo test\n",
	})
	probeRoot := t.TempDir()
	t.Setenv("TMPDIR", probeRoot)
	cleanupErr := errors.New("private_cleanup_token=/secret/should_not_escape")
	s.environmentProbeRemoveAll = func(dir string) error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return cleanupErr
	}

	response := s.repositoryEnvironmentContext(context.Background(), Request{Path: repo})
	if !response.OK {
		t.Fatalf("environment discovery unexpectedly failed: %s", response.ReasonCode)
	}
	var summary RepositoryEnvironmentSummary
	if err := json.Unmarshal([]byte(response.Output), &summary); err != nil {
		t.Fatal(err)
	}
	tool, found := environmentToolByExecutable(summary, "make")
	if !found || tool.Source != "system_path" || tool.Compatibility != "unknown" ||
		tool.Reason != "version_probe_cleanup_failed" {
		t.Fatalf("cleanup-only failure not machine-readable in public summary: %+v", tool)
	}
	if strings.Contains(response.Output, "private_cleanup_token") ||
		strings.Contains(response.Output, "/secret/") {
		t.Fatal("sensitive cleanup error details escaped the response")
	}
	entries, err := os.ReadDir(probeRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("injected cleanup test left temporary directories: entries=%v err=%v", entries, err)
	}

	// The underlying cleanup error must remain available to internal callers.
	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	version, probeErr := s.environmentToolVersion(context.Background(), makeBinary, "make")
	var cleanupFailure *environmentProbeCleanupError
	if !errors.As(probeErr, &cleanupFailure) ||
		!errors.Is(probeErr, cleanupErr) ||
		cleanupFailure.probeErr != nil ||
		!strings.Contains(version, "GNU Make") {
		t.Fatalf("cleanup-only error wrapping lost the original outcome: version=%q err=%v", version, probeErr)
	}
}

// A canceled probe and failed cleanup are two separate, preserved errors.
// The public tool reason must signal BOTH without including raw error strings.
func TestRepositoryEnvironmentReportsCanceledProbeAndCleanupFailure(t *testing.T) {
	s, root := environmentTestServer(t)
	probeRoot := t.TempDir()
	t.Setenv("TMPDIR", probeRoot)
	cleanupErr := errors.New("private_cleanup_token=/secret/not_for_api")
	s.environmentProbeRemoveAll = func(dir string) error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return cleanupErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	_, resultErr := s.environmentToolVersion(ctx, makeBinary, "make")
	var combined *environmentProbeCleanupError
	if !errors.As(resultErr, &combined) ||
		!errors.Is(resultErr, context.Canceled) ||
		!errors.Is(resultErr, cleanupErr) ||
		combined.probeErr == nil {
		t.Fatalf("combined failure lost cancellation or cleanup cause: %v", resultErr)
	}
	builder := &environmentBuilder{
		server: s,
		root:   root,
		tools: map[string]*EnvironmentTool{
			"make": {Name: "Make", Executable: "make", Required: true},
		},
		mechanisms: map[string]*EnvironmentMechanism{},
	}
	builder.probeTools(ctx)
	tool := builder.tools["make"]
	if tool.Source != "system_path" || tool.Compatibility != "unknown" ||
		tool.Reason != "version_probe_and_cleanup_failed" {
		t.Fatalf("combined failure not machine-readable at tool boundary: %+v", tool)
	}
	publicData, err := json.Marshal(RepositoryEnvironmentSummary{Tools: []EnvironmentTool{*tool}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicData), "private_cleanup_token") ||
		strings.Contains(string(publicData), "/secret/") {
		t.Fatal("combined failure leaked sensitive error details")
	}
	entries, err := os.ReadDir(probeRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("combined failure left temporary directories: entries=%v err=%v", entries, err)
	}

	// Also exercise cancellation AFTER process launch, so a prematurely
	// failing shebang or permission error cannot masquerade as cancellation.
	slowMake := filepath.Join(root, "slow-make")
	started := filepath.Join(root, "slow-make-started")
	script := "#!/bin/sh\nprintf 'started\\n' > '" + started + "'\nexec /bin/sleep 10\n"
	if err := os.WriteFile(slowMake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slowMake, 0755); err != nil {
		t.Fatal(err)
	}
	liveCtx, stop := context.WithCancel(context.Background())
	defer stop()
	timer := time.AfterFunc(60*time.Millisecond, stop)
	_, liveErr := s.environmentToolVersion(liveCtx, slowMake, "make")
	timer.Stop()
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("combined failure did not start its worker subprocess: %v", err)
	}
	var liveFailure *environmentProbeCleanupError
	if !errors.As(liveErr, &liveFailure) || !errors.Is(liveErr, cleanupErr) ||
		liveCtx.Err() != context.Canceled || liveFailure.probeErr == nil ||
		environmentVersionProbeReason(liveErr) != "version_probe_and_cleanup_failed" {
		t.Fatalf("in-flight cancellation lost combined cleanup error classification: %v", liveErr)
	}
	entries, err = os.ReadDir(probeRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("in-flight combined error left a probe HOME: %v err=%v", entries, err)
	}
}

func TestRepositoryEnvironmentKeepsOrdinaryVersionFailureReason(t *testing.T) {
	s, root := environmentTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	builder := &environmentBuilder{
		server: s,
		root:   root,
		tools: map[string]*EnvironmentTool{
			"make": {Name: "Make", Executable: "make", Required: true},
		},
	}
	builder.probeTools(ctx)
	tool := builder.tools["make"]
	if tool.Source != "system_path" || tool.Compatibility != "unknown" ||
		tool.Reason != "version_probe_failed" {
		t.Fatalf("ordinary probe failure reason changed: %+v", tool)
	}
}

// Root-running Executors must stage the temporary mode file and directories
// so the separately credentialed worker can read them. This is an isolated
// root-gated test: it never changes real Go configuration or running services.
func TestEnvironmentToolVersionGoRootDelegatesTelemetryFreeProbe(t *testing.T) {
	if os.Geteuid() != 0 {
		if os.Getenv("ASA_ENV_ROOT_PROBE_REQUIRED") == "1" {
			t.Fatal("root-to-worker Go acceptance requires effective UID 0; SKIP is not allowed")
		}
		t.Skip("separate root-to-worker credential regression requires root")
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		if os.Getenv("ASA_ENV_ROOT_PROBE_REQUIRED") == "1" {
			t.Fatalf("real Go binary required for root-to-worker acceptance: %v", err)
		}
		t.Skip("Go binary not installed")
	}
	probeRoot, err := os.MkdirTemp("", "asa156-worker-credential-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(probeRoot); err != nil {
			t.Errorf("cleanup root worker probe fixture: %v", err)
		}
	})
	// /tmp is traversable; the synthetic worker must also traverse this
	// parent to reach the per-call HOME, which is individually chmod 0700.
	if err := os.Chmod(probeRoot, 0755); err != nil {
		t.Fatal(err)
	}
	s, _ := environmentTestServer(t)
	t.Setenv("TMPDIR", probeRoot)
	s.workerUID = 65534 // system unprivileged uid on supported Linux hosts
	s.workerGID = 65534
	const realGoRuns = 4
	// Explicitly prove that the version subprocess, not just the root test
	// harness, executes with the configured unprivileged worker identity.
	workerUIDScript := filepath.Join(probeRoot, "verify-worker-uid.sh")
	if err := os.WriteFile(workerUIDScript, []byte("#!/bin/sh\n/usr/bin/id -u\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Root test runners may use umask 077. Make this isolated worker
	// fixture explicitly executable after writing it.
	if err := os.Chmod(workerUIDScript, 0755); err != nil {
		t.Fatal(err)
	}
	childUID, err := s.environmentToolVersion(context.Background(), workerUIDScript, "go")
	if err != nil || childUID != "65534" {
		t.Fatalf("root-to-worker credential subprocess did not execute as 65534: uid=%q err=%v", childUID, err)
	}
	for i := 0; i < realGoRuns; i++ {
		version, err := s.environmentToolVersion(context.Background(), goBinary, "go")
		if err != nil || !strings.HasPrefix(version, "go version go") {
			t.Fatalf("root-to-worker Go probe %d failed: version=%q err=%v", i, version, err)
		}
	}
	time.Sleep(75 * time.Millisecond)
	entries, err := os.ReadDir(probeRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != "verify-worker-uid.sh" {
		t.Fatalf("root-to-worker Go probe left artifacts: %v err=%v", entries, err)
	}
	t.Logf("ROOT_GO_PROBE_ACCEPTANCE=PASS effective_uid=%d child_uid=%s real_go_probes=%d", os.Geteuid(), childUID, realGoRuns)
}
