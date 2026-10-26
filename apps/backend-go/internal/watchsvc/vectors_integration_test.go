//go:build integration

package watchsvc_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/watchsvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type vErr struct {
	Status  int    `json:"status"`
	Detail  string `json:"detail"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

type createVec struct {
	Name         string                       `json:"name"`
	Body         map[string]json.RawMessage   `json:"body"`
	Flags        map[string]json.RawMessage   `json:"flags"`
	Preexisting  []map[string]any             `json:"preexisting"`
	Response     map[string]json.RawMessage   `json:"response"`
	Error        *vErr                        `json:"error"`
	RulesCreated []map[string]json.RawMessage `json:"rules_created"`
	PydanticErr  string                       `json:"pydantic_error"`
}

type patchVec struct {
	Name     string                     `json:"name"`
	Body     map[string]json.RawMessage `json:"body"`
	Watch    map[string]json.RawMessage `json:"watch"`
	Response map[string]json.RawMessage `json:"response"`
	Error    *vErr                      `json:"error"`
	Mirror   bool                       `json:"mirror_enabled"`
	MirrorN  bool                       `json:"mirror_next_poll_set"`
}

func flagBool(f map[string]json.RawMessage, k string, def bool) bool {
	if v, ok := f[k]; ok {
		var b bool
		_ = json.Unmarshal(v, &b)
		return b
	}
	return def
}

func flagInt(f map[string]json.RawMessage, k string, def int) int {
	if v, ok := f[k]; ok {
		var n int
		_ = json.Unmarshal(v, &n)
		return n
	}
	return def
}

func decodeCreate(t *testing.T, body map[string]json.RawMessage, lead *int32) watchsvc.CreateInput {
	t.Helper()
	var in watchsvc.CreateInput
	str := func(k string) *string {
		if v, ok := body[k]; ok && string(v) != "null" {
			var s string
			_ = json.Unmarshal(v, &s)
			return &s
		}
		return nil
	}
	if s := str("kind"); s != nil {
		in.Kind = *s
	}
	if s := str("target"); s != nil {
		in.Target = *s
	}
	in.Interval = str("interval")
	in.WebhookURL = str("webhook_url")
	in.WebhookSecretRef = str("webhook_secret_ref")
	if v, ok := body["create_webhook_rule"]; ok {
		_ = json.Unmarshal(v, &in.CreateWebhookRule)
	}
	if v, ok := body["signal_types"]; ok && string(v) != "null" {
		_ = json.Unmarshal(v, &in.SignalTypes)
	}
	if v, ok := body["config"]; ok && string(v) != "null" {
		c, err := parseJSON(v)
		if err != nil {
			t.Fatal(err)
		}
		in.Config = c
	}
	in.LeadID = lead
	return in
}

func parseJSON(raw []byte) (any, error) {
	o, err := wbcrud.ParseObjectCanon(append(append([]byte(`{"v":`), raw...), '}'))
	if err != nil {
		return nil, err
	}
	v, _ := o.Get("v")
	return v, nil
}

func normalized(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	v, err := parseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func checkVecErr(t *testing.T, got error, want *vErr) {
	t.Helper()
	he, ok := got.(*watchsvc.HTTPError)
	if !ok {
		t.Fatalf("want %+v, got %v", want, got)
	}
	if want.Type != "" {
		// Python crashed with a DB IntegrityError (duplicate); Go answers 409
		if he.Status != 409 {
			t.Fatalf("python crash %s: want 409, got %d %v", want.Type, he.Status, he.Detail)
		}
		return
	}
	if he.Status != want.Status || he.Detail != want.Detail {
		t.Fatalf("got %d %q, want %d %q", he.Status, he.Detail, want.Status, want.Detail)
	}
}

// TestWatchValidateVectorsCreate replays every Python-recorded create vector
// against the service and the database.
func TestWatchValidateVectorsCreate(t *testing.T) {
	raw, err := os.ReadFile("testdata/validate_watch_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Create []createVec `json:"create"`
		Patch  []patchVec  `json:"patch"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1,'HOOK','x')`, wsA)
		return err
	})
	leadA := int32(seedLead(t, e, wsA))
	for _, v := range f.Create {
		if v.PydanticErr != "" {
			continue // request-model validation belongs to the route layer
		}
		t.Run(v.Name, func(t *testing.T) {
			inWS(t, e, wsA, func(tx pgx.Tx) error {
				for _, q := range []string{`DELETE FROM watch_subscriptions`, `DELETE FROM triggers`} {
					if _, err := tx.Exec(e.ctx, q); err != nil {
						return err
					}
				}
				return nil
			})
			_, _ = e.pool.Exec(e.ctx, `DELETE FROM jobs WHERE workspace_id=$1`, wsA)
			inWS(t, e, wsB, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `DELETE FROM watch_subscriptions`)
				return err
			})
			for _, p := range v.Preexisting {
				ws := wsA
				if w, ok := p["workspace_id"].(string); ok && w == "ws-2" {
					ws = wsB
				}
				insertWatch(t, e, ws, p["id"].(string), "funding", p["target"].(string), true, 0, nil, nil)
			}
			cfg := baseCfg()
			if v.Flags != nil {
				if s, ok := v.Flags["INTENT_POLLER_DEFAULT_INTERVAL"]; ok {
					_ = json.Unmarshal(s, &cfg.DefaultInterval)
				}
				cfg.JobChangeMaxContacts = flagInt(v.Flags, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS", cfg.JobChangeMaxContacts)
				cfg.MaxWatchesPerWS = flagInt(v.Flags, "INTENT_POLLER_MAX_WATCHES_PER_WS", cfg.MaxWatchesPerWS)
				cfg.AutomationsEnabled = flagBool(v.Flags, "AUTOMATIONS_ENABLED", true)
				if he := watchsvc.RequireEnabled(flagBool(v.Flags, "INTENT_POLLER_ENABLED", true), flagBool(v.Flags, "PG_LEAD_STORE", true)); he != nil {
					checkVecErr(t, he, v.Error)
					return
				}
			}
			e.svc.Cfg = cfg
			var lead *int32
			if string(v.Body["lead_id"]) == "14" {
				lead = &leadA
			}
			c, err := e.svc.Create(e.ctx, wsA, decodeCreate(t, v.Body, lead))
			if v.Error != nil {
				checkVecErr(t, err, v.Error)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := c.MarshalJSON()
			got, _ := wbcrud.ParseObject(b)
			for k, want := range v.Response {
				if k == "has_webhook_rule_id" || k == "workspace_id" {
					continue // the recorded harness used its own ids
				}
				g, ok := got.Get(k)
				if !ok {
					t.Fatalf("response lacks %s", k)
				}
				gb, _ := json.Marshal(g)
				if k == "lead_id" && lead != nil {
					if string(gb) != itoa(int(leadA)) {
						t.Fatalf("lead_id %s", gb)
					}
					continue
				}
				if normalized(t, gb) != normalized(t, want) {
					t.Fatalf("%s: got %s want %s", k, gb, want)
				}
			}
			// scheduling side effects
			if next, _ := got.Get("next_poll_at"); next == nil {
				t.Fatal("next_poll_at must be set by the create bootstrap")
			}
			var jobs int
			_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM jobs WHERE workspace_id=$1 AND type='watch_poll'`, wsA).Scan(&jobs)
			if jobs != 1 {
				t.Fatalf("first poll jobs %d", jobs)
			}
			// rules created
			var rules []map[string]string
			inWS(t, e, wsA, func(tx pgx.Tx) error {
				rows, err := tx.Query(e.ctx, `SELECT name, trigger_type, trigger_config::text, actions::text, enabled::text, scope_workbook_ids::text FROM triggers ORDER BY created_at`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var n, tt, tc, a, en, sc string
					if err := rows.Scan(&n, &tt, &tc, &a, &en, &sc); err != nil {
						return err
					}
					rules = append(rules, map[string]string{"name": n, "trigger_type": tt, "trigger_config": tc, "actions": a, "enabled": en, "scope_workbook_ids": sc})
				}
				return rows.Err()
			})
			if len(rules) != len(v.RulesCreated) {
				t.Fatalf("rules %d want %d", len(rules), len(v.RulesCreated))
			}
			for i, want := range v.RulesCreated {
				for k, wv := range want {
					gv := rules[i][k]
					if k == "enabled" {
						gv = map[string]string{"true": "true", "false": "false"}[gv]
					}
					if k == "name" {
						var wn string
						_ = json.Unmarshal(wv, &wn)
						if gv != wn {
							t.Fatalf("rule name %q want %q", gv, wn)
						}
						continue
					}
					if k == "trigger_type" {
						var wn string
						_ = json.Unmarshal(wv, &wn)
						if gv != wn {
							t.Fatalf("trigger_type %q", gv)
						}
						continue
					}
					if normalized(t, json.RawMessage(gv)) != normalized(t, wv) {
						t.Fatalf("rule %s: got %s want %s", k, gv, wv)
					}
				}
			}
		})
	}
}

