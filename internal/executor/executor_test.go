package executor

import (
	"os"
	"path/filepath"
	"testing"

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
