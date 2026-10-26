//go:build integration

package leadswrite_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool     *pgxpool.Pool
	owner    *pgxpool.Pool
	wsA, wsB string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 20)}
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e.owner = owner
	e.wsA = fmt.Sprintf("t3a_%d", os.Getpid())
	e.wsB = fmt.Sprintf("t3b_%d", os.Getpid())
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM leads WHERE workspace_id = ANY($1)`, []string{e.wsA, e.wsB})
	}
	clean()
	t.Cleanup(clean)
	return e
}

func (e *env) in(t *testing.T, ws string, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return tenancy.InWorkspace(context.Background(), e.pool, ws, fn)
}

func (e *env) create(t *testing.T, ws string, n leadswrite.NewLead) int64 {
	t.Helper()
	var id int64
	err := e.in(t, ws, func(tx pgx.Tx) (err error) { id, err = leadswrite.Create(context.Background(), tx, ws, n); return })
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return id
}

// row reads one lead as text columns (owner pool, bypasses RLS) except search_tsv.
func (e *env) row(t *testing.T, id int64) map[string]*string {
	t.Helper()
	rows, err := e.owner.Query(context.Background(),
		`SELECT key, value #>> '{}' FROM leads l, jsonb_each(to_jsonb(l) - 'search_tsv') WHERE l.id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]*string{}
	for rows.Next() {
		var k string
		var v *string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func s(v string) *string { return &v }

func TestCreateMatchesPythonRow(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{
		Company: "Fresh Co", City: "Oslo", Website: "https://fresh.example.com", Email: "a@b.co",
		Phone: "123", Specialization: "IT", ContactPerson: "Ann", LinkedinURL: "https://li/x", Notes: "n", Source: "manual",
	})
	r := e.row(t, id)
	want := map[string]string{
		"workspace_id": e.wsA, "company": "Fresh Co", "city": "Oslo", "website": "https://fresh.example.com",
		"email": "a@b.co", "phone": "123", "specialization": "IT", "contact_person": "Ann",
		"linkedin_url": "https://li/x", "notes": "n", "source": "manual",
		"score": "0", "score_tier": "unqualified", "status": "new",
		"employee_count_exact": "0", "enrichment_attempts": "0",
	}
	for k, v := range r {
		if v == nil {
			t.Errorf("column %s is NULL; the Python Lead dataclass writes every column", k)
			continue
		}
		if k == "id" || k == "created_at" || k == "updated_at" {
			continue
		}
		w, ok := want[k]
		if !ok {
			w = "" // every other column is the "" dataclass default
		}
		if *v != w {
			t.Errorf("column %s = %q, want %q", k, *v, w)
		}
	}
	if len(r) != 56 {
		t.Errorf("columns = %d, want 56 (all but search_tsv)", len(r))
	}
	// Python isoformat(): 2026-01-02T03:04:05[.ffffff]+00:00
	for _, k := range []string{"created_at", "updated_at"} {
		v := *r[k]
		if !strings.HasSuffix(v, "+00:00") || v[10] != 'T' {
			t.Errorf("%s = %q, not Python isoformat()", k, v)
		}
	}
}

func TestCreateDuplicateReturnsExistsError(t *testing.T) {
	e := newEnv(t)
	for _, city := range []string{"Oslo", ""} {
		id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Dup", City: city, Notes: "first"})
		err := e.in(t, e.wsA, func(tx pgx.Tx) error {
			_, err := leadswrite.Create(context.Background(), tx, e.wsA, leadswrite.NewLead{Company: "Dup", City: city, Notes: "second"})
			return err
		})
		var ee *leadswrite.ExistsError
		if !errors.As(err, &ee) || ee.LeadID != id {
			t.Fatalf("city %q: err = %v, want ExistsError{%d}", city, err, id)
		}
		if got := *e.row(t, id)["notes"]; got != "first" {
			t.Errorf("existing row modified: notes = %q", got)
		}
	}
	// Same key in another workspace is allowed.
	e.create(t, e.wsB, leadswrite.NewLead{Company: "Dup", City: "Oslo"})
}