// TestWatchValidateVectorsPatch replays every Python-recorded patch vector.
func TestWatchValidateVectorsPatch(t *testing.T) {
	raw, _ := os.ReadFile("testdata/validate_watch_vectors.json")
	var f struct {
		Patch []patchVec `json:"patch"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	for _, v := range f.Patch {
		t.Run(v.Name, func(t *testing.T) {
			switch v.Name {
			case "poller_disabled":
				checkVecErr(t, watchsvc.RequireEnabled(false, true), v.Error)
				return
			case "pg_required":
				checkVecErr(t, watchsvc.RequireEnabled(true, false), v.Error)
				return
			case "unknown_watch_404":
				_, err := e.svc.Patch(e.ctx, wsA, "nope", watchsvc.PatchInput{})
				checkVecErr(t, err, v.Error)
				return
			}
			inWS(t, e, wsA, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `DELETE FROM watch_subscriptions`)
				return err
			})
			_, _ = e.pool.Exec(e.ctx, `DELETE FROM jobs WHERE workspace_id=$1`, wsA)
			_, _ = e.pool.Exec(e.ctx, `DELETE FROM watch_schedules WHERE workspace_id=$1`, wsA)
			w := v.Watch
			str := func(k, def string) string {
				if x, ok := w[k]; ok {
					var s string
					_ = json.Unmarshal(x, &s)
					return s
				}
				return def
			}
			raw := func(k, def string) string {
				if x, ok := w[k]; ok {
					return string(x)
				}
				return def
			}
			var failures int
			_ = json.Unmarshal([]byte(raw("consecutive_failures", "0")), &failures)
			var lastErr *string
			_ = json.Unmarshal([]byte(raw("last_error", "null")), &lastErr)
			var enabled bool
			_ = json.Unmarshal([]byte(raw("enabled", "true")), &enabled)
			var nextPoll *time.Time
			_ = json.Unmarshal([]byte(raw("next_poll_at", "null")), &nextPoll)
			inWS(t, e, wsA, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, signal_types, "interval", schedule_anchor, enabled, next_poll_at, cursor, consecutive_failures, last_error, config)
					VALUES ('w-1',$1,$2,$3,$4::json,$5,$6,$7,$8,'{"bootstrapped": false}'::json,$9,$10,$11::json)`,
					wsA, str("kind", "funding"), str("target", "Acme"), raw("signal_types", `["company_funded"]`), str("interval", "daily"),
					fixedNow.Add(-24*time.Hour), enabled, nextPoll, failures, lastErr, raw("config", "null"))
				return err
			})
			if _, err := e.pool.Exec(e.ctx, `INSERT INTO watch_schedules (watch_id, workspace_id, next_poll_at, enabled) VALUES ('w-1',$1,NULL,true)`, wsA); err != nil {
				t.Fatal(err)
			}
			var pin watchsvc.PatchInput
			for k, x := range v.Body {
				if string(x) == "null" {
					continue
				}
				switch k {
				case "enabled":
					var b bool
					_ = json.Unmarshal(x, &b)
					pin.Enabled = &b
				case "interval":
					var s string
					_ = json.Unmarshal(x, &s)
					pin.Interval = &s
				case "signal_types":
					var s []string
					_ = json.Unmarshal(x, &s)
					pin.SignalTypes = &s
				case "lead_id":
					var n int32
					_ = json.Unmarshal(x, &n)
					pin.LeadID = &n
				case "config":
					c, err := parseJSON(x)
					if err != nil {
						t.Fatal(err)
					}
					pin.Config = c
				}
			}
			e.svc.Cfg = baseCfg()
			e.svc.Cfg.JobChangeMaxContacts = 2
			var leadSeeded int32
			if pin.LeadID != nil {
				leadSeeded = int32(seedLead(t, e, wsA)) // lead_id vectors pin a lead of this workspace
				want := *pin.LeadID
				pin.LeadID = &leadSeeded
				_ = want
			}
			got, err := e.svc.Patch(e.ctx, wsA, "w-1", pin)
			if v.Error != nil {
				checkVecErr(t, err, v.Error)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := got.MarshalJSON()
			o, _ := wbcrud.ParseObject(b)
			for k, want := range v.Response {
				if k == "id" || k == "next_poll_at" || k == "created_at" || k == "workspace_id" {
					continue
				}
				g, _ := o.Get(k)
				gb, _ := json.Marshal(g)
				if k == "lead_id" && pin.LeadID != nil {
					if string(gb) != itoa(int(leadSeeded)) {
						t.Fatalf("lead_id %s", gb)
					}
					continue
				}
				if normalized(t, gb) != normalized(t, want) {
					t.Fatalf("%s: got %s want %s", k, gb, want)
				}
			}
			var men bool
			var mnext *time.Time
			if err := e.pool.QueryRow(e.ctx, `SELECT enabled, next_poll_at FROM watch_schedules WHERE watch_id='w-1'`).Scan(&men, &mnext); err != nil {
				t.Fatal(err)
			}
			if men != v.Mirror || (mnext != nil) != v.MirrorN {
				t.Fatalf("mirror enabled=%v next=%v, want enabled=%v next-set=%v", men, mnext, v.Mirror, v.MirrorN)
			}
		})
	}
}
