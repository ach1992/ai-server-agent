package executor

import "testing"

// The physical source graph is a statement-level claim, never permission to
// publish a plausible table/column discovered in a malformed SQL prefix.
func TestSQLReviewEmissionGateNegatives(t *testing.T) {
	cases := []struct{ dialect, sql string }{
		{"postgres", "SELECT a.id FROM accounts a WHERE a.id = 1 'ignored';"},
		{"postgres", "SELECT a.id FROM accounts a HAVING a.id = 1 'ignored';"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, PRIMARY KEY (id) 'ignored');"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, UNIQUE (id) 'ignored');"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, FOREIGN KEY (id) REFERENCES owners(id) 'ignored');"},
		{"postgres", "CREATE INDEX ix ON accounts (id) WHERE id=1 'ignored';"},
		{"postgres", "CREATE VIEW phantom AS SELECT +;"},
		{"postgres", "CREATE VIEW phantom AS SELECT *;"},
		{"postgres", "CREATE MATERIALIZED VIEW phantom AS SELECT +;"},
		{"postgres", "CREATE TABLE phantom AS SELECT +;"},
		{"postgres", "INSERT INTO accounts (id) FROBNICATE;"},
		{"postgres", "UPDATE accounts FROBNICATE;"},
		{"postgres", "DELETE FROM accounts FROBNICATE;"},
		{"postgres", "TRUNCATE TABLE accounts FROBNICATE;"},
		{"postgres", "INSERT INTO accounts (id) VALUES (1) FROBNICATE;"},
		{"postgres", "UPDATE accounts SET id=1 FROBNICATE;"},
		{"postgres", "DELETE FROM accounts WHERE id=1 FROBNICATE;"},
		{"postgres", "CREATE TABLE accounts (id NUMERIC 'ignored'(10));"},
		{"postgres", "CREATE TABLE accounts (id INT REFERENCES owners 'ignored'(id));"},
		{"postgres", "CREATE TABLE accounts (id INT CHECK 'ignored'(id=1));"},
		{"postgres", "CREATE TABLE accounts (id WITH);"},
		{"postgres", "CREATE TABLE accounts (id ONLY);"},
		{"postgres", "CREATE INDEX ONLY ON accounts (id);"},
		{"postgres", "CREATE INDEX SELECT ON accounts (id);"},
		{"postgres", "CREATE INDEX ix.extra ON accounts (id);"},
		{"postgres", "ALTER TABLE accounts RENAME INDEX old TO new;"},
		{"postgres", "ALTER TABLE accounts RENAME KEY old TO new;"},
		{"mysql", "CREATE INDEX ix.extra ON accounts (id);"},
		{"mysql", "UPDATE accounts FROBNICATE;"},
		{"mariadb", "DELETE FROM accounts FROBNICATE;"},
		{"postgres", "CREATE VIEW IF NOT EXISTS bogus AS SELECT 1;"},
		{"mysql", "CREATE VIEW IF NOT EXISTS bogus AS SELECT 1;"},
		{"mariadb", "CREATE OR REPLACE VIEW IF NOT EXISTS bogus AS SELECT 1;"},
		// A complete structural grammar must own every removed literal.
		{"mysql", "CREATE TABLE child 'discarded' LIKE parent;"},
		{"mariadb", "CREATE TABLE child 'discarded' LIKE parent;"},
		{"mysql", "CREATE TABLE child (id INT) ENGINE='discarded'InnoDB;"},
		{"mariadb", "CREATE TABLE child (id INT) ENGINE='discarded'InnoDB;"},
		{"mysql", "CREATE TABLE child (id INT) 'discarded' ENGINE=InnoDB;"},
		{"mariadb", "CREATE TABLE child (id INT) 'discarded' ENGINE=InnoDB;"},
		{"postgres", "CREATE VIEW phantom AS SELECT 1 'discarded' AS id;"},
		{"postgres", "CREATE MATERIALIZED VIEW phantom AS SELECT 1 'discarded' AS id WITH NO DATA;"},
		{"postgres", "ALTER MATERIALIZED 'discarded' VIEW reporting.daily RENAME TO renamed;"},
		{"postgres", "ALTER MATERIALIZED VIEW 'discarded' reporting.daily RENAME TO renamed;"},
	}
	for _, tc := range cases {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var events []sqlNavOccurrence
				var ok bool
				if all {
					events, ok = sqlNavAllOccurrences(tc.sql, tc.dialect)
				} else {
					events, ok = sqlNavOccurrences(tc.sql, tc.dialect)
				}
				if ok && len(events) > 0 {
					t.Fatalf("unproven SQL published physical graph all=%v: %+v", all, events)
				}
			}
		})
	}
}
func TestSQLReviewEmissionGatePositives(t *testing.T) {
	cases := []struct{ dialect, sql, want string }{
		{"postgres", "CREATE TABLE accounts (id INT DEFAULT 'ready');", "accounts"},
		{"postgres", "SELECT 'standalone', a.id FROM accounts a WHERE a.id=1;", "accounts"},
		{"postgres", "CREATE VIEW v AS SELECT 1 AS id;", "v"},
		{"postgres", "CREATE VIEW v AS SELECT TRUE AS enabled;", "v"},
		{"postgres", "CREATE TABLE v AS SELECT NULL AS item;", "v"},
		{"postgres", "CREATE MATERIALIZED VIEW v AS SELECT 1 WITH NO DATA;", "v"},
		{"postgres", "INSERT INTO accounts (id,label) VALUES (1,'owned');", "accounts"},
		{"postgres", "INSERT INTO accounts (id) SELECT a.id FROM accounts a;", "accounts"},
		{"postgres", "UPDATE accounts SET id=1;", "accounts"},
		{"postgres", "DELETE FROM accounts WHERE id=1;", "accounts"},
		{"postgres", "TRUNCATE TABLE accounts;", "accounts"},
		{"postgres", "CREATE TABLE accounts (id NUMERIC(10,2) CHECK(id>=0));", "accounts"},
		{"postgres", "CREATE TABLE accounts (id INT REFERENCES owners(id));", "accounts"},
		{"postgres", "CREATE TABLE accounts (id integer);", "accounts"},
		{"postgres", "CREATE TABLE accounts (id public.mytype);", "accounts"},
		{"postgres", "CREATE TABLE accounts (id \"UserType\");", "accounts"},
		{"postgres", "CREATE TABLE accounts (id \"only\");", "accounts"},
		{"postgres", "CREATE TABLE accounts (id AUTHORIZATION);", "accounts"},
		{"postgres", "CREATE INDEX ix ON accounts (id);", "accounts"},
		{"postgresql", "CREATE INDEX ix ON accounts (id);", "accounts"},
		{"postgresql", "CREATE INDEX CONCURRENTLY ix ON ONLY accounts USING BTREE (id);", "accounts"},
		{"postgres", "CREATE INDEX \"only\" ON accounts (id);", "accounts"},
		{"mysql", "CREATE TABLE child LIKE parent;", "child"},
		{"mariadb", "CREATE TABLE child LIKE parent;", "child"},
		{"mysql", "CREATE TABLE child (id INT) ENGINE=InnoDB;", "child"},
		{"mariadb", "CREATE TABLE child (id INT) ENGINE=InnoDB;", "child"},
		{"postgres", "CREATE VIEW phantom AS SELECT 1 AS id;", "phantom"},
		{"postgres", "CREATE MATERIALIZED VIEW phantom AS SELECT 1 AS id WITH NO DATA;", "phantom"},
		{"postgres", "CREATE MATERIALIZED VIEW phantom AS SELECT 1 AS id WITH DATA;", "phantom"},
		{"postgres", `CREATE TABLE "child" ("id" INT DEFAULT 'ok');`, "child"},
		{"mysql", "CREATE TABLE accounts (id INT);", "accounts"},
		{"mariadb", "CREATE VIEW IF NOT EXISTS accounts AS SELECT 1;", "accounts"},
		{"mysql", "CREATE INDEX ONLY ON accounts (id);", "accounts"},
		{"mariadb", "CREATE INDEX ONLY ON accounts (id);", "accounts"},
		{"mysql", "ALTER TABLE accounts RENAME INDEX old TO new;", "accounts"},
		{"mariadb", "ALTER TABLE accounts RENAME KEY old TO new;", "accounts"},
		{"mysql", "INSERT INTO accounts (id,label) VALUES (1,'x');", "accounts"},
	}
	for _, tc := range cases {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var ev []sqlNavOccurrence
				var ok bool
				if all {
					ev, ok = sqlNavAllOccurrences(tc.sql, tc.dialect)
				} else {
					ev, ok = sqlNavOccurrences(tc.sql, tc.dialect)
				}
				if !ok {
					t.Fatal("tokenization failed")
				}
				found := false
				for _, item := range ev {
					if !item.isColumn && item.identity == tc.want {
						found = true
					}
				}
				if !found {
					t.Fatalf("lost supported physical identity all=%v: %+v", all, ev)
				}
			}
		})
	}
}

