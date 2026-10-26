//go:build integration

package poller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	jcWS      = "p08t12-a"
	jcOtherWS = "p08t12-b"
	jcOffset  = 7_000_000 // keeps the vectors' lead ids clear of other tests' rows
)

// TestMaterializeContactsVectors runs the recorded materialize_contacts vectors
// against real lead rows read through the tenant (RLS), including a lead that
// belongs to another workspace.
func TestMaterializeContactsVectors(t *testing.T) {
	pool := testpg.NewAppPool(t, 10)
	ctx := context.Background()
	clean := func() {
		for _, ws := range []string{jcWS, jcOtherWS} {
			_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM leads WHERE id BETWEEN $1 AND $2`, jcOffset, jcOffset+1000)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)

	seed := []struct {
		id         int
		ws         string
		company    string
		person, li *string
	}{
		{14, jcWS, "Hot Pursuit Labs", sp("Sam Lee"), sp("https://linkedin.com/in/sam-lee")},
		{15, jcWS, "Quantum Foods", sp(""), sp("https://linkedin.com/company/quantum")},
		{16, jcOtherWS, "Foreign Co", sp("Mallory"), sp("")},
		{17, jcWS, "No Person", nil, sp("https://linkedin.com/in/handle-only")},
	}
	for _, s := range seed {
		if err := tenancy.InWorkspace(ctx, pool, s.ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO leads (id, workspace_id, company, contact_person, linkedin_url) VALUES ($1,$2,$3,$4,$5)`,
				jcOffset+s.id, s.ws, s.company, s.person, s.li)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	var v struct {
		Cases []struct {
			Name     string           `json:"name"`
			Config   map[string]any   `json:"config"`
			Contacts []map[string]any `json:"contacts"`
		} `json:"materialize_contacts"`
	}
	raw, err := readFile("testdata/job_change_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Cases) != 13 {
		t.Fatalf("vectors: %v (%d)", err, len(v.Cases))
	}
	shift := func(x any) any {
		switch n := x.(type) {
		case float64:
			if n != 0 {
				return n + jcOffset
			}
		case string:
			if i, err := strconv.Atoi(n); err == nil && i != 0 {
				return strconv.Itoa(i + jcOffset)
			}
		}
		return x
	}
	for _, c := range v.Cases {
		cfg := map[string]any{}
		if c.Config != nil {
			if cs, ok := c.Config["contacts"].([]any); ok {
				out := []any{}
				for _, e := range cs {
					if m, ok := e.(map[string]any); ok {
						m2 := map[string]any{}
						for k, val := range m {
							m2[k] = val
						}
						if lv, ok := m2["lead_id"]; ok {
							m2["lead_id"] = shift(lv)
						}
						out = append(out, m2)
					} else {
						out = append(out, e)
					}
				}
				cfg["contacts"] = out
			}
		}
		var got []poller.Contact
		err := tenancy.InWorkspace(ctx, pool, jcWS, func(tx pgx.Tx) error {
			var err error
			got, err = poller.MaterializeContacts(ctx, tx, poller.PyWatch{WorkspaceID: jcWS, Config: cfg})
			return err
		})
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		gotJSON := []any{}
		for _, ct := range got {
			var lead any
			if ct.LeadID != nil {
				lead = float64(*ct.LeadID)
			}
			gotJSON = append(gotJSON, map[string]any{"lead_id": lead, "name": ct.Name, "company": ct.Company, "linkedin_url": ct.LinkedInURL, "key": ct.Key})
		}
		want := []any{}
		for _, ct := range c.Contacts {
			ct2 := map[string]any{}
			for k, val := range ct {
				ct2[k] = val
			}
			if id, ok := ct2["lead_id"].(float64); ok {
				ct2["lead_id"] = id + jcOffset
				ct2["key"] = fmt.Sprintf("lead:%d", int(id)+jcOffset)
			}
			want = append(want, ct2)
		}
		if !reflect.DeepEqual(gotJSON, want) {
			g, _ := json.Marshal(gotJSON)
			w, _ := json.Marshal(want)
			t.Errorf("%s\n got %s\nwant %s", c.Name, g, w)
		}
	}
}

// A lead_id that is not an integer or does not fit the column is skipped (Python
// would raise); the other entries still resolve.
func TestMaterializeContactsHardening(t *testing.T) {
	pool := testpg.NewAppPool(t, 10)
	ctx := context.Background()
	cfg := map[string]any{"contacts": []any{
		map[string]any{"lead_id": "abc"}, map[string]any{"lead_id": []any{1}}, map[string]any{"lead_id": 1e30},
		map[string]any{"lead_id": float64(1 << 40)}, map[string]any{"name": "Ok"},
	}}
	var got []poller.Contact
	if err := tenancy.InWorkspace(ctx, pool, jcWS, func(tx pgx.Tx) error {
		var err error
		got, err = poller.MaterializeContacts(ctx, tx, poller.PyWatch{WorkspaceID: jcWS, Config: cfg})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Ok" || !strings.HasPrefix(got[0].Key, "nm:ok") {
		t.Errorf("got %+v", got)
	}
}
