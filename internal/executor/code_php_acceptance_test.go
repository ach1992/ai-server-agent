package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPHPActorProfileRejectsComposerExecutionAndProjectConfig(t *testing.T) {
	profile := phpLSPProfile("/trusted/phpactor.phar", "/worker-home/.cache/phpactor/index/private")
	if profile.languageForPath("src/Controller.php") != "php" ||
		!profile.pushOnlyDiagnostics || !profile.syntheticChangeForDiagnostics {
		t.Fatalf("PHP syntax/profile not selected for passive LSP: %+v", profile)
	}
	args := strings.Join(profile.arguments, " ")
	for _, safe := range []string{"-n", "--no-interaction", "--no-ansi",
		"/trusted/phpactor.phar", "core.trusted", "core.project_config_candidates",
		"composer.enable", "indexer.enabled_watchers", "indexer.follow_symlinks",
		"indexer.index_path"} {
		if !strings.Contains(args, safe) {
			t.Fatalf("safe PHPActor flag %q missing: %s", safe, args)
		}
	}
	for _, unsafe := range []string{"--trust", "composer install", "vendor/autoload.php"} {
		if strings.Contains(args, unsafe) {
			t.Fatalf("PHPActor profile would run project commands: %q", unsafe)
		}
	}
	s, req, _ := testStdioBroker(t)
	req.Action, req.CodeMethod, req.Language, req.Path, req.FileVersion =
		"code_inspect", "symbols", "php", "src/Consumer.php", "v1:some"
	req.Root = true
	if x := s.codeInspect(context.Background(), req); x.OK || x.ErrorCode != "code_requires_worker_version" {
		t.Fatalf("PHP source analysis acquired root authority: %+v", x)
	}
}

func TestCodeInspectRealPHPActorSafe(t *testing.T) {
	binary, archive := os.Getenv("AGENT_PHP_LSP_PROOF_BIN"), os.Getenv("AGENT_PHP_LSP_PROOF_PHAR")
	if binary == "" && archive == "" {
		t.Skip("real PHP LSP proof requires AGENT_PHP_LSP_PROOF_BIN and AGENT_PHP_LSP_PROOF_PHAR")
	}
	if binary == "" || archive == "" {
		t.Fatal("partial PHP LSP fixture is not acceptance")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.phpBinary = binary
	s.phpActorPHAR = archive
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-php-workspace-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("worker file helper unavailable: %v %s", err, output)
	}
	s.workspaceHelperBinary = helper
	for _, dir := range []string{"src", "vendor",
		"app/Http/Controllers", "wp-content/plugins/agent-proof"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	greeter := "<?php\nnamespace App;\nclass Greeter {\n  public function greet(string $name): string { return 'Hello ' . $name; }\n}\n"
	consumer := "<?php\nnamespace App;\nuse App\\Greeter;\n$greeter = new Greeter();\necho $greeter->greet('ACh');\n"
	files := map[string]string{
		"src/Greeter.php":     greeter,
		"consumer.php":        consumer,
		"composer.json":       `{"name":"acceptance/phpactor","autoload":{"psr-4":{"App\\":"src/"}},"scripts":{"post-autoload-dump":"touch PHP-COMPOSER-MUST-NOT-EXECUTE"}}`,
		"vendor/autoload.php": "<?php file_put_contents('PHP-AUTOLOAD-MUST-NOT-EXECUTE','unsafe');",
		".phpactor.yml":       "composer.enable: true\nindexer.follow_symlinks: true\n",
		"wp-config.php":       "<?php if (isset($GLOBALS['do_not_execute'])) {file_put_contents('PHP-WORDPRESS-MUST-NOT-EXECUTE','bad');}\n",
		"app/Http/Controllers/HomeController.php":   "<?php\nnamespace App\\Http\\Controllers;\nclass HomeController { public function index(): string { return 'Laravel style'; } }\n",
		"wp-content/plugins/agent-proof/plugin.php": "<?php\nnamespace AgentProofPlugin;\nclass RegisterHooks { public static function initialize(): void {} }\n",
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(path string) Response {
		t.Helper()
		r := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: path})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("PHP worker source metadata absent: %+v", r)
		}
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 105*time.Second)
	defer cancel()
	version := stat("src/Greeter.php")
	owner.Action, owner.Path, owner.Language, owner.FileVersion =
		"code_inspect", "src/Greeter.php", "php", version.FileVersion
	run := func(method string, line, char int) Response {
		t.Helper()
		owner.CodeMethod, owner.Line, owner.Character = method, line, char
		r := s.codeInspect(ctx, owner)
		if !r.OK || r.FileVersion != owner.FileVersion {
			t.Fatalf("real PHP %s failed: %+v", method, r)
		}
		return r
	}
	if r := run("symbols", 0, 0); !strings.Contains(r.Output, "Greeter") || !strings.Contains(r.Output, "greet") {
		t.Fatalf("Phpactor structural symbols unproven: %s", r.Output)
	}
	if r := run("definition", 3, 20); !strings.Contains(r.Output, `"uri":"src/Greeter.php"`) ||
		!strings.Contains(r.Output, version.FileVersion) {
		t.Fatalf("Phpactor PHP method definition not file-version pinned: %s", r.Output)
	}
	if r := run("references", 3, 20); !strings.Contains(r.Output, "file_version") {
		t.Fatalf("Phpactor symbol references not proven: %s", r.Output)
	}
	// Representative WordPress-plugin and Laravel-controller PHP syntax is
	// independently parsed without evaluating files or enabling Composer.
	for _, tc := range []struct{ path, symbol string }{
		{"app/Http/Controllers/HomeController.php", "HomeController"},
		{"wp-content/plugins/agent-proof/plugin.php", "RegisterHooks"},
	} {
		owner.Path, owner.FileVersion = tc.path, stat(tc.path).FileVersion
		if r := run("symbols", 0, 0); !strings.Contains(r.Output, tc.symbol) {
			t.Fatalf("%s project-shaped PHP symbols missing: %s", tc.path, r.Output)
		}
	}
	owner.Path, owner.FileVersion = "src/Greeter.php", version.FileVersion
	invalid := "<?php\nnamespace App;\nclass Broken { public function f( { }\n"
	if err := os.WriteFile(filepath.Join(workspace, "src/Greeter.php"), []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = stat("src/Greeter.php").FileVersion
	if r := run("diagnostics", 2, 0); !strings.Contains(r.Output, "Parse error") &&
		!strings.Contains(r.Output, "syntax error") {
		t.Fatalf("Phpactor syntactic diagnostics never positively observed: %s", r.Output)
	}
	owner.FileVersion = version.FileVersion
	if r := s.codeInspect(ctx, owner); r.OK || r.ErrorCode != "file_changed" {
		t.Fatalf("PHP stale source accepted: %+v", r)
	}
	for _, marker := range []string{
		"PHP-COMPOSER-MUST-NOT-EXECUTE", "PHP-AUTOLOAD-MUST-NOT-EXECUTE",
		"PHP-WORDPRESS-MUST-NOT-EXECUTE",
	} {
		if _, err := os.Stat(filepath.Join(workspace, marker)); !os.IsNotExist(err) {
			t.Fatalf("read-only PHP analysis executed untrusted project source %q: %v", marker, err)
		}
	}
	s.sessions.mu.Lock()
	n := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("PHP LSP session leaked: %d", n)
	}
}
