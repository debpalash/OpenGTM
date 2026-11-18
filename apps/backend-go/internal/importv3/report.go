package importv3

import (
	"fmt"
	"io"
)

// Print writes the per-table counts and the not-migrated list.
func (r Report) Print(w io.Writer) {
	mode := "committed"
	if r.DryRun {
		mode = "dry run (rolled back)"
	}
	fmt.Fprintf(w, "import-v3 %s\n", mode)
	fmt.Fprintf(w, "%-32s %8s %8s %9s %9s %8s %8s\n", "table", "source", "skipped", "inserted", "existing", "present", "dest")
	for _, t := range r.Tables {
		dest := fmt.Sprint(t.Dest)
		if t.Dest < 0 {
			dest = "(rls)"
		}
		fmt.Fprintf(w, "%-32s %8d %8d %9d %9d %8d %8s\n", t.Table, t.Source, t.Skipped, t.Inserted, t.Existing, t.Present, dest)
		if t.BadJSON > 0 {
			fmt.Fprintf(w, "  warning: %s: %d unparseable JSON value(s) replaced by the empty default\n", t.Table, t.BadJSON)
		}
		if t.Skipped > 0 {
			fmt.Fprintf(w, "  note: %s: %d source row(s) not copied (no workspace/user, or parent skipped)\n", t.Table, t.Skipped)
		}
		if len(t.Orphans) > 0 {
			fmt.Fprintf(w, "  warning: %s refers to user id(s) with no row in users (copied anyway): %v\n", t.Table, t.Orphans)
		}
	}
	if len(r.NotMigrated) == 0 {
		fmt.Fprintln(w, "not migrated: nothing")
		return
	}
	fmt.Fprintln(w, "NOT MIGRATED (rows stay in the SQLite file, which is never modified):")
	for _, n := range r.NotMigrated {
		fmt.Fprintf(w, "  %s table %s: %d rows: %s\n", n.File, n.Table, n.Rows, n.Reason)
	}
}
