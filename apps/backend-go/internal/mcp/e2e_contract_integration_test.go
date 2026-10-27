//go:build integration

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type e2eIndex struct {
	Profiles map[string]struct {
		Flags map[string]any `json:"flags"`
		Cases []string       `json:"cases"`
	} `json:"profiles"`
	Tokens map[string]struct {
		UserID       int      `json:"user_id"`
		WorkspaceID  string   `json:"workspace_id"`
		Capabilities []string `json:"capabilities"`
		ExpiresAt    *string  `json:"expires_at"`
		RevokedAt    *string  `json:"revoked_at"`
	} `json:"tokens"`
}

func loadE2EIndex(t *testing.T) e2eIndex {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx e2eIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

// e2eEnv is the Python seed (mcp_seed.py) rebuilt in a real database with
// per-run unique ids, served through the PRODUCTION adapters and registry.
type e2eEnv struct {
	*authEnv
	srv   *Server
	cfg   Config
	plain map[string]string // token alias -> plaintext
	names map[string]string // fixture id (ws-alpha, wb-alpha-1) -> real id
	ids   map[int64]int64   // fixture lead id -> real id
	rev   map[int64]int64   // real lead id -> fixture id
}

var e2eRoles = map[int]string{1: "owner", 2: "editor", 3: "viewer", 6: "admin", 4: "owner", 5: "member"}

func newE2E(t *testing.T, flags map[string]any) *e2eEnv {
	t.Helper()
	a := newAuthEnv(t)
	idx := loadE2EIndex(t)
	e := &e2eEnv{authEnv: a, plain: map[string]string{}, names: map[string]string{}, ids: map[int64]int64{}, rev: map[int64]int64{}}
	ctx := context.Background()

	users := map[int]int32{}
	for n := 1; n <= 6; n++ {
		users[n] = a.user(t, fmt.Sprintf("u%d", n))
	}
	owner := map[string]int32{"ws-alpha": users[1], "ws-beta": users[4]}
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		o := owner[ws]
		e.names[ws] = a.workspace(t, "e2e-"+strings.TrimPrefix(ws, "ws-"), &o, "")
	}
	members := []struct {
		ws string
		u  int
	}{{"ws-alpha", 1}, {"ws-alpha", 2}, {"ws-alpha", 3}, {"ws-alpha", 6}, {"ws-beta", 4}, {"ws-beta", 5}}
	for _, m := range members {
		a.member(t, e.names[m.ws], users[m.u], e2eRoles[m.u])
	}

	parse := func(s *string) *time.Time {
		if s == nil {
			return nil
		}
		tm, err := time.Parse("2006-01-02 15:04:05-07", *s)
		if err != nil {
			t.Fatal(err)
		}
		return &tm
	}
	aliases := make([]string, 0, len(idx.Tokens))
	for k := range idx.Tokens {
		aliases = append(aliases, k)
	}
	sort.Strings(aliases)
	for i, alias := range aliases {
		tk := idx.Tokens[alias]
		e.plain[alias] = "ycp_seed_" + alias + "_" + a.sfx
		a.token(t, "e"+strconv.Itoa(i), e.plain[alias], e.names[tk.WorkspaceID], users[tk.UserID], tk.Capabilities, parse(tk.ExpiresAt), parse(tk.RevokedAt))
	}

	wss := [2]string{e.names["ws-alpha"], e.names["ws-beta"]}
	for _, l := range seedLeads {
		w := wss[l.ws]
		var id int64
		err := tenancy.InWorkspace(ctx, a.pool, w, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, website, email, phone, contact_person, specialization,
				company_size, status, score, score_tier, decision_makers, source, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'mcp-seed','2026-08-29T10:00:00','2026-08-29T10:00:00') RETURNING id`,
				w, l.company, l.city, l.website, l.email, l.phone, l.person, l.spec, l.sz, l.status, l.score, l.tier, l.dm).Scan(&id)
		})
		if err != nil {
			t.Fatal(err)
		}
		n := int64(len(e.ids) + 1)
		e.ids[n], e.rev[id] = id, n
	}
	for _, wb := range []struct{ id, ws, name, desc string }{
		{"wb-alpha-1", "ws-alpha", "Alpha Empty", "empty workbook"},
		{"wb-alpha-del", "ws-alpha", "Alpha Disposable", "deleted by the delete_workbook case"},
		{"wb-beta-1", "ws-beta", "Beta Workbook", "beta tenant"},
	} {
		real := wb.id + "-" + a.sfx[len(a.sfx)-8:]
		e.names[wb.id] = real
		err := tenancy.InWorkspace(ctx, a.pool, e.names[wb.ws], func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, description, workspace_id, source_type) VALUES ($1,$2,$3,$4,'manual')`,
				real, wb.name, wb.desc, e.names[wb.ws])
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, w := range wss {
			_ = tenancy.InWorkspace(ctx, a.pool, w, func(tx pgx.Tx) error {
				for _, q := range []string{`DELETE FROM leads`, `DELETE FROM workbooks`, `DELETE FROM mcp_audit_log`, `DELETE FROM trigger_cap_reservations`} {
					_, _ = tx.Exec(ctx, q)
				}
				return nil
			})
		}
	})

	flag := func(k string) bool { v, _ := flags[k].(bool); return v }
	num := func(k string) int { v, _ := flags[k].(float64); return int(v) }
	e.cfg = Config{
		RequireAuth:                    true,
		WriteEnabled:                   flag("MCP_WRITE_ENABLED"),
		MaxWritesPerDay:                num("MCP_MAX_WRITES_PER_DAY"),
		AutomationsEnabled:             flag("AUTOMATIONS_ENABLED"),
		AutomationsAllowLegacyOutreach: flag("AUTOMATIONS_ALLOW_LEGACY_OUTREACH"),
		AutomationsMaxRulesPerWS:       num("AUTOMATIONS_MAX_RULES_PER_WS"),
		TokenTTLDays:                   90,
	}
	verify := emailverify.NewDeps()
	verify.SMTPDisabled = true
	d := NewProductionDeps(e.cfg, a.pool, nil, verify) // no provider caller: provider tools answer "backend unavailable"
	e.srv = NewServer(d, BuildRegistry(d))
	e.d = d
	return e
}

