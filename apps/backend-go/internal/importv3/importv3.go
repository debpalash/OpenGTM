package importv3

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver ("sqlite"); linked into cmd/migrate only
)

// Options configures a run.
type Options struct {
	DataDir string // the v3.0.0 data/ directory
	DryRun  bool   // do everything, verify, then roll back
	// AllowMissingWorkspaces accepts a data dir without workspaces.db (an install that never created a workspace).
	AllowMissingWorkspaces bool
	Out                    io.Writer
}

// TableReport is the outcome for one table.
type TableReport struct {
	Table    string
	Source   int // rows read from SQLite (after exclusions)
	Skipped  int // rows deliberately not copied (invisible or orphaned source rows)
	Inserted int // rows that were missing in PostgreSQL
	Existing int // rows already present, left untouched
	Present  int // source keys found in PostgreSQL after the run (verification)
	Dest     int // total rows in the PostgreSQL table
	BadJSON  int // JSON text columns replaced by their default
	Orphans  []int64
}

// NotMigrated is a SQLite table nothing imports.
type NotMigrated struct {
	File, Table string
	Rows        int
	Reason      string
}

// Report is the result of a run.
type Report struct {
	Tables      []TableReport
	NotMigrated []NotMigrated
	DryRun      bool
}

// Mismatch is returned (with the Report) when verification fails; nothing was committed.
var ErrVerification = errors.New("importv3: verification failed: not every source row is present in PostgreSQL")

var storeOrder = []string{wsFile, "data.db", "chat_history.db", "functions.db", "chat_memory.db"}

// Run imports every store found in opts.DataDir. db must connect as a role that can write the tenant tables (the
// migrator's owner role); RLS stays on: every tenant batch sets app.workspace_id for its transaction like the API.
func Run(ctx context.Context, db *sql.DB, opts Options) (Report, error) {
	rep := Report{DryRun: opts.DryRun}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	st, err := os.Stat(opts.DataDir)
	if err != nil || !st.IsDir() {
		return rep, fmt.Errorf("importv3: --data-dir %q is not a directory", opts.DataDir)
	}
	if _, err := os.Stat(filepath.Join(opts.DataDir, wsFile)); err != nil && !opts.AllowMissingWorkspaces {
		return rep, fmt.Errorf("importv3: %s not found in %s: wrong --data-dir? (pass --allow-missing-workspaces only if this install never had a workspace)", wsFile, opts.DataDir)
	}
	tmp, err := os.MkdirTemp("", "opengtm-importv3-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)

	specs := Specs()
	var firstErr error
	for _, file := range storeOrder {
		path := filepath.Join(opts.DataDir, file)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		var fs []Spec
		for _, s := range specs {
			if s.File == file {
				fs = append(fs, s)
			}
		}
		tr, err := importStore(ctx, db, tmp, path, fs, opts.DryRun)
		rep.Tables = append(rep.Tables, tr...)
		if err != nil {
			firstErr = fmt.Errorf("importv3: %s: %w (nothing from this store was committed)", file, err)
			break
		}
	}
	rep.NotMigrated = scanNotMigrated(tmp, opts.DataDir, specs)
	return rep, firstErr
}

// snapshot copies a SQLite file (and its WAL) so the original is never opened.
func snapshot(src, dir string) (string, error) {
	dst := filepath.Join(dir, filepath.Base(filepath.Dir(src))+"-"+filepath.Base(src))
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(src + suffix)
		if err != nil {
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			in.Close()
			return "", err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
	}
	return dst, nil
}

func openSnapshot(src, dir string) (*sql.DB, error) {
	snap, err := snapshot(src, dir)
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", snap)
}

func readRows(ctx context.Context, lite *sql.DB, s Spec) ([]Row, int, error) {
	var src []string
	for _, c := range s.Columns {
		if !slices.Contains(s.Derived, c) {
			src = append(src, `"`+c+`"`)
		}
	}
	order := ""
	if s.OrderBy != "" {
		order = " ORDER BY " + s.OrderBy
	}
	rs, err := lite.QueryContext(ctx, `SELECT `+strings.Join(src, ", ")+` FROM "`+s.Table+`"`+order)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", s.Table, err)
	}
	defer rs.Close()
	var out []Row
	badJSON := 0
rows:
	for rs.Next() {
		vals := make([]any, len(src))
		ptrs := make([]any, len(src))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, 0, err
		}
		r := Row{}
		i := 0
		for _, c := range s.Columns {
			if slices.Contains(s.Derived, c) {
				r[c] = nil
				continue
			}
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			r[c] = vals[i]
			i++
		}
		for c, bad := range s.Exclude {
			if v, ok := r[c].(string); ok && slices.Contains(bad, v) {
				continue rows
			}
		}
		for c, fn := range s.Convert {
			v, err := fn(r[c])
			if err != nil {
				return nil, 0, fmt.Errorf("%s.%s: %w", s.Name, c, err)
			}
			r[c] = v
		}
		for c, def := range s.JSON {
			text, bad := normalizeJSON(r[c], def)
			r[c] = text
			if bad {
				badJSON++
			}
		}
		out = append(out, r)
	}
	return out, badJSON, rs.Err()
}

