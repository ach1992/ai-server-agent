package executor

import (
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
