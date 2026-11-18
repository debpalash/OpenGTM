package importv3

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// reasons explains, per file name, why its tables are not imported. Every SQLite table under the data dir that no
// Spec covers is reported with its row count, so nothing is skipped silently.
var reasons = map[string]string{
	"enrichment_cache.db": "provider-result cache (also email-verify cache); the Go runtime rebuilds it on demand",
	"leads.db": "pre-Postgres lead store; v3.0.0 reads it only when PG_LEAD_STORE is off. Rows you still need must be moved " +
		"with v3.0.0's scripts/migrate_sqlite_to_pg.py BEFORE upgrading; the Go runtime has no importer for it",
	"outreach.db":   "legacy sequence store with no caller in v3.0.0 (outreach lives in the Postgres outreach_* tables)",
	"signals.db":    "legacy signal store (v3.0.0 reads and writes signals through the Postgres store)",
	"openmemory.db": "optional third-party memory backend; its format is not documented here",
}

// scanNotMigrated lists SQLite tables under dataDir that are not imported.
func scanNotMigrated(tmp, dataDir string, specs []Spec) []NotMigrated {
	covered := map[string][]string{} // file -> sqlite tables imported
	for _, s := range specs {
		covered[s.File] = append(covered[s.File], s.Table)
	}
	var files []string
	if m, err := filepath.Glob(filepath.Join(dataDir, "*.db")); err == nil {
		files = append(files, m...)
	}
	if m, err := filepath.Glob(filepath.Join(dataDir, "workspaces", "*", "*.db")); err == nil {
		files = append(files, m...)
	}
	slices.Sort(files)
	var out []NotMigrated
	for _, f := range files {
		rel, _ := filepath.Rel(dataDir, f)
		top := !strings.Contains(rel, string(os.PathSeparator))
		lite, err := openSnapshot(f, tmp)
		if err != nil {
			out = append(out, NotMigrated{File: rel, Table: "(unreadable: " + err.Error() + ")", Reason: "could not be opened"})
			continue
		}
		reason := reasons[filepath.Base(f)]
		if reason == "" {
			reason = "no importer for this store"
		}
		rs, err := lite.QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err == nil {
			var names []string
			for rs.Next() {
				var n string
				if rs.Scan(&n) == nil {
					names = append(names, n)
				}
			}
			rs.Close()
			for _, n := range names {
				if top && slices.Contains(covered[filepath.Base(f)], n) {
					continue
				}
				out = append(out, NotMigrated{File: rel, Table: n, Rows: count(lite, n), Reason: reason})
			}
		}
		lite.Close()
	}
	return out
}

func count(db *sql.DB, table string) int {
	var n int
	if db.QueryRow(`SELECT count(*) FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`).Scan(&n) != nil {
		return -1
	}
	return n
}