func TestCreateRaceExactlyOneWins(t *testing.T) {
	e := newEnv(t)
	const n = 16
	var wg sync.WaitGroup
	type res struct {
		id  int64
		err error
	}
	out := make([]res, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out[i].err = e.in(t, e.wsA, func(tx pgx.Tx) (err error) {
				out[i].id, err = leadswrite.Create(context.Background(), tx, e.wsA, leadswrite.NewLead{Company: "Race", City: "X"})
				return
			})
		}(i)
	}
	close(start)
	wg.Wait()
	var winner int64
	wins := 0
	for _, r := range out {
		if r.err == nil {
			wins++
			winner = r.id
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want 1", wins)
	}
	for _, r := range out {
		if r.err == nil {
			continue
		}
		var ee *leadswrite.ExistsError
		if !errors.As(r.err, &ee) || ee.LeadID != winner {
			t.Errorf("loser err = %v, want ExistsError{%d}", r.err, winner)
		}
	}
}

func (e *env) update(t *testing.T, ws string, id int64, f map[string]any) (bool, error) {
	t.Helper()
	var found bool
	err := e.in(t, ws, func(tx pgx.Tx) (err error) {
		found, err = leadswrite.UpdateFields(context.Background(), tx, id, f)
		return
	})
	return found, err
}

func TestUpdateFieldsIgnoresImmutable(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Imm", City: "A"})
	before := e.row(t, id)
	found, err := e.update(t, e.wsA, id, map[string]any{
		"id": 999999, "workspace_id": e.wsB, "created_at": "x", "updated_at": "y", "search_tsv": "z",
	})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	after := e.row(t, id)
	for k, v := range before {
		if *after[k] != *v {
			t.Errorf("%s changed: %q -> %q", k, *v, *after[k])
		}
	}
}

func TestUpdateFieldsUnknownKeysDropped(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Unk", City: "A"})
	found, err := e.update(t, e.wsA, id, map[string]any{"nope": 1, "notes; DROP TABLE leads": "x", "notes": "kept"})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := *e.row(t, id)["notes"]; got != "kept" {
		t.Errorf("notes = %q", got)
	}
}

func TestUpdateFieldsAdvancesUpdatedAt(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Adv", City: "A"})
	before := *e.row(t, id)["updated_at"]
	if _, err := e.update(t, e.wsA, id, map[string]any{"score": 7, "company_size": nil, "notes": "x"}); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, id)
	if *r["updated_at"] <= before {
		t.Errorf("updated_at not advanced: %q -> %q", before, *r["updated_at"])
	}
	if *r["score"] != "7" || r["company_size"] != nil {
		t.Errorf("score=%v company_size=%v", *r["score"], r["company_size"])
	}
}

func TestUpdateFieldsTypeError(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Typ", City: "A", Notes: "orig"})
	before := e.row(t, id)
	for field, f := range map[string]map[string]any{
		"score":                {"notes": "changed", "score": "high"},
		"company":              {"notes": "changed", "company": nil},
		"employee_count_exact": {"notes": "changed", "employee_count_exact": "7.0"},
		"enrichment_attempts":  {"notes": "changed", "enrichment_attempts": int64(1) << 40},
		"notes":                {"notes": map[string]any{"a": 1}},
		"description":          {"description": "a\x00b"},
	} {
		_, err := e.update(t, e.wsA, id, f)
		var fe *leadswrite.FieldError
		if !errors.As(err, &fe) || fe.Field != field {
			t.Errorf("%s: err = %v, want FieldError{%s}", field, err, field)
		}
		after := e.row(t, id)
		for k, v := range before {
			if *after[k] != *v {
				t.Errorf("%s: partial write, %s changed", field, k)
			}
		}
	}
}

