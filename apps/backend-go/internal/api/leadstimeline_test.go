//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func (e leadsEnv) replayTimeline(t *testing.T, name, who, ws string) {
	t.Helper()
	fx := contract.Load(t, "timeline/"+name)
	rec := e.get(who, ws, fx.Request.Path)
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	if v := rec.Header().Get("Content-Type"); v != "" {
		got.Headers["Content-Type"] = v
	}
	contract.AssertEqual(t, got, fx.Response)
}

// Fixtures that need only the seeded leads (signal/event-backed pages are covered by leadtimeline's own Python replay).
func TestTimelineContract(t *testing.T) {
	e := newLeadsEnv(t)
	for _, n := range []string{"timeline_empty_lead", "timeline_limit_zero", "timeline_limit_201", "timeline_before_bad",
		"timeline_before_id_long", "timeline_foreign", "timeline_missing"} {
		t.Run(n, func(t *testing.T) { e.replayTimeline(t, n, "alice", "ws-alpha") })
	}
}

func TestTimelineLimitBounds422(t *testing.T) {
	e := newLeadsEnv(t)
	for _, q := range []string{"limit=0", "limit=201", "limit=-5", "limit=abc", "limit=99999999999999999999999"} {
		if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?"+q); rec.Code != 422 {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	for _, q := range []string{"limit=1", "limit=200"} {
		if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?"+q); rec.Code != 200 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}

func TestTimelineBeforeNotFloat422(t *testing.T) {
	e := newLeadsEnv(t)
	for _, q := range []string{"before=abc", "before=", "before=1,5", "before=0x10", "before=1__0", "before=_1"} {
		if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?"+q); rec.Code != 422 || !strings.Contains(rec.Body.String(), "float_parsing") {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	for _, q := range []string{"before=1e10", "before=+5", "before=.5", "before=5.", "before=1_000", "before=%201%20", "before=inf", "before=-Infinity"} {
		if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?"+q); rec.Code != 200 {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	// NaN orders against nothing: every item is filtered out (Python tuple comparison).
	rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?before=nan")
	if rec.Code != 200 || rec.Body.String() != `{"items":[],"next_before":null,"next_before_id":null}` {
		t.Errorf("nan: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTimelineBeforeIDTooLong422(t *testing.T) {
	e := newLeadsEnv(t)
	if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?before_id="+strings.Repeat("x", 256)); rec.Code != 422 {
		t.Errorf("256 chars: %d", rec.Code)
	}
	if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?before_id="+strings.Repeat("x", 255)); rec.Code != 200 {
		t.Errorf("255 chars: %d", rec.Code)
	}
	// max_length counts characters, not bytes
	if rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline?before_id="+strings.Repeat("%C3%A9", 255)); rec.Code != 200 {
		t.Errorf("255 two-byte chars: %d", rec.Code)
	}
}

func TestTimeline404(t *testing.T) {
	e := newLeadsEnv(t)
	for _, p := range []string{"9999", "0", "-1", "99999999999999999999"} {
		rec := e.get("alice", "ws-alpha", "/api/lead/"+p+"/timeline")
		if rec.Code != 404 || rec.Body.String() != `{"detail":"Lead not found"}` {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if rec := e.get("alice", "ws-alpha", "/api/lead/abc/timeline"); rec.Code != 422 {
		t.Errorf("abc: %d", rec.Code)
	}
	if rec := e.get("", "", "/api/lead/2/timeline"); rec.Code != 401 {
		t.Errorf("no auth: %d", rec.Code)
	}
	if rec := e.get("alice", "ws-beta", "/api/lead/2/timeline"); rec.Code != 403 {
		t.Errorf("non-member: %d", rec.Code)
	}
	// 404 is evaluated after validation: a bad limit on a missing lead is a 422.
	if rec := e.get("alice", "ws-alpha", "/api/lead/9999/timeline?limit=0"); rec.Code != 422 {
		t.Errorf("422 before 404: %d", rec.Code)
	}
}

func TestTimelineCrossWorkspace404(t *testing.T) {
	e := newLeadsEnv(t)
	e.replayTimeline(t, "timeline_foreign", "alice", "ws-alpha")
	if rec := e.get("bob", "ws-beta", "/api/lead/2/timeline"); rec.Code != 404 {
		t.Errorf("alpha lead as beta: %d", rec.Code)
	}
	if rec := e.get("bob", "ws-beta", "/api/lead/17/timeline"); rec.Code != 200 {
		t.Errorf("own lead as beta: %d", rec.Code)
	}
}

// Starlette JSONResponse uses ensure_ascii=False: raw non-ASCII, including U+2028/U+2029.
func TestTimelineRawNonASCII(t *testing.T) {
	e := newLeadsEnv(t)
	title := "café     \U0001F600 <&>"
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO signals (id, workspace_id, lead_id, signal_type, title, created_at) VALUES ('t11-uni','ws-alpha',2,'news',$1,1787900000.5)`, title)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM signals WHERE id='t11-uni'`)
			return err
		})
	})
	rec := e.get("alice", "ws-alpha", "/api/lead/2/timeline")
	var rr *httptest.ResponseRecorder = rec
	body := rr.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"title":"`+title+`"`) {
		t.Fatalf("%d %s", rec.Code, body)
	}
	if strings.Contains(body, `\u`) {
		t.Errorf("escape left in body: %s", body)
	}
}