// One invalid statement cannot borrow a valid physical declaration from a
// different migration or contaminate the lifecycle of a correct statement.
func TestSQLReviewEmissionGateLexicalBoundaryMatrix(t *testing.T) {
	for _, tc := range []struct{ dialect, sql string }{
		{"postgres", "DROP TABLE accounts 'forgotten';"},
		{"postgres", "DROP TABLE accounts 'forgotten', other;"},
		{"postgres", "ALTER TABLE accounts RENAME TO archive 'forgotten';"},
		{"postgres", "ALTER TABLE accounts RENAME COLUMN id TO other 'forgotten';"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN id INT 'forgotten', ADD COLUMN x INT;"},
		{"postgres", "CREATE INDEX ix 'forgotten' ON accounts (id);"},
		{"postgres", "CREATE INDEX ix ON 'forgotten' accounts (id);"},
		{"postgres", "CREATE TABLE accounts (id INT DEFAULT 'x' 'extra');"},
		{"postgres", "SELECT 'x' 'extra', a.id FROM accounts a;"},
		{"postgres", "INSERT INTO accounts (id) VALUES ('x' 'extra');"},
		{"postgres", "UPDATE accounts SET id='x' 'extra';"},
		{"postgres", "DELETE FROM accounts 'extra' WHERE id=1;"},
		{"postgres", "DELETE FROM accounts 'extra';"},
		{"mysql", "DROP TABLE accounts 'extra';"},
		{"mysql", "CREATE INDEX ix 'extra' ON accounts (id);"},
		{"mariadb", "INSERT INTO accounts (id) VALUES ('x' 'extra');"},
	} {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var got []sqlNavOccurrence
				var ok bool
				if all {
					got, ok = sqlNavAllOccurrences(tc.sql, tc.dialect)
				} else {
					got, ok = sqlNavOccurrences(tc.sql, tc.dialect)
				}
				if ok && len(got) > 0 {
					t.Fatalf("unconsumed literal creates physical identity (all=%v): %+v", all, got)
				}
			}
		})
	}
}