func TestUpdateFieldsEmptyAfterFilterIsNoop(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Emp", City: "A"})
	before := *e.row(t, id)["updated_at"]
	found, err := e.update(t, e.wsA, id, map[string]any{"id": 5, "bogus": 1})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if after := *e.row(t, id)["updated_at"]; after != before {
		t.Errorf("updated_at bumped on no-op: %q -> %q", before, after)
	}
	found, err = e.update(t, e.wsA, id+100000, map[string]any{"bogus": 1})
	if err != nil || found {
		t.Errorf("missing id: found=%v err=%v", found, err)
	}
}

func TestUpdateStatus(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "St", City: "A"})
	var found bool
	if err := e.in(t, e.wsA, func(tx pgx.Tx) (err error) {
		found, err = leadswrite.UpdateStatus(context.Background(), tx, id, "contacted")
		return
	}); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := *e.row(t, id)["status"]; got != "contacted" {
		t.Errorf("status = %q", got)
	}
}

func TestDeleteRemovesRowOnly(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Del", City: "A"})
	other := e.create(t, e.wsA, leadswrite.NewLead{Company: "Keep", City: "A"})
	var found bool
	if err := e.in(t, e.wsA, func(tx pgx.Tx) (err error) { found, err = leadswrite.Delete(context.Background(), tx, id); return }); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if e.row(t, id) != nil || e.row(t, other) == nil {
		t.Error("delete removed wrong rows")
	}
	if err := e.in(t, e.wsA, func(tx pgx.Tx) (err error) { found, err = leadswrite.Delete(context.Background(), tx, id); return }); err != nil || found {
		t.Errorf("second delete found=%v err=%v", found, err)
	}
}

func TestWritesTenantIsolation(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsB, leadswrite.NewLead{Company: "Theirs", City: "A", Notes: "orig"})
	before := e.row(t, id)
	var f1, f2, f3 bool
	err := e.in(t, e.wsA, func(tx pgx.Tx) (err error) {
		ctx := context.Background()
		if f1, err = leadswrite.UpdateStatus(ctx, tx, id, "dead"); err != nil {
			return
		}
		if f2, err = leadswrite.UpdateFields(ctx, tx, id, map[string]any{"notes": "pwn"}); err != nil {
			return
		}
		f3, err = leadswrite.Delete(ctx, tx, id)
		return
	})
	if err != nil || f1 || f2 || f3 {
		t.Fatalf("found=%v/%v/%v err=%v", f1, f2, f3, err)
	}
	after := e.row(t, id)
	for k, v := range before {
		if *after[k] != *v {
			t.Errorf("cross-tenant write changed %s", k)
		}
	}
}

// TestUpdateFieldsStoresWhatPythonStores applies every Python-generated vector
// through UpdateFields and compares the value Postgres stored.
func TestUpdateFieldsStoresWhatPythonStores(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, e.wsA, leadswrite.NewLead{Company: "Seed Co", City: "X", Notes: "orig"})
	for _, v := range loadVectors(t) {
		reset := map[string]any{"company": "Seed Co", "notes": "orig", "score": 42}
		if _, err := e.update(t, e.wsA, id, reset); err != nil {
			t.Fatal(err)
		}
		_, err := e.update(t, e.wsA, id, map[string]any{v.Column: body(t, v.Raw)})
		if !v.OK {
			var fe *leadswrite.FieldError
			if !errors.As(err, &fe) {
				t.Errorf("%s %s: want FieldError, got %v", v.Class, v.Raw, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s %s: %v", v.Class, v.Raw, err)
			continue
		}
		got := e.row(t, id)[v.Column]
		if (got == nil) != (v.Stored == nil) || (got != nil && *got != *v.Stored) {
			t.Errorf("%s %s: stored %v, Python %v", v.Class, v.Raw, got, v.Stored)
		}
	}
}