// normalizeJSON validates a JSON text column: NULL -> default; unparseable or of the wrong top-level kind ->
// default, reported as bad.
func normalizeJSON(v any, def string) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return def, v != nil
	}
	var val any
	if json.Unmarshal([]byte(s), &val) != nil {
		return def, true
	}
	_, isArr := val.([]any)
	_, isObj := val.(map[string]any)
	if (def == "[]" && !isArr) || (def == "{}" && !isObj) {
		return def, true
	}
	return s, false
}

func keyOf(r Row, cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprint(r[c])
	}
	return strings.Join(parts, "\x1f")
}

func validateUnique(s Spec, rows []Row) error {
	seen := map[string]bool{}
	for _, r := range rows {
		k := keyOf(r, s.PK)
		if seen[k] {
			return fmt.Errorf("%s: duplicate key %v in the source", s.Name, s.PK)
		}
		seen[k] = true
	}
	if s.Name == "workspaces" {
		slugs := map[string]bool{}
		for _, r := range rows {
			if slugs[str(r["slug"])] {
				return fmt.Errorf("workspaces: duplicate slug %q in the source", str(r["slug"]))
			}
			slugs[str(r["slug"])] = true
		}
	}
	return nil
}

func upsertSQL(s Spec) string {
	cols := make([]string, len(s.Columns))
	ph := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		cols[i] = c
		ph[i] = fmt.Sprintf("$%d", i+1)
		if _, ok := s.JSON[c]; ok {
			ph[i] += "::jsonb"
		}
	}
	// DO NOTHING, not DO UPDATE: a re-run must never revert a row changed in the Go app since the first run.
	return "INSERT INTO " + s.Name + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(ph, ", ") +
		") ON CONFLICT (" + strings.Join(s.PK, ", ") + ") DO NOTHING"
}

func importStore(ctx context.Context, db *sql.DB, tmp, path string, specs []Spec, dry bool) ([]TableReport, error) {
	lite, err := openSnapshot(path, tmp)
	if err != nil {
		return nil, err
	}
	defer lite.Close()
	loaded := map[string][]Row{}
	reports := map[string]*TableReport{}
	for _, s := range specs {
		rows, bad, err := readRows(ctx, lite, s)
		if err != nil {
			return nil, err
		}
		reports[s.Name] = &TableReport{Table: s.Name, BadJSON: bad}
		loaded[s.Name] = rows
	}
	for _, s := range specs {
		if s.PostLoad != nil {
			loaded[s.Name], reports[s.Name].Skipped = s.PostLoad(loaded[s.Name], loaded)
		}
		reports[s.Name].Source = len(loaded[s.Name])
		if err := validateUnique(s, loaded[s.Name]); err != nil {
			return nil, err
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	users := map[int64]bool{}
	ur, err := tx.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return nil, err
	}
	for ur.Next() {
		var id int64
		if err := ur.Scan(&id); err != nil {
			ur.Close()
			return nil, err
		}
		users[id] = true
	}
	ur.Close()

	if err := validateFKs(ctx, tx, specs, loaded); err != nil {
		return nil, err
	}
	var mismatch []string
	for _, s := range specs {
		rep := reports[s.Name]
		rows := loaded[s.Name]
		stmt := upsertSQL(s)
		for _, batch := range tenantBatches(s, rows) {
			if s.Tenant != "" {
				if _, err := tx.ExecContext(ctx, `SELECT set_config('app.workspace_id', $1, true)`, batch.ws); err != nil {
					return nil, err
				}
			}
			for _, r := range batch.rows {
				args := make([]any, len(s.Columns))
				for i, c := range s.Columns {
					args[i] = r[c]
				}
				res, err := tx.ExecContext(ctx, stmt, args...)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", s.Name, err)
				}
				if n, _ := res.RowsAffected(); n == 1 {
					rep.Inserted++
				} else {
					rep.Existing++
				}
			}
			n, err := presentCount(ctx, tx, s, batch.rows)
			if err != nil {
				return nil, err
			}
			rep.Present += n
		}
		if s.Sequence != "" {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(
				`SELECT setval(pg_get_serial_sequence('%[1]s', '%[2]s'), COALESCE((SELECT max(%[2]s) FROM %[1]s), 1), (SELECT max(%[2]s) IS NOT NULL FROM %[1]s))`,
				s.Name, s.Sequence)); err != nil {
				return nil, err
			}
		}
		if s.Tenant != "" { // total rows are only countable per tenant under RLS; report the source side
			rep.Dest = -1
		} else if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+s.Name).Scan(&rep.Dest); err != nil {
			return nil, err
		}
		if s.UserCol != "" {
			seen := map[int64]bool{}
			for _, r := range rows {
				if id, ok := r[s.UserCol].(int64); ok && !users[id] && !seen[id] {
					seen[id] = true
					rep.Orphans = append(rep.Orphans, id)
				}
			}
			slices.Sort(rep.Orphans)
		}
		if rep.Present != rep.Source {
			mismatch = append(mismatch, fmt.Sprintf("%s: %d source rows, %d present", s.Name, rep.Source, rep.Present))
		}
	}
	out := make([]TableReport, 0, len(specs))
	for _, s := range specs {
		out = append(out, *reports[s.Name])
	}
	if len(mismatch) > 0 {
		return out, fmt.Errorf("%w: %s", ErrVerification, strings.Join(mismatch, "; "))
	}
	if dry {
		return out, nil // the deferred rollback discards everything
	}
	return out, tx.Commit()
}