var (
	uuidRe  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}`)
	keyRe   = regexp.MustCompile(`mcpw_[0-9a-f]{32}`)
	e2eIDRe = regexp.MustCompile(`"(id|lead_id)": (\d+)`)
)

func mask(s string) string {
	return keyRe.ReplaceAllString(uuidRe.ReplaceAllString(s, "<uuid>"), "<key>")
}

// toFixture rewrites a real response text into the recorded id space.
func (e *e2eEnv) toFixture(s string) string {
	for fx, real := range e.names {
		s = strings.ReplaceAll(s, real, fx)
	}
	s = e2eIDRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := e2eIDRe.FindStringSubmatch(m)
		n, _ := strconv.ParseInt(sub[2], 10, 64)
		if fx, ok := e.rev[n]; ok {
			return `"` + sub[1] + `": ` + strconv.FormatInt(fx, 10)
		}
		return m
	})
	return mask(s)
}

// learn pairs the ids in a real and a recorded text (in order) and records the
// mapping of ids created by the replay (fixture ids above the seed).
func (e *e2eEnv) learn(got, want string) {
	g, w := e2eIDRe.FindAllStringSubmatch(got, -1), e2eIDRe.FindAllStringSubmatch(want, -1)
	if len(g) != len(w) {
		return
	}
	for i := range g {
		r, _ := strconv.ParseInt(g[i][2], 10, 64)
		n, _ := strconv.ParseInt(w[i][2], 10, 64)
		if _, known := e.ids[n]; !known {
			if _, used := e.rev[r]; !used {
				e.ids[n], e.rev[r] = r, n
			}
		}
	}
}

// fwd maps a recorded request into this run's ids.
func (e *e2eEnv) fwd(req map[string]any) {
	params, _ := req["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if v, ok := args["lead_id"].(float64); ok {
		if real, ok := e.ids[int64(v)]; ok {
			args["lead_id"] = real
		}
	}
	for _, k := range []string{"workbook_id", "sequence_id"} {
		if v, ok := args[k].(string); ok {
			if real, ok := e.names[v]; ok {
				args[k] = real
			}
		}
	}
}

type e2eToolFixture struct {
	Token     string         `json:"token"`
	Request   map[string]any `json:"request"`
	Response  map[string]any `json:"response"`
	Stubs     map[string]any `json:"stubs"`
	Deviation string         `json:"deviation"`
}

func canonicalJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func envelopeText(m map[string]any) (string, bool) {
	res, _ := m["result"].(map[string]any)
	cs, _ := res["content"].([]any)
	if len(cs) != 1 {
		return "", false
	}
	c, _ := cs[0].(map[string]any)
	s, ok := c["text"].(string)
	return s, ok
}

func setEnvelopeText(m map[string]any, s string) {
	res := m["result"].(map[string]any)
	res["content"].([]any)[0].(map[string]any)["text"] = s
}

// Tools whose adapters (plans 08/09) are not part of this build.
var portlessTools = map[string]bool{"enroll_leads": true, "send_email": true, "create_automation": true}

// TestEveryRecordedFixtureReplaysByteForByte replays every recorded rpc/ and
// tools/ fixture, profile by profile in recording order, through Server.Handle
// built by BuildRegistry over the production adapters. Cases that cannot match
// by design are counted, not hidden: Go-deviation cases (asserted by their own
// tests), enrichment cases that need provider stubs and tools whose adapters land
// with plans 08/09 (must answer "not available in this build" or match). score_lead
// is scored in Go (leadscore) and compared.
func TestEveryRecordedFixtureReplaysByteForByte(t *testing.T) {
	idx := loadE2EIndex(t)
	profiles := make([]string, 0, len(idx.Profiles))
	for p := range idx.Profiles {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	var compared, skipped, portless int
	for _, p := range profiles {
		prof := idx.Profiles[p]
		t.Run(p, func(t *testing.T) {
			e := newE2E(t, prof.Flags)
			for _, c := range prof.Cases {
				if !strings.HasPrefix(c, "rpc/") && !strings.HasPrefix(c, "tools/") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(fixtureRoot, c))
				if err != nil {
					t.Fatal(err)
				}
				var fx e2eToolFixture
				if err := json.Unmarshal(raw, &fx); err != nil {
					t.Fatal(err)
				}
				params, _ := fx.Request["params"].(map[string]any)
				tool, _ := params["name"].(string)
				if fx.Deviation != "" || len(fx.Stubs) > 0 {
					skipped++
					continue
				}
				cx, err := Resolve(context.Background(), e.d, e.plain[fx.Token])
				if err != nil {
					t.Fatalf("%s: resolve %q: %v", c, fx.Token, err)
				}
				e.fwd(fx.Request)
				body, _ := json.Marshal(fx.Request)
				resp, respond := e.srv.Handle(context.Background(), cx, body)
				if fx.Response == nil {
					if respond {
						t.Errorf("%s: notification answered: %s", c, resp)
					}
					compared++
					continue
				}
				if !respond {
					t.Errorf("%s: no response", c)
					continue
				}
				var got map[string]any
				if err := json.Unmarshal(resp, &got); err != nil {
					t.Fatalf("%s: %v (%s)", c, err, resp)
				}
				gt, gok := envelopeText(got)
				wt, wok := envelopeText(fx.Response)
				if gok != wok {
					t.Errorf("%s: shape\n got %s\nwant %v", c, resp, fx.Response)
					continue
				}
				if gok {
					if portlessTools[tool] && strings.Contains(gt, "not available in this build") {
						portless++
						continue
					}
					e.learn(gt, wt)
					gt, wt = e.toFixture(gt), mask(wt)
					if tool == "get_pipeline_stats" {
						// Documented deviation: Python's bucket order is GROUP BY hash
						// order; Go sorts (README, "MCP read tools"). Buckets compare as maps.
						gt, wt = canonicalJSON(gt), canonicalJSON(wt)
					}
					setEnvelopeText(got, gt)
					setEnvelopeText(fx.Response, wt)
					if _, isErr := resultError(wt); isErr { // F7: Python recorded isError false for tool errors
						if res, ok := fx.Response["result"].(map[string]any); ok {
							res["isError"] = true
						}
					}
				}
				if !reflect.DeepEqual(got, fx.Response) {
					t.Errorf("%s:\n got %v\nwant %v", c, got, fx.Response)
					continue
				}
				compared++
			}
		})
	}
	t.Logf("compared=%d skipped(deviation/stubs)=%d portless=%d", compared, skipped, portless)
	if compared < 100 {
		t.Fatalf("only %d fixtures compared; replay is vacuous", compared)
	}
}

// TestSSEEndToEndAgainstFixtures serves the production handler on a real
// loopback listener and replays the recorded auth/ and sse/ HTTP cases.
func TestSSEEndToEndAgainstFixtures(t *testing.T) {
	idx := loadE2EIndex(t)
	e := newE2E(t, idx.Profiles["read"].Flags)
	ts := httptest.NewServer(SSEHandler(e.srv, e.d))
	defer ts.Close()
	if !strings.HasPrefix(ts.URL, "http://127.0.0.1:") {
		t.Fatalf("listener %s is not loopback", ts.URL)
	}

	var files []string
	for _, c := range idx.Profiles["read"].Cases {
		if strings.HasPrefix(c, "auth/") || strings.HasPrefix(c, "sse/") {
			files = append(files, c)
		}
	}
	if len(files) < 20 {
		t.Fatalf("fixtures missing: %d", len(files))
	}
	client := &http.Client{Timeout: 20 * time.Second}
	for _, c := range files {
		t.Run(c, func(t *testing.T) {
			b, _ := os.ReadFile(filepath.Join(fixtureRoot, c))
			var fx sseFixture
			if err := json.Unmarshal(b, &fx); err != nil {
				t.Fatal(err)
			}
			var body io.Reader
			if s := string(fx.Request.Body); s != "null" && s != "" {
				var req map[string]any
				if json.Unmarshal(fx.Request.Body, &req) == nil {
					e.fwd(req)
					nb, _ := json.Marshal(req)
					body = bytes.NewReader(nb)
				} else {
					body = strings.NewReader(s)
				}
			}
			r, err := http.NewRequest(fx.Request.Method, ts.URL+fx.Request.Path, body)
			if err != nil {
				t.Fatal(err)
			}
			if _, has := fx.Request.Headers["Authorization"]; has {
				scheme := "Bearer "
				switch {
				case strings.Contains(c, "basic_scheme"):
					scheme = "Basic "
				case strings.Contains(c, "lowercase_scheme"):
					scheme = "bearer "
				}
				tok := ""
				switch {
				case strings.Contains(c, "bearer_empty"):
				case strings.Contains(c, "invalid_token"):
					tok = "ycp_not_a_real_token"
				case fx.Token != nil:
					tok = e.plain[*fx.Token]
				}
				r.Header.Set("Authorization", scheme+tok)
			}
			if body != nil {
				r.Header.Set("Content-Type", "application/json")
			}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			gotBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != fx.Response.Status {
				t.Fatalf("status %d want %d body %s", resp.StatusCode, fx.Response.Status, gotBody)
			}
			for k, v := range fx.Response.Headers {
				if got := resp.Header.Get(k); got != v {
					t.Fatalf("header %s = %q want %q", k, got, v)
				}
			}
			var want any
			if err := json.Unmarshal(fx.Response.Body, &want); err != nil {
				t.Fatal(err)
			}
			if s, isStr := want.(string); isStr {
				if string(gotBody) != s {
					t.Fatalf("body %q want %q", gotBody, s)
				}
				return
			}
			var got any
			if err := json.Unmarshal(gotBody, &got); err != nil {
				t.Fatalf("body %q: %v", gotBody, err)
			}
			if gm, ok := got.(map[string]any); ok {
				if gt, ok := envelopeText(gm); ok {
					wm := want.(map[string]any)
					wt, _ := envelopeText(wm)
					e.learn(gt, wt)
					setEnvelopeText(gm, e.toFixture(gt))
					setEnvelopeText(wm, mask(wt))
				}
			}
			markToolError(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("body\n got %v\nwant %v", got, want)
			}
		})
	}
}

// markToolError sets result.isError on a recorded tools/call envelope whose text is an error
// object: Python recorded false, Go follows the MCP spec (F7).
func markToolError(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if txt, ok := envelopeText(m); ok {
		if _, isErr := resultError(txt); isErr {
			if res, ok := m["result"].(map[string]any); ok {
				res["isError"] = true
			}
		}
	}
}
