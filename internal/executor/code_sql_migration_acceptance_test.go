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

// These migrations have deliberately engine-specific syntax. The SQL LSP
// remains offline; only this explicitly enabled disposable acceptance test
// runs SQL against a newly initialized MariaDB instance with no TCP listener.
const (
	mysqlMigration001 = `CREATE TABLE acceptance_accounts (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  label VARCHAR(100) NOT NULL UNIQUE,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB;
`
	mysqlMigration002 = `ALTER TABLE acceptance_accounts
  ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;
CREATE TABLE acceptance_entries (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  account_id BIGINT UNSIGNED NOT NULL,
  body TEXT NOT NULL,
  CONSTRAINT fk_entries_account FOREIGN KEY (account_id)
    REFERENCES acceptance_accounts (id)
) ENGINE=InnoDB;
CREATE INDEX idx_entries_account ON acceptance_entries (account_id);
`
	postgresMigration001 = `CREATE TABLE acceptance_accounts (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  label TEXT NOT NULL UNIQUE,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb
);
`
	postgresMigration002 = `ALTER TABLE acceptance_accounts ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;
CREATE INDEX idx_acceptance_enabled ON acceptance_accounts (enabled);
`
)

func TestRealIsolatedMariaDBMigration(t *testing.T) {
	if os.Getenv("AGENT_MARIADB_DISPOSABLE_PROOF") != "1" {
		t.Skip("requires explicit disposable MariaDB acceptance opt-in")
	}
	for _, path := range []string{"/usr/bin/mariadb-install-db", "/usr/sbin/mariadbd", "/usr/bin/mariadb"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("MariaDB executable unavailable: %s: %v", path, err)
		}
	}
	dir := t.TempDir() // test-owned private directory; not an installed DB path
	data, socket, pidPath, logPath := filepath.Join(dir, "data"), filepath.Join(dir, "m.sock"), filepath.Join(dir, "m.pid"), filepath.Join(dir, "m.log")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	initCtx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	init := exec.CommandContext(initCtx, "/usr/bin/mariadb-install-db",
		"--no-defaults", "--datadir="+data, "--skip-test-db", "--auth-root-authentication-method=normal")
	init.Dir = dir
	init.Env = sanitizedCommandEnv(dir)
	out, err := init.CombinedOutput()
	if err != nil {
		t.Fatalf("disposable MariaDB initialization failed: %v: %s", err, string(out))
	}
	server := exec.Command("/usr/sbin/mariadbd", "--no-defaults",
		"--datadir="+data, "--socket="+socket, "--pid-file="+pidPath,
		"--log-error="+logPath, "--skip-networking", "--max-connections=5")
	server.Dir = dir
	server.Env = sanitizedCommandEnv(dir)
	server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	var output bytes.Buffer
	server.Stdout = &output
	server.Stderr = &output
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	defer func() {
		_ = server.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			_ = server.Process.Kill()
			<-done
		}
	}()
	client := func(query string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
		defer cancel()
		cli := exec.CommandContext(ctx, "/usr/bin/mariadb", "--no-defaults", "--protocol=socket",
			"--socket="+socket, "--user=root", "--batch", "--skip-column-names")
		cli.Stdin = strings.NewReader(query)
		cli.Env = sanitizedCommandEnv(dir)
		body, err := cli.CombinedOutput()
		if len(body) > 32<<10 {
			return "", fmt.Errorf("unbounded MariaDB client output")
		}
		return string(body), err
	}
	var ready bool
	for deadline := time.Now().Add(9 * time.Second); time.Now().Before(deadline); {
		body, err := client("SELECT 1;")
		if err == nil && strings.TrimSpace(body) == "1" {
			ready = true
			break
		}
		select {
		case exit := <-done:
			t.Fatalf("isolated server exited: %v; stdout=%s", exit, output.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("disposable MariaDB did not start: %s", output.String())
	}
	setup := "CREATE DATABASE acceptance_migration; USE acceptance_migration;\n" +
		mysqlMigration001 + mysqlMigration002 +
		"INSERT INTO acceptance_accounts (label) VALUES ('first');\n" +
		"INSERT INTO acceptance_entries (account_id,body) VALUES (1,'owned');\n"
	if out, err := client(setup); err != nil {
		t.Fatalf("real migrations failed: %v; %s", err, out)
	}
	body, err := client("USE acceptance_migration; SELECT enabled FROM acceptance_accounts; SELECT COUNT(*) FROM acceptance_entries;")
	if err != nil || strings.TrimSpace(body) != "1\n1" {
		t.Fatalf("migrated schema/data incorrect: result=%q err=%v", body, err)
	}
	schema, err := client("USE acceptance_migration; SHOW CREATE TABLE acceptance_entries;")
	if err != nil || !strings.Contains(schema, "fk_entries_account") || !strings.Contains(schema, "idx_entries_account") {
		t.Fatalf("foreign key/index missing in real MariaDB: %s (%v)", schema, err)
	}
	t.Log("real MariaDB migrations, FK, index, data verification passed on fresh private --skip-networking socket-only instance")
}
