package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

func TestConfigDefaultsMatchPython(t *testing.T) {
	c, err := config.LoadMCPFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !c.RequireAuth || c.WriteEnabled || c.TokenTTLDays != 90 || c.MaxWritesPerDay != 0 ||
		c.AutomationsEnabled || c.AutomationsAllowLegacyOutreach || c.AutomationsMaxRulesPerWS != 50 ||
		c.SSEHost != "127.0.0.1" || c.MaxEnrichPerDay != 0 || c.ToolTimeoutSeconds != 120 ||
		c.StdioRevalidateSeconds != 30 || c.SSEAllowNonLoopback || c.Token.Reveal() != "" {
		t.Fatalf("defaults: %+v", c)
	}
	if _, err := config.LoadMCPFrom(func(k string) string {
		if k == "MCP_WRITE_ENABLED" {
			return "maybe"
		}
		return ""
	}); err == nil {
		t.Fatal("bad boolean accepted")
	}
}

func TestConfigTokenEnvFallback(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, _ := config.LoadMCPFrom(env(map[string]string{"YUPCHA_MCP_TOKEN": "legacy-tok", "YUPCHA_MCP_SSE_HOST": "10.0.0.1"}))
	if c.Token.Reveal() != "legacy-tok" || c.SSEHost != "10.0.0.1" {
		t.Fatalf("fallback: host=%q", c.SSEHost)
	}
	c, _ = config.LoadMCPFrom(env(map[string]string{"OPENGTM_MCP_TOKEN": "new-tok", "YUPCHA_MCP_TOKEN": "legacy-tok"}))
	if c.Token.Reveal() != "new-tok" {
		t.Fatal("OPENGTM_MCP_TOKEN must win")
	}
	if s := (struct{ C config.MCP }{c}); len(s.C.Token.Reveal()) == 0 {
		t.Fatal("token empty")
	}
}

func TestArgsHelpersReturnTypedErrors(t *testing.T) {
	a := Args{"i": float64(3), "frac": 1.5, "s": "x", "b": true, "n": nil, "ints": []any{float64(1), "z"}, "strs": []any{"a", 1.0}, "big": float64(1e300), "num": json.Number("7")}
	if v, ok, err := a.Int("i"); v != 3 || !ok || err != nil {
		t.Fatal(v, ok, err)
	}
	if v, ok, err := a.Int("num"); v != 7 || !ok || err != nil {
		t.Fatal(v, ok, err)
	}
	if _, ok, err := a.Int("n"); ok || err != nil {
		t.Fatal("null is absent")
	}
	if _, ok, err := a.Int("missing"); ok || err != nil {
		t.Fatal("missing is absent")
	}
	for _, k := range []string{"frac", "s", "b", "big"} {
		_, _, err := a.Int(k)
		var ae *ArgError
		if !errors.As(err, &ae) || ae.Field != k || ae.Want == "" {
			t.Fatalf("Int(%s): %v", k, err)
		}
	}
	if _, _, err := a.String("i"); err == nil {
		t.Fatal("String(number)")
	}
	if s, ok, err := a.String("s"); s != "x" || !ok || err != nil {
		t.Fatal(s, ok, err)
	}
	var ae *ArgError
	if _, _, err := a.IntList("ints"); !errors.As(err, &ae) || ae.Field != "ints" {
		t.Fatal(err)
	}
	if _, _, err := a.StringList("strs"); !errors.As(err, &ae) {
		t.Fatal(err)
	}
	if _, _, err := a.StringList("s"); !errors.As(err, &ae) {
		t.Fatal(err)
	}
	if l, ok, err := (Args{"x": []any{"a", "b"}}).StringList("x"); len(l) != 2 || !ok || err != nil {
		t.Fatal(l, ok, err)
	}
	var nilArgs Args
	if _, ok, err := nilArgs.Int("x"); ok || err != nil {
		t.Fatal("nil Args must not panic")
	}
}

func TestCtxHas(t *testing.T) {
	c := Ctx{Capabilities: map[string]bool{"leads:read": true}}
	if !c.Has("leads:read") || c.Has("leads:write") || (Ctx{}).Has("x") {
		t.Fatal("Has")
	}
}

func TestNilPortReturnsUnavailable(t *testing.T) {
	d := Deps{}
	ctx := context.Background()
	if _, err := d.LeadPort().ByID(ctx, nil, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := d.LeadPort().Stats(ctx, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := d.EnrichPort().VerifyEmail(ctx, "w", "a@b.c"); !IsUnavailable(err) {
		t.Fatal(err)
	}
	if _, err := d.EnrichPort().Py(ctx, "w", "p", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := d.WorkbookPort().Create(ctx, nil, "w", "n", "d", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := d.OutreachPort().SequenceExists(ctx, nil, "w", "s"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := d.AutomationPort().CountRules(ctx, nil, "w"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if he := d.AutomationPort().ValidateRule(ctx, nil, "w", RuleInput{}); he == nil || he.Status != 503 {
		t.Fatal(he)
	}
	if !IsUnavailable(pyworker.ErrUnavailable) || IsUnavailable(errors.New("x")) {
		t.Fatal("IsUnavailable")
	}
	if leadWriteTools(d) != nil ||
		workbookWriteTools(d) != nil || outreachWriteTools(d) != nil || automationWriteTools(d) != nil {
		t.Fatal("constructors must return nil in the scaffold")
	}
}

func TestRunInWorkspace(t *testing.T) {
	if err := (Deps{}).RunInWorkspace(context.Background(), "w", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	called := ""
	d := Deps{InWorkspace: func(ctx context.Context, ws string, fn func(pgx.Tx) error) error { called = ws; return fn(nil) }}
	if err := d.RunInWorkspace(context.Background(), "w1", func(pgx.Tx) error { return nil }); err != nil || called != "w1" {
		t.Fatal(err, called)
	}
	d = Deps{InWorkspace: nilTxRunner}
	if err := d.RunInWorkspace(context.Background(), "w", func(tx pgx.Tx) error {
		if tx != nil {
			t.Fatal("want nil tx")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