// A source-only grammar is unsafe if a header or rename branch bypasses the
// same complete-proof rule applied to CREATE TABLE and ordinary SELECT.
func TestSQLReviewEmissionGateOtherProducerBoundaries(t *testing.T) {
	bad := []struct{ dialect, sql string }{
		{"postgres", "ALTER VIEW reporting.events FROBNICATE;"},
		{"postgres", "ALTER VIEW reporting.events RENAME TO;"},
		{"postgres", "RENAME TABLE accounts TO archived;"},
		{"postgres", "CREATE OR REPLACE TABLE accounts (id INT);"},
		{"postgres", "CREATE UNLOGGED VIEW bogus AS SELECT 1;"},
		{"postgres", "CREATE MATERIALIZED VIEW bogus AS SELECT 1 'ignored' WITH DATA;"},
		{"postgres", "CREATE MATERIALIZED VIEW bogus AS SELECT 1 'ignored' WITH NO DATA;"},
		{"mysql", "ALTER VIEW v AS FROBNICATE;"},
		{"mariadb", "ALTER VIEW v FROBNICATE;"},
	}
	for _, tc := range bad {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var ev []sqlNavOccurrence
				var ok bool
				if all {
					ev, ok = sqlNavAllOccurrences(tc.sql, tc.dialect)
				} else {
					ev, ok = sqlNavOccurrences(tc.sql, tc.dialect)
				}
				if ok && len(ev) > 0 {
					t.Fatalf("invalid branch published source all=%v: %+v", all, ev)
				}
			}
		})
	}
	for _, tc := range []struct{ sql, identity string }{
		{"ALTER VIEW reporting.events RENAME TO archived;", "reporting\x00archived"},
		{"ALTER VIEW reporting.events SET SCHEMA archive;", "archive\x00events"},
	} {
		t.Run("valid/"+tc.sql, func(t *testing.T) {
			ev, ok := sqlNavOccurrences(tc.sql, "postgres")
			if !ok {
				t.Fatal("parse failure")
			}
			for _, event := range ev {
				if event.identity == tc.identity {
					return
				}
			}
			t.Fatalf("lost proven view relation rename: %+v", ev)
		})
	}
}

