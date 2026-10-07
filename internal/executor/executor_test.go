package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

func TestCommandCredentials(t *testing.T) {
	s := &Server{
		cfg:       config.Config{WorkspaceDir: t.TempDir()},
		guard:     policy.New(nil),
		workerUID: 1234,
		workerGID: 1235,
	}

	rootCmd, _ := s.command(Request{Command: "id", Root: true})
	if rootCmd.SysProcAttr == nil || rootCmd.SysProcAttr.Credential == nil {
		t.Fatal("root command must set explicit credentials")
	}
	rootCred := rootCmd.SysProcAttr.Credential
	if rootCred.Uid != 0 || rootCred.Gid != 0 || len(rootCred.Groups) != 1 || rootCred.Groups[0] != 0 {
		t.Fatalf("root credentials = uid:%d gid:%d groups:%v, want uid:0 gid:0 groups:[0]", rootCred.Uid, rootCred.Gid, rootCred.Groups)
	}

	workerCmd, _ := s.command(Request{Command: "id"})
	if workerCmd.SysProcAttr == nil || workerCmd.SysProcAttr.Credential == nil {
		t.Fatal("worker command must set explicit credentials")
	}
	workerCred := workerCmd.SysProcAttr.Credential
	if workerCred.Uid != 1234 || workerCred.Gid != 1235 || len(workerCred.Groups) != 1 || workerCred.Groups[0] != 1235 {
		t.Fatalf("worker credentials = uid:%d gid:%d groups:%v, want uid:1234 gid:1235 groups:[1235]", workerCred.Uid, workerCred.Gid, workerCred.Groups)
	}
}

func TestTrustedDirRejectsWritableAndSymlink(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "trusted")
	if err := os.Mkdir(trusted, 0711); err != nil {
		t.Fatal(err)
	}
	if err := trustedDir(trusted); err != nil {
		t.Fatalf("trustedDir rejected executor-owned 0711 dir: %v", err)
	}
	if err := os.Chmod(trusted, 0733); err != nil {
		t.Fatal(err)
	}
	if err := trustedDir(trusted); err == nil {
		t.Fatal("trustedDir accepted group/other-writable directory")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}
	if err := trustedDir(link); err == nil {
		t.Fatal("trustedDir accepted symlink")
	}
}

func TestCreateJobFileRejectsExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "job.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if err := createJobFile(link, uid, gid); err == nil {
		t.Fatal("createJobFile followed or replaced an existing symlink")
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sentinel" {
		t.Fatalf("symlink target changed: %q", string(b))
	}
}

func TestOpenJobFileRejectsLegacySymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("root-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "123.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s := &Server{workerUID: uint32(os.Geteuid())}
	if f, err := s.openJobFile(link); err == nil {
		_ = f.Close()
		t.Fatal("openJobFile followed a legacy symlink")
	}
}

func TestCommandUsesSanitizedRootEnvironment(t *testing.T) {
	workspace := t.TempDir()
	s := &Server{
		cfg:       config.Config{WorkspaceDir: workspace, WorkerUser: "aiworker"},
		guard:     policy.New(nil),
		workerUID: 1234,
		workerGID: 1235,
	}
	rootCmd, _ := s.command(Request{Command: "printf safe", Root: true})
	if got := strings.Join(rootCmd.Args, " "); strings.Contains(got, " -l") || !strings.Contains(got, "--noprofile --norc -s") || strings.Contains(got, "printf safe") {
		t.Fatalf("root shell args are not startup-file-safe: %q", got)
	}
	env := strings.Join(rootCmd.Env, "\n")
	if !strings.Contains(env, "HOME=/root") || strings.Contains(env, "BASH_ENV=") || strings.Contains(env, "ENV=") {
		t.Fatalf("root environment is not sanitized: %q", env)
	}
	if strings.Contains(env, "HOME="+workspace) {
		t.Fatalf("root command inherited worker HOME: %q", env)
	}
	if rootCmd.Dir != "/root" {
		t.Fatalf("root command working directory = %q, want /root", rootCmd.Dir)
	}
	workerCmd, _ := s.command(Request{Command: "printf safe", Root: false})
	if workerCmd.Dir != workspace {
		t.Fatalf("worker command working directory = %q, want workspace", workerCmd.Dir)
	}
}

func TestShellCommandIgnoresWorkerStartupFiles(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "startup-ran")
	payload := []byte("printf pwned > " + marker + "\n")
	for _, name := range []string{".bash_profile", ".bash_login", ".profile", ".bashrc"} {
		if err := os.WriteFile(filepath.Join(home, name), payload, 0644); err != nil {
			t.Fatal(err)
		}
	}
	bashEnv := filepath.Join(home, "bash-env")
	if err := os.WriteFile(bashEnv, payload, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", bashEnv)
	t.Setenv("ENV", bashEnv)
	cmd := newShellCommand("printf safe", home, workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sanitized shell failed: %v (%s)", err, out)
	}
	if string(out) != "safe" {
		t.Fatalf("unexpected shell output: %q", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker-controlled startup state executed; stat err=%v", err)
	}
}

func TestRootCommandIgnoresWorkerStartupFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("privileged execution behavior is exercised under sudo in High Assurance Security")
	}
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "worker-startup-ran")
	payload := []byte("printf pwned > " + marker + "\n")
	for _, name := range []string{".bash_profile", ".bash_login", ".profile", ".bashrc"} {
		if err := os.WriteFile(filepath.Join(workspace, name), payload, 0644); err != nil {
			t.Fatal(err)
		}
	}
	bashEnv := filepath.Join(workspace, "bash-env")
	if err := os.WriteFile(bashEnv, payload, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", bashEnv)
	t.Setenv("ENV", bashEnv)
	ambientName := ".ai-worker-ambient-" + filepath.Base(workspace)
	if err := os.WriteFile(filepath.Join(workspace, ambientName), []byte("worker-controlled"), 0644); err != nil {
		t.Fatal(err)
	}
	benignRootCommand := "test ! -e " + shellQuote(ambientName) + " && printf safe"

	s := &Server{
		cfg: config.Config{WorkspaceDir: workspace, WorkerUser: "root"},
		guard:     policy.New(nil),
		audit:     audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workerUID: 0,
		workerGID: 0,
		runs:      newRunLimiter(),
	}
	resp := s.run(Request{Command: benignRootCommand, Root: true, Approval: true})
	if !resp.OK || resp.Output != "safe" {
		t.Fatalf("root command failed: %+v", resp)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("root command executed worker startup state; stat err=%v", err)
	}
}

func TestRootJobInvocationSanitizesEnvironment(t *testing.T) {
	source, err := os.ReadFile("jobs.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, want := range []string{"/usr/bin/env", `"-i"`, `"HOME="+home`, `workDir = "/root"`, `"--property=WorkingDirectory=" + workDir`, `runnerPath, "job-runner"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("startJob is missing sanitized invocation contract %q", want)
		}
	}
	if strings.Contains(text, `"/bin/bash", "--noprofile", "--norc", "-c"`) || strings.Contains(text, `--setenv=HOME=`) {
		t.Fatal("startJob still contains raw-command argv or inherited-HOME behavior")
	}
}

func TestNewServerRejectsEmptyTokenBeforeWorkerLookup(t *testing.T) {
	cfg := config.Default()
	if _, err := NewServer(cfg, " \n\t"); err == nil || !strings.Contains(err.Error(), "executor token is empty") {
		t.Fatalf("NewServer() error = %v, want empty-token rejection", err)
	}
}
