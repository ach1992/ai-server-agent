package executor

import "testing"

func TestSQLIndexSourceGraphConservativeReferences(t *testing.T) {
	for _, tc := range []struct {
		name, source, dialect, table string
		columns                      []string
	}{
		{"postgres", "CREATE INDEX idx_accounts_id ON public.accounts (id);", "postgres", "public.accounts", []string{"public.accounts.id"}},
		{"postgres_concurrent", "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_accounts_id ON ONLY public.accounts USING BTREE (id);", "postgres", "public.accounts", []string{"public.accounts.id"}},
		{"postgres_unnamed", "CREATE INDEX ON accounts (id, active);", "postgres", "accounts", []string{"accounts.id", "accounts.active"}},
		{"mysql_fulltext", "CREATE FULLTEXT INDEX idx_articles_title ON articles (title);", "mysql", "articles", []string{"articles.title"}},
		{"mysql_method_before_on", "CREATE INDEX idx USING BTREE ON accounts (id);", "mysql", "accounts", []string{"accounts.id"}},
		{"mysql_spatial_rtree", "CREATE SPATIAL INDEX geom_idx USING RTREE ON geom (g);", "mysql", "geom", []string{"geom.g"}},
		{"mariadb_replace", "CREATE OR REPLACE UNIQUE INDEX idx_accounts_id ON accounts (id);", "mariadb", "accounts", []string{"accounts.id"}},
		{"mariadb_if_not_exists", "CREATE INDEX IF NOT EXISTS idx ON accounts (id);", "mariadb", "accounts", []string{"accounts.id"}},
		{"mariadb_spatial", "CREATE SPATIAL INDEX idx_geo_point ON geo (point);", "mariadb", "geo", []string{"geo.point"}},
		{"expression_excluded", "CREATE INDEX ix ON accounts (LOWER(email), id DESC, code) WHERE active;", "postgres", "accounts", []string{"accounts.code"}},
		{"function_excluded", "CREATE INDEX ix ON accounts ((id+1));", "postgres", "accounts", nil},
		{"quoted", "CREATE UNIQUE INDEX `idx` ON `orders` (`id`);", "mysql", "orders", []string{"orders.id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all, ok := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !ok || len(all) != len(tc.columns)+1 {
				t.Fatalf("index references inconsistent: valid=%t %+v", ok, all)
			}
			if all[0].name != tc.table || all[0].declaration || all[0].drop || all[0].isColumn {
				t.Fatalf("index table was not a reference: %+v", all[0])
			}
			for i, want := range tc.columns {
				if all[i+1].name != want || !all[i+1].isColumn || all[i+1].declaration || all[i+1].drop {
					t.Fatalf("index column %d not source-backed: %+v want=%s", i, all[i+1], want)
				}
			}
		})
	}
	for _, unsafe := range []string{
		"CREATE INDEX idx ON accounts;",                   // no actual index column list
		"CREATE INDEX idx ON accounts USING custom (id);", // unknown access method
		"CREATE UNIQUE KEY idx ON accounts (id);",
		"CREATE INDEX idx ON (accounts) (id);",
	} {
		all, ok := sqlNavAllOccurrences(unsafe, "mysql")
		if ok && len(all) != 0 {
			t.Fatalf("unsupported index syntax fabricated SQL graph: %s %+v", unsafe, all)
		}
	}
	for _, invalidDialect := range []struct{ sql, dialect string }{
		{"CREATE FULLTEXT INDEX ix ON articles (title);", "postgres"},
		{"CREATE SPATIAL INDEX ix ON points (position);", "postgres"},
		{"CREATE INDEX CONCURRENTLY ix ON accounts (id);", "mysql"},
		{"CREATE INDEX ix ON ONLY accounts (id);", "mariadb"},
		{"CREATE INDEX IF NOT EXISTS ix ON accounts (id);", "mysql"},
		{"CREATE INDEX IF NOT EXISTS ON accounts (id);", "postgres"},
		{"CREATE OR REPLACE INDEX ix ON accounts (id);", "postgres"},
		{"CREATE INDEX ix ON accounts USING BTREE (id);", "mysql"},
		{"CREATE INDEX ix USING RTREE ON accounts (id);", "mysql"},
	} {
		all, ok := sqlNavAllOccurrences(invalidDialect.sql, invalidDialect.dialect)
		if ok && len(all) != 0 {
			t.Fatalf("SQL dialect-specific invalid index invented references: %s/%s %+v", invalidDialect.dialect, invalidDialect.sql, all)
		}
	}
}