type batch struct {
	ws   string
	rows []Row
}

func tenantBatches(s Spec, rows []Row) []batch {
	if s.Tenant == "" {
		return []batch{{rows: rows}}
	}
	byWS := map[string][]Row{}
	for _, r := range rows {
		ws := str(r[s.Tenant])
		byWS[ws] = append(byWS[ws], r)
	}
	keys := make([]string, 0, len(byWS))
	for k := range byWS {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]batch, 0, len(keys))
	for _, k := range keys {
		out = append(out, batch{k, byWS[k]})
	}
	return out
}

// presentCount counts how many of rows' keys exist in the destination table (inside the tenant's RLS scope).
func presentCount(ctx context.Context, tx *sql.Tx, s Spec, rows []Row) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, keyOf(r, s.PK))
	}
	parts := make([]string, len(s.PK))
	for i, c := range s.PK {
		parts[i] = c + "::text"
	}
	var n int
	// A JSON array parameter keeps this independent of the driver's array support.
	b, _ := json.Marshal(keys)
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+s.Name+` WHERE (`+strings.Join(parts, " || chr(31) || ")+`) IN (SELECT jsonb_array_elements_text($1::jsonb))`, string(b)).Scan(&n)
	return n, err
}

// validateFKs fails before the first write when a source row references a parent PostgreSQL would reject
// (SQLite never enforced these).
func validateFKs(ctx context.Context, tx *sql.Tx, specs []Spec, loaded map[string][]Row) error {
	byName := map[string]Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	for _, s := range specs {
		for _, fk := range s.FKs {
			parents := map[string]bool{}
			for _, r := range loaded[fk.Parent] {
				parents[keyOf(r, fk.PCols)] = true
			}
			if p, ok := byName[fk.Parent]; !ok || p.Tenant == "" { // non-tenant parents may already be in PostgreSQL
				rs, err := tx.QueryContext(ctx, `SELECT `+strings.Join(fk.PCols, "||chr(31)||")+`::text FROM `+fk.Parent)
				if err != nil {
					return err
				}
				for rs.Next() {
					var k string
					if err := rs.Scan(&k); err != nil {
						rs.Close()
						return err
					}
					parents[k] = true
				}
				rs.Close()
			}
			var bad []string
			for _, r := range loaded[s.Name] {
				k := keyOf(r, fk.Cols)
				if !parents[k] && !slices.Contains(bad, k) {
					bad = append(bad, strings.ReplaceAll(k, "\x1f", "/"))
				}
			}
			if len(bad) > 0 {
				slices.Sort(bad)
				shown := bad
				if len(shown) > 20 {
					shown = shown[:20]
				}
				return fmt.Errorf("%s: %d distinct %s value(s) reference missing %s(%s): %s. Nothing was written; fix the source copy "+
					"(delete the orphan rows or add the missing parents) and re-run",
					s.Name, len(bad), strings.Join(fk.Cols, "/"), fk.Parent, strings.Join(fk.PCols, ", "), strings.Join(shown, ", "))
			}
		}
	}
	return nil
}
