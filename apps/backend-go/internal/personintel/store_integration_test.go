//go:build integration

package personintel_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/personintel"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func tx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func profile(user string) personintel.Profile {
	p := personintel.NewProfile("https://www.linkedin.com/in/" + user)
	p.Username = user
	p.Name = "Jane <Doe> & Co"
	p.Headline = "VP"
	p.Summary = "sum"
	p.Emails = []string{"b@x.io", "a@x.io"}
	p.SocialLinks.Set("twitter", "https://twitter.com/z")
	p.SocialLinks.Set("github", "https://github.com/a")
	p.SocialLinks.Set("blog", "https://b")
	p.Articles = []personintel.Entry{{Title: "T", URL: "u", Snippet: "s", Source: "web"}}
	p.Skills = []string{"go", "sql"}
	p.RawSources = []personintel.Result{{Title: "t", URL: "https://x", Snippet: "é", Source: "duckduckgo", Category: "general"}}
	p.Status = "completed"
	return *p
}

func save(t *testing.T, pool *pgxpool.Pool, p personintel.Profile) int64 {
	t.Helper()
	var id int64
	tx(t, pool, func(x pgx.Tx) {
		var err error
		if id, err = personintel.Save(context.Background(), x, p); err != nil {
			t.Fatal(err)
		}
	})
	return id
}

func TestSaveRoundTripsJSONColumns(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	id := save(t, pool, profile("rt-"+time.Now().Format("150405.000000")))
	t.Cleanup(func() { tx(t, pool, func(x pgx.Tx) { _, _ = personintel.Delete(context.Background(), x, id) }) })
	tx(t, pool, func(x pgx.Tx) {
		d, err := personintel.Get(context.Background(), x, id)
		if err != nil || d == nil {
			t.Fatalf("%v %v", d, err)
		}
		b, err := d.JSON()
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, want := range []string{
			`"emails":["b@x.io","a@x.io"]`,
			`"social_links":{"twitter":"https://twitter.com/z","github":"https://github.com/a","blog":"https://b"}`,
			`"articles":[{"title":"T","url":"u","snippet":"s","source":"web"}]`,
			`"mentions":[]`, `"companies":[]`, `"education":[]`, `"skills":["go","sql"]`,
			`"raw_sources":[{"title":"t","url":"https://x","snippet":"é","source":"duckduckgo","category":"general"}]`,
			`"name":"Jane <Doe> & Co"`, `"status":"completed"`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in %s", want, s)
			}
		}
	})
}

func TestGetDetailKeyOrder(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	id := save(t, pool, profile("ko"))
	t.Cleanup(func() { tx(t, pool, func(x pgx.Tx) { _, _ = personintel.Delete(context.Background(), x, id) }) })
	tx(t, pool, func(x pgx.Tx) {
		d, _ := personintel.Get(context.Background(), x, id)
		b, _ := d.JSON()
		var keys []string
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.Token()
		for dec.More() {
			k, _ := dec.Token()
			keys = append(keys, k.(string))
			var skip json.RawMessage
			dec.Decode(&skip)
		}
		want := "id,linkedin_url,username,name,headline,location,summary,emails,social_links,articles,mentions,companies,education,skills,raw_sources,status,created_at"
		if strings.Join(keys, ",") != want {
			t.Fatalf("keys %v", keys)
		}
		// created_at: naive UTC isoformat (no zone suffix)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		ca := m["created_at"].(string)
		if strings.ContainsAny(ca[10:], "Z+") || ca[10] != 'T' {
			t.Fatalf("created_at %q", ca)
		}
		if ts, err := time.Parse("2006-01-02T15:04:05.999999", ca); err != nil || time.Since(ts) > time.Minute || time.Since(ts) < -time.Minute {
			t.Fatalf("created_at %q not now UTC: %v", ca, err)
		}
	})
}

func TestHistoryShapeAndOrder(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	a := save(t, pool, profile("h1"))
	b := save(t, pool, profile("h2"))
	c := save(t, pool, profile("h3"))
	t.Cleanup(func() {
		tx(t, pool, func(x pgx.Tx) {
			for _, id := range []int64{a, b, c} {
				_, _ = personintel.Delete(context.Background(), x, id)
			}
		})
	})
	tx(t, pool, func(x pgx.Tx) {
		rows, err := personintel.History(context.Background(), x, 0, 3)
		if err != nil || len(rows) != 3 || rows[0].ID != c || rows[1].ID != b || rows[2].ID != a {
			t.Fatalf("%v %v", rows, err)
		}
		off, _ := personintel.History(context.Background(), x, 1, 1)
		if len(off) != 1 || off[0].ID != b {
			t.Fatalf("offset %v", off)
		}
		raw, _ := json.Marshal(rows[0])
		if !strings.HasPrefix(string(raw), `{"id":`) || !strings.Contains(string(raw), `"email_count":2,"status":"completed","created_at":"`) ||
			!strings.Contains(string(raw), `"username":"h3","name":"Jane <Doe> & Co","headline":"VP","email_count"`) {
			t.Fatalf("shape %s", raw)
		}
	})
}

func TestDeleteMissingFalse(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	id := save(t, pool, profile("del"))
	tx(t, pool, func(x pgx.Tx) {
		ok, err := personintel.Delete(context.Background(), x, id)
		if !ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
		if ok, err := personintel.Delete(context.Background(), x, id); ok || err != nil {
			t.Fatalf("second delete %v %v", ok, err)
		}
		if d, err := personintel.Get(context.Background(), x, id); d != nil || err != nil {
			t.Fatalf("get missing %v %v", d, err)
		}
	})
}