// Deterministic, grammar-boundary metamorphic test. Inserting an independent
// quoted SQL literal before any visible token of these source-backed fixtures
// cannot silently preserve their physical graph. The corpus deliberately
// avoids positions where an inserted standalone string would be a legal
// SELECT projection or standalone DEFAULT/VALUES value.
func TestSQLReviewEmissionGateNoInvisibleLiteralMutations(t *testing.T) {
	fixtures := []struct{ dialect, sql string }{
		{"postgres", "CREATE TABLE accounts (id BIGINT PRIMARY KEY, label TEXT);"},
		{"postgres", "CREATE TABLE accounts (id NUMERIC(10,2), FOREIGN KEY (id) REFERENCES owners(id));"},
		{"postgres", "CREATE TABLE accounts (id TEXT DEFAULT 'ready');"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN id INT, RENAME COLUMN old TO newest;"},
		{"postgres", "CREATE INDEX idx ON accounts (id) WHERE id=1;"},
		{"postgres", "CREATE VIEW v AS SELECT a.id FROM accounts a;"},
		{"postgres", "CREATE MATERIALIZED VIEW v AS SELECT 1 WITH NO DATA;"},
		{"postgres", "SELECT a.id FROM accounts a WHERE a.id=1;"},
		{"postgres", "INSERT INTO accounts (id,label) VALUES (1,'owned');"},
		{"postgres", "UPDATE accounts SET id=2 WHERE id=1;"},
		{"postgres", "DELETE FROM accounts WHERE id=1;"},
		{"postgres", "TRUNCATE TABLE accounts;"},
		{"postgres", "DROP TABLE accounts;"},
		{"postgres", "ALTER VIEW reporting.events RENAME TO archived;"},
		{"mysql", "CREATE TABLE child LIKE parent;"},
		{"mariadb", "CREATE TABLE child (id INT) ENGINE=InnoDB;"},
		{"postgres", "CREATE VIEW phantom AS SELECT 1 AS id;"},
		{"postgres", "CREATE MATERIALIZED VIEW phantom AS SELECT 1 AS id WITH NO DATA;"},
		{"postgres", "ALTER MATERIALIZED VIEW reporting.daily RENAME TO renamed;"},
		{"mysql", "CREATE INDEX ix ON accounts (id);"},
		{"mysql", "CREATE TABLE accounts (id VARCHAR(64));"},
		{"mysql", "ALTER TABLE accounts RENAME INDEX old TO new;"},
		{"mariadb", "CREATE VIEW IF NOT EXISTS v AS SELECT 1;"},
		{"mariadb", "INSERT INTO accounts (id) VALUES (1);"},
	}
	for _, tc := range fixtures {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			original, ok := sqlNavOccurrences(tc.sql, tc.dialect)
			if !ok || len(original) == 0 {
				t.Fatalf("fixture lacks proven source: %+v", original)
			}
			tokens, ok := sqlDDLTokens(tc.sql, tc.dialect)
			if !ok {
				t.Fatal("cannot tokenize fixture")
			}
			for _, tok := range tokens {
				if tok.start.Line != 0 {
					t.Fatal("one-line test fixture expected")
				}
				at := tok.start.Character
				sql := tc.sql[:at] + " 'discarded' " + tc.sql[at:]
				for _, all := range []bool{false, true} {
					var got []sqlNavOccurrence
					var valid bool
					if all {
						got, valid = sqlNavAllOccurrences(sql, tc.dialect)
					} else {
						got, valid = sqlNavOccurrences(sql, tc.dialect)
					}
					if valid && len(got) > 0 {
						t.Fatalf("invisible SQL literal at %d (before %q) kept physical graph all=%v sql=%q events=%+v", at, tok.text, all, sql, got)
					}
				}
			}
		})
	}
}
