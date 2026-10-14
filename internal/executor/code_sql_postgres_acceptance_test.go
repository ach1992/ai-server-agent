package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// PostgreSQL is tested as a real database engine ONLY when explicitly opted
// in by a human-authorized disposable test environment. code_* and the SQL
// LSP never connect to it, execute SQL, read credentials or use this fixture.
// This runs entirely as the unprivileged test user on a test-owned Unix
// socket with TCP disabled and no installation/system-service mutation.
func TestRealIsolatedPostgreSQLMigrations(t *testing.T) {
	if os.Getenv("AGENT_POSTGRES_DISPOSABLE_PROOF") != "1" {
		t.Skip("explicit isolated PostgreSQL acceptance is not enabled")
	}
	bin := os.Getenv("AGENT_POSTGRES_PROOF_BIN_DIR")
	lib := os.Getenv("AGENT_POSTGRES_PROOF_LIBRARY_DIR")
	if bin == "" || lib == "" {
		t.Fatal("pin both an isolated exact PostgreSQL binary and client library directory")
	}
	// Actual operator-provided test binaries stay outside the source worktree.
	for _, name := range []string{"initdb", "postgres", "psql"} {
		path := filepath.Join(bin, name)
		stat, err := os.Lstat(path)
		if err != nil || !stat.Mode().IsRegular() || stat.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("invalid PostgreSQL test fixture %s: %v", name, err)
		}
	}
	if os.Geteuid() == 0 {
		t.Fatal("PostgreSQL disposable acceptance must never run its server as root")
	}
	dir := t.TempDir()
	if len(dir) > 75 {
		t.Fatalf("test-only PostgreSQL socket path could exceed Unix limits: %q", dir)
	}
	data := filepath.Join(dir, "data")
	fixtureEnv := append(sanitizedCommandEnv(dir), "LD_LIBRARY_PATH="+lib, "LC_ALL=C")
	initCtx, cancelInit := context.WithTimeout(t.Context(), 18*time.Second)
	defer cancelInit()
	init := exec.CommandContext(initCtx, filepath.Join(bin, "initdb"),
		"-D", data, "--no-sync", "--no-locale", "--encoding=UTF8", "--auth=trust", "--username=agentproof")
	init.Dir = dir
	init.Env = fixtureEnv
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("private empty PostgreSQL initdb failed: %v %.2000s", err, output)
	}
	// Port is a UNIX socket *filename suffix* here: listen_addresses is empty.
	// No network listener, default DB discovery, user .psqlrc or system cluster.
	const socketPort = "55437"
	server := exec.Command(filepath.Join(bin, "postgres"),
		"-D", data, "-c", "listen_addresses=", "-c", "unix_socket_directories="+dir,
		"-c", "port="+socketPort, "-c", "fsync=off",
		"-c", "max_connections=7", "-c", "shared_buffers=16MB")
	server.Dir = dir
	server.Env = fixtureEnv
	server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	var logs bytes.Buffer
	server.Stdout = &logs
	server.Stderr = &logs
	if err := server.Start(); err != nil {
		t.Fatalf("start disposable PostgreSQL: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait(); close(done) }()
	defer func() {
		_ = server.Process.Signal(syscall.SIGINT) // fast PostgreSQL shutdown
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = server.Process.Kill()
			<-done
			t.Error("PostgreSQL fixture required forced stop (clean shutdown unverified)")
		}
	}()
	psql := func(query string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, "psql"), "-X", "-w", "-At",
			"-v", "ON_ERROR_STOP=1", "-h", dir, "-p", socketPort, "-U", "agentproof", "-d", "postgres")
		cmd.Dir = dir
		cmd.Env = fixtureEnv
		cmd.Stdin = strings.NewReader(query)
		body, err := cmd.CombinedOutput()
		if len(body) > 24<<10 {
			return "", fmt.Errorf("PostgreSQL acceptance output exceeded 24KiB")
		}
		return string(body), err
	}
	started := false
	for deadline := time.Now().Add(9 * time.Second); time.Now().Before(deadline); {
		output, err := psql("SELECT 1;")
		if err == nil && strings.TrimSpace(output) == "1" {
			started = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("temporary PostgreSQL exited before readiness: %v; %.1600s", err, logs.String())
		case <-time.After(110 * time.Millisecond):
		}
	}
	if !started {
		t.Fatalf("isolated PostgreSQL did not become ready: %.1600s", logs.String())
	}
	transport, err := psql("SHOW listen_addresses; SHOW unix_socket_directories; SHOW port;")
	// -At renders disabled TCP listen_addresses as an empty first line;
	// trimming whitespace leaves precisely the private socket and port.
	if err != nil || strings.TrimSpace(transport) != dir+"\n"+socketPort {
		t.Fatalf("PostgreSQL fixture network isolation not proven: %q %v", transport, err)
	}
	// PostgreSQL-specific GENERATED ALWAYS AS IDENTITY, JSONB, ::jsonb,
	// REFERENCES, CREATE INDEX and ALTER are executed on actual engine state.
	statements := postgresMigration001 + postgresMigration002 +
		`CREATE TABLE acceptance_entries (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 account_id BIGINT NOT NULL REFERENCES acceptance_accounts(id),
 body TEXT NOT NULL);
CREATE INDEX idx_acceptance_entry_account ON acceptance_entries(account_id);
INSERT INTO acceptance_accounts(label,payload)
 VALUES ('first','{"priority":3}'::jsonb);
INSERT INTO acceptance_entries(account_id,body) VALUES (1,'owned');
`
	if output, err := psql(statements); err != nil {
		t.Fatalf("genuine PostgreSQL versioned migrations failed: %v %.1700s", err, output)
	}
	result, err := psql(`SELECT enabled::int FROM acceptance_accounts;
SELECT payload->>'priority' FROM acceptance_accounts;
SELECT COUNT(*) FROM acceptance_entries;
SELECT COUNT(*) FROM pg_indexes WHERE indexname='idx_acceptance_entry_account';
SELECT COUNT(*) FROM pg_constraint WHERE contype='f' AND conrelid='acceptance_entries'::regclass;`)
	if err != nil || strings.TrimSpace(result) != "1\n3\n1\n1\n1" {
		t.Fatalf("Postgres actual identity/jsonb/index/FK/schema evidence inconsistent: %v %q", err, result)
	}
	t.Log("PostgreSQL 17.11 real disposable local-socket migrations PASS: identity, JSONB, ALTER, FK, index, data")
}
