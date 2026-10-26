//go:build integration

package autosvc_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
)

type vecFile struct {
	Workbooks []struct {
		ID          string          `json:"id"`
		WorkspaceID string          `json:"workspace_id"`
		Columns     json.RawMessage `json:"columns"`
	} `json:"workbooks"`
	Secrets map[string][]string `json:"secrets"`
	Vectors []struct {
		Name             string          `json:"name"`
		WorkspaceID      string          `json:"workspace_id"`
		TriggerType      *string         `json:"trigger_type"`
		TriggerConfig    json.RawMessage `json:"trigger_config"`
		Actions          json.RawMessage `json:"actions"`
		Condition        *string         `json:"condition"`
		ScopeWorkbookIDs []string        `json:"scope_workbook_ids"`
		AllowLegacy      bool            `json:"allow_legacy"`
		MaxActions       int             `json:"max_actions"`
		OK               bool            `json:"ok"`
		Error            *struct {
			Status  int    `json:"status"`
			Detail  string `json:"detail"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"vectors"`
}

// TestValidateRuleVectors runs every Python-recorded _validate_rule vector.
// Python crashes (500) are asserted as a 422 (D4).
func TestValidateRuleVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/validate_rule_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vecFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	ws := map[string]string{"ws-1": wsA, "ws-2": wsB}
	wbID := func(id string) string { return "p13-" + id }
	for _, wb := range vf.Workbooks {
		cols := string(wb.Columns)
		seedWorkbook(t, e, ws[wb.WorkspaceID], wbID(wb.ID), cols)
	}
	for vws, keys := range vf.Secrets {
		for _, k := range keys {
			val := "secret-value"
			if k == "EMPTY" {
				val = ""
			}
			inWS(t, e, ws[vws], func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1,$2,$3)`, ws[vws], k, val)
				return err
			})
		}
	}
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			in := autosvc.RuleInput{TriggerType: v.TriggerType, Enabled: true}
			if len(v.TriggerConfig) > 0 {
				c, err := autosvc.ParseJSON(v.TriggerConfig)
				if err != nil {
					t.Fatal(err)
				}
				in.TriggerConfig = c
			}
			if len(v.Actions) > 0 && string(v.Actions) != "null" {
				a, err := autosvc.ParseJSON(v.Actions)
				if err != nil {
					t.Fatal(err)
				}
				in.Actions = a.([]any)
			}
			if v.Condition != nil {
				in.Condition = *v.Condition
			}
			for _, id := range v.ScopeWorkbookIDs {
				in.ScopeWorkbookIDs = append(in.ScopeWorkbookIDs, wbID(id))
			}
			cfg := e.svc.Cfg
			cfg.MaxActionsPerRule = v.MaxActions
			cfg.AllowLegacyOutreach = v.AllowLegacy
			var verr error
			inWS(t, e, ws[v.WorkspaceID], func(tx pgx.Tx) error {
				verr = autosvc.ValidateRule(e.ctx, tx, ws[v.WorkspaceID], in, cfg)
				return nil
			})
			switch {
			case v.OK:
				if v.AllowLegacy && verr != nil {
					return
				}
				if verr != nil {
					t.Fatalf("want ok, got %v", verr)
				}
			case v.Error != nil && v.Error.Type != "":
				// Python crashed; Go answers a 4xx
				var he *autosvc.HTTPError
				if !asHTTP(verr, &he) || he.Status != 422 {
					t.Fatalf("python crash %s (%s): want 422, got %v", v.Error.Type, v.Error.Message, verr)
				}
			default:
				var he *autosvc.HTTPError
				if !asHTTP(verr, &he) {
					t.Fatalf("want %d %q, got %v", v.Error.Status, v.Error.Detail, verr)
				}
				if he.Status != v.Error.Status || he.Detail != v.Error.Detail {
					t.Fatalf("got %d %q\nwant %d %q", he.Status, he.Detail, v.Error.Status, v.Error.Detail)
				}
			}
		})
	}
}

func asHTTP(err error, target **autosvc.HTTPError) bool {
	for err != nil {
		if he, ok := err.(*autosvc.HTTPError); ok {
			*target = he
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestValidateRuleLegacyAllowedStillRefusesInGo pins the D2 behaviour: with
// the flag on, a well-formed sequencer action is refused (the outreach store
// is Python-only) rather than silently accepted.
func TestValidateRuleLegacyAllowedStillRefusesInGo(t *testing.T) {
	e := setup(t)
	in := ruleJSON(t, "on_row_added", "", `[{"type":"sequencer","config":{"sequence_id":"s1"}}]`, "")
	cfg := e.svc.Cfg
	cfg.AllowLegacyOutreach = true
	var verr error
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		verr = autosvc.ValidateRule(e.ctx, tx, wsA, in, cfg)
		return nil
	})
	httpErr(t, verr, 409, "legacy_outreach_unavailable")
}

// TestValidateWebhookAllowlist covers the exact/.suffix allow-list (no globs).
func TestValidateWebhookAllowlist(t *testing.T) {
	e := setup(t)
	cfg := e.svc.Cfg
	cfg.WebhookAllowlist = []string{"hooks.example.com", ".corp.example.org", "*.evil.com"}
	check := func(url string) error {
		in := ruleJSON(t, "on_row_added", "", `[{"type":"webhook","config":{"url":"`+url+`"}}]`, "")
		var verr error
		inWS(t, e, wsA, func(tx pgx.Tx) error {
			verr = autosvc.ValidateRule(e.ctx, tx, wsA, in, cfg)
			return nil
		})
		return verr
	}
	for _, ok := range []string{"https://hooks.example.com/x", "https://HOOKS.example.com:8443/x", "https://a.corp.example.org/x", "https://{host}/x"} {
		if err := check(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://example.com/x", "https://corp.example.org/x", "https://a.evil.com/x", "https://hooks.example.com.evil.net/x"} {
		err := check(bad)
		if err == nil || !strings.Contains(err.Error(), "not in webhook domain allowlist") {
			t.Errorf("%s: want allow-list refusal, got %v", bad, err)
		}
	}
}
