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
	// ONLY is a legal unquoted MySQL/MariaDB object identifier, not the
	// PostgreSQL ALTER TABLE inheritance modifier. Verify this against a
	// real private MariaDB parser as well as our offline navigation tests.
	onlySQL := "USE acceptance_migration; " +
		"CREATE TABLE ONLY (id INT); " +
		"ALTER TABLE ONLY ADD COLUMN payload INT; " +
		"ALTER TABLE ONLY RENAME COLUMN payload TO next_payload; " +
		"ALTER TABLE ONLY RENAME TO renamed_only; " +
		"DROP TABLE renamed_only; " +
		"CREATE TABLE ONLY (id INT); DROP TABLE ONLY; " +
		"CREATE VIEW ONLY AS SELECT 1 AS id; DROP VIEW ONLY;"
	if out, err := client(onlySQL); err != nil {
		t.Fatalf("real MariaDB ONLY identifier DDL failed: %v; %s", err, out)
	}
	// MariaDB ALTER TABLE permits comma-separated alter_specification entries.
	// Prove both clause orders, using unquoted ONLY and an ordinary table,
	// against a real parser before offline source navigation claims support.
	compound := "USE acceptance_migration; " +
		"CREATE TABLE ONLY (id INT); " +
		"ALTER TABLE ONLY ADD COLUMN detail INT, RENAME TO archived_only_first; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_only_first' AND COLUMN_NAME='detail'; " +
		"DROP TABLE archived_only_first; " +
		"CREATE TABLE ONLY (id INT); " +
		"ALTER TABLE ONLY RENAME TO archived_only_second, ADD COLUMN detail INT; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_only_second' AND COLUMN_NAME='detail'; " +
		"DROP TABLE archived_only_second; " +
		"CREATE TABLE ordinary_jobs (id INT); " +
		"ALTER TABLE ordinary_jobs ADD COLUMN detail INT, RENAME TO ordinary_archived_first; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='ordinary_archived_first' AND COLUMN_NAME='detail'; " +
		"DROP TABLE ordinary_archived_first; " +
		"CREATE TABLE ordinary_jobs (id INT); " +
		"ALTER TABLE ordinary_jobs RENAME TO ordinary_archived_second, ADD COLUMN detail INT; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='ordinary_archived_second' AND COLUMN_NAME='detail'; " +
		"DROP TABLE ordinary_archived_second; " +
		"CREATE TABLE ordinary_jobs (id INT); " +
		"ALTER TABLE ordinary_jobs ADD COLUMN detail INT NOT NULL, RENAME TO ordinary_unmodeled; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='ordinary_unmodeled' AND COLUMN_NAME='detail'; " +
		"DROP TABLE ordinary_unmodeled; " +
		"CREATE TABLE ONLY (id INT); " +
		"ALTER TABLE ONLY RENAME archived_only_direct; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_only_direct' AND COLUMN_NAME='id'; " +
		"DROP TABLE archived_only_direct; " +
		"CREATE TABLE ONLY (id INT); " +
		"ALTER TABLE ONLY ADD COLUMN detail INT, RENAME archived_only_optional_first; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_only_optional_first' AND COLUMN_NAME='detail'; " +
		"DROP TABLE archived_only_optional_first; " +
		"CREATE TABLE ONLY (id INT); " +
		"ALTER TABLE ONLY RENAME archived_only_optional_second, ADD COLUMN detail INT; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_only_optional_second' AND COLUMN_NAME='detail'; " +
		"DROP TABLE archived_only_optional_second; " +
		"CREATE TABLE ordinary_jobs (id INT); " +
		"ALTER TABLE ordinary_jobs RENAME archived_ordinary_direct; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_ordinary_direct' AND COLUMN_NAME='id'; " +
		"DROP TABLE archived_ordinary_direct; " +
		"CREATE TABLE ordinary_jobs (id INT); " +
		"ALTER TABLE ordinary_jobs ADD COLUMN detail INT, RENAME archived_ordinary_optional_first; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_ordinary_optional_first' AND COLUMN_NAME='detail'; " +
		"DROP TABLE archived_ordinary_optional_first; " +
		"CREATE TABLE ordinary_jobs (id INT); " +
		"ALTER TABLE ordinary_jobs RENAME archived_ordinary_optional_second, ADD COLUMN detail INT; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='archived_ordinary_optional_second' AND COLUMN_NAME='detail'; " +
		"DROP TABLE archived_ordinary_optional_second;"
	compoundResult, err := client(compound)
	if err != nil || strings.TrimSpace(compoundResult) != "1\n1\n1\n1\n1\n1\n1\n1\n1\n1\n1" {
		t.Fatalf("real MariaDB compound ALTER / rename parity failed: output=%q err=%v", compoundResult, err)
	}

	// F5: reject reserved unquoted table rename targets and ADD/DROP column
	// names against the real private MariaDB parser, not just token slices.
	if out, err := client("USE acceptance_migration; CREATE TABLE keyword_probe (id INT);"); err != nil {
		t.Fatalf("real MariaDB keyword probe setup failed: %v; %s", err, out)
	}
	for _, invalid := range []string{
		"ALTER TABLE keyword_probe RENAME SELECT;",
		"ALTER TABLE keyword_probe ADD COLUMN SELECT INT, RENAME TO renamed_invalid;",
		"ALTER TABLE keyword_probe RENAME TO renamed_invalid, ADD COLUMN SELECT INT;",
		"ALTER TABLE keyword_probe DROP COLUMN FROM, RENAME TO renamed_invalid;",
		"RENAME TABLE keyword_probe TO SELECT;",
	} {
		if out, err := client("USE acceptance_migration; " + invalid); err == nil {
			t.Fatalf("real MariaDB accepted reserved bare identifier in %q: %s", invalid, out)
		}
	}
	body, err = client("USE acceptance_migration; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='keyword_probe'; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='renamed_invalid';")
	if err != nil || strings.TrimSpace(body) != "1\n0" {
		t.Fatalf("invalid reserved-name ALTER mutated table lifecycle: output=%q err=%v", body, err)
	}
	// Quoted reserved words, reserved words following a qualification dot,
	// and the ordinary nonreserved identifier ONLY must all remain legal.
	validKeywords := "USE acceptance_migration; " +
		"CREATE TABLE quoted_keyword_source (id INT); " +
		"ALTER TABLE quoted_keyword_source RENAME `SELECT`; " +
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='SELECT'; " +
		"DROP TABLE `SELECT`; " +
		"CREATE TABLE qualified_keyword_source (id INT); " +
		"ALTER TABLE qualified_keyword_source RENAME acceptance_migration.SELECT; " +
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='SELECT'; " +
		"DROP TABLE acceptance_migration.SELECT; " +
		"CREATE TABLE direct_keyword_source (id INT); " +
		"RENAME TABLE direct_keyword_source TO `SELECT`; " +
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='SELECT'; " +
		"DROP TABLE `SELECT`; " +
		"CREATE TABLE aux_keyword_source (id INT); " +
		"ALTER TABLE aux_keyword_source ADD COLUMN `SELECT` INT, RENAME TO aux_keyword_moved; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='aux_keyword_moved' AND COLUMN_NAME='SELECT'; " +
		"DROP TABLE aux_keyword_moved; " +
		"CREATE TABLE only_keyword_source (id INT); " +
		"ALTER TABLE only_keyword_source ADD COLUMN ONLY INT, RENAME TO only_keyword_moved; " +
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='only_keyword_moved' AND COLUMN_NAME='ONLY'; " +
		"DROP TABLE only_keyword_moved;"
	keywordResult, err := client(validKeywords)
	if err != nil || strings.TrimSpace(keywordResult) != "1\n1\n1\n1\n1" {
		t.Fatalf("real MariaDB rejected legal qualified/quoted/nonreserved names: output=%q err=%v", keywordResult, err)
	}

	// MariaDB F5-R1/R2/R3: the official default-mode grammar distinguishes
	// unquoted DATABASE/WINDOW from version-reserved identifiers and handles
	// unquoted SYSTEM specially in ALTER ADD. Exercise a fresh socket-only
	// MariaDB engine, not a production database or an offline-only simulation.
	for _, invalid := range []string{
		"ALTER TABLE keyword_probe RENAME TO OFFSET;",
		"ALTER TABLE keyword_probe RENAME TO ROW_NUMBER;",
		"ALTER TABLE keyword_probe RENAME TO SQL_AFTER_GTIDS;",
		"ALTER TABLE keyword_probe RENAME TO SQL_BEFORE_GTIDS;",
		"ALTER TABLE keyword_probe ADD COLUMN OFFSET INT, RENAME TO invalid_keyword_move;",
		"ALTER TABLE keyword_probe ADD SYSTEM VARCHAR(64), RENAME TO invalid_keyword_move;",
		"ALTER TABLE keyword_probe RENAME TO invalid_keyword_move, ADD SYSTEM VARCHAR(64);",
		"ALTER TABLE keyword_probe DROP SYSTEM, RENAME TO invalid_keyword_move;",
		"ALTER TABLE keyword_probe RENAME TO invalid_keyword_move, DROP SYSTEM;",
		"ALTER TABLE keyword_probe DROP SYSTEM RESTRICT, RENAME TO invalid_keyword_move;",
		"RENAME TABLE keyword_probe TO ROW_NUMBER;",
		"RENAME TABLE keyword_probe TO invalid_keyword_move, extra TO OFFSET;",
	} {
		if out, err := client("USE acceptance_migration; " + invalid); err == nil {
			t.Fatalf("real MariaDB accepted an invalid reserved/contextual name %q: %s", invalid, out)
		}
	}
	for _, valid := range []string{
		"CREATE TABLE DATABASE (id INT); DROP TABLE DATABASE;",
		"CREATE TABLE WINDOW (id INT); DROP TABLE WINDOW;",
		"CREATE TABLE has_system_column (system INT); DROP TABLE has_system_column;",
		"ALTER TABLE keyword_probe ADD COLUMN DATABASE INT;",
		"ALTER TABLE keyword_probe ADD COLUMN WINDOW INT;",
		"ALTER TABLE keyword_probe ADD COLUMN `SYSTEM` VARCHAR(64);",
		"CREATE TABLE explicit_system_probe (id INT); ALTER TABLE explicit_system_probe ADD COLUMN system VARCHAR(64), RENAME TO explicit_system_moved; DROP TABLE explicit_system_moved;",
		"CREATE TABLE quoted_system_probe (id INT); ALTER TABLE quoted_system_probe ADD `system` VARCHAR(64), RENAME TO quoted_system_moved; DROP TABLE quoted_system_moved;",
		"CREATE TABLE explicit_drop_system_probe (id INT, system INT); ALTER TABLE explicit_drop_system_probe DROP COLUMN system, RENAME TO explicit_drop_system_moved; DROP TABLE explicit_drop_system_moved;",
		"CREATE TABLE quoted_drop_system_probe (id INT, `system` INT); ALTER TABLE quoted_drop_system_probe DROP `system`, RENAME TO quoted_drop_system_moved; DROP TABLE quoted_drop_system_moved;",
		"CREATE TABLE reverse_explicit_drop_system_probe (id INT, system INT); ALTER TABLE reverse_explicit_drop_system_probe RENAME TO reverse_explicit_drop_system_moved, DROP COLUMN system; DROP TABLE reverse_explicit_drop_system_moved;",
		"CREATE TABLE reverse_quoted_drop_system_probe (id INT, `system` INT); ALTER TABLE reverse_quoted_drop_system_probe RENAME TO reverse_quoted_drop_system_moved, DROP `system`; DROP TABLE reverse_quoted_drop_system_moved;",
		"CREATE TABLE legal_db_rename (id INT); ALTER TABLE legal_db_rename RENAME TO DATABASE; DROP TABLE DATABASE;",
		"CREATE TABLE legal_window_rename (id INT); ALTER TABLE legal_window_rename RENAME TO WINDOW; DROP TABLE WINDOW;",
		"CREATE TABLE qualified_reserved (id INT); RENAME TABLE qualified_reserved TO acceptance_migration.ROW_NUMBER; DROP TABLE acceptance_migration.ROW_NUMBER;",
	} {
		if out, err := client("USE acceptance_migration; " + valid); err != nil {
			t.Fatalf("real MariaDB rejected a legal unquoted/quoted/qualified name %q: %s (%v)", valid, out, err)
		}
	}
	if out, err := client("USE acceptance_migration; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='keyword_probe'; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='invalid_keyword_move';"); err != nil || strings.TrimSpace(out) != "1\n0" {
		t.Fatalf("MariaDB reserved/contextual probes changed table lifecycle: output=%q err=%v", out, err)
	}

	// Invalid type parameters must not be confused with successful DDL.
	if out, err := client("USE acceptance_migration; CREATE TABLE invalid_decimal_probe (id INT);"); err != nil {
		t.Fatalf("unable to create disposable precision probe: %v; %s", err, out)
	}
	// This MariaDB version accepts DECIMAL(0,0), unlike MySQL 8.4's
	// documented precision minimum. The source-only common subset still
	// declines to certify it; do not claim the engine rejected it.
	for _, invalid := range []string{
		"DECIMAL(5,6)",
		"DECIMAL(66,1)",
	} {
		ddl := "USE acceptance_migration; ALTER TABLE invalid_decimal_probe ADD COLUMN amount " + invalid + ", RENAME invalid_moved;"
		if out, err := client(ddl); err == nil {
			t.Fatalf("real MariaDB unexpectedly accepted invalid decimal parameters %s: %s", invalid, out)
		}
	}
	if out, err := client("USE acceptance_migration; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='acceptance_migration' AND TABLE_NAME='invalid_decimal_probe';"); err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("invalid DECIMAL attempt incorrectly moved disposable source: %q (%v)", out, err)
	}
	t.Log("real private MariaDB grammar acceptance: TO and optional-TO rename, both clause orders, ONLY and ordinary identifiers, invalid DECIMAL rejected")
}
