package outreachcfg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

type fakeStore struct {
	app map[string]string
	ws  map[string]string // "ws/key" -> ciphertext
}

func (s *fakeStore) AppSetting(_ context.Context, key string) (string, error) { return s.app[key], nil }
func (s *fakeStore) WorkspaceSetting(_ context.Context, ws, key string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+key]
	return v, ok, nil
}
func (s *fakeStore) SetWorkspaceSetting(_ context.Context, ws, key, value string) error {
	s.ws[ws+"/"+key] = value
	return nil
}

type errStore struct{ fakeStore }

func (errStore) AppSetting(context.Context, string) (string, error) { return "", errors.New("db down") }

func newRes(t *testing.T, st secrets.Store, env map[string]string) *secrets.Resolver {
	t.Helper()
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "cfg-test", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.NewResolver(e, st, func(k string) string { return env[k] })
}

func newStore() *fakeStore { return &fakeStore{app: map[string]string{}, ws: map[string]string{}} }

func set(t *testing.T, r *secrets.Resolver, ws, k, v string) {
	t.Helper()
	if err := r.Set(context.Background(), ws, k, v); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSMTPDefaults(t *testing.T) {
	r := newRes(t, newStore(), map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_EMAIL": "a@b.co", "SMTP_PASSWORD": "pw"})
	c := LoadSMTP(context.Background(), r, "w1")
	if c.Host != "smtp.example.com" || c.Port != 587 || c.FromName != "OpenGTM" || !c.UseTLS || c.MaxPerHour != 50 {
		t.Fatalf("defaults: %+v", c)
	}
	if !c.Configured() {
		t.Fatal("want configured")
	}
	if (SMTP{}).Configured() {
		t.Fatal("zero must not be configured")
	}
	r2 := newRes(t, newStore(), map[string]string{"SMTP_USE_TLS": "0"})
	if LoadSMTP(context.Background(), r2, "w1").UseTLS {
		t.Fatal("SMTP_USE_TLS=0 must disable TLS")
	}
}

func TestLoadSMTPWorkspaceBeatsGlobal(t *testing.T) {
	st := newStore()
	r := newRes(t, st, map[string]string{"SMTP_HOST": "global.example.com", "SMTP_PORT": "25"})
	set(t, r, "w1", "SMTP_HOST", "ws.example.com")
	set(t, r, "w1", "SMTP_PORT", "2525")
	c := LoadSMTP(context.Background(), r, "w1")
	if c.Host != "ws.example.com" || c.Port != 2525 {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadSMTPBadPortYieldsZeroConfig(t *testing.T) {
	r := newRes(t, newStore(), map[string]string{"SMTP_HOST": "h", "SMTP_PORT": "abc"})
	if c := LoadSMTP(context.Background(), r, "w1"); c != (SMTP{}) {
		t.Fatalf("want zero, got %+v", c)
	}
	r = newRes(t, newStore(), map[string]string{"SMTP_HOST": "h", "SMTP_MAX_PER_HOUR": "x"})
	if c := LoadSMTP(context.Background(), r, "w1"); c != (SMTP{}) {
		t.Fatalf("want zero, got %+v", c)
	}
	r = newRes(t, &errStore{*newStore()}, nil)
	if c := LoadSMTP(context.Background(), r, "w1"); c != (SMTP{}) {
		t.Fatalf("store error: want zero, got %+v", c)
	}
}

func TestLoadIMAPFallsBackToSMTPCreds(t *testing.T) {
	r := newRes(t, newStore(), map[string]string{"IMAP_HOST": "imap.example.com", "SMTP_EMAIL": "me@x.co", "SMTP_PASSWORD": "pw"})
	c := LoadIMAP(context.Background(), r, "w1")
	if c.User != "me@x.co" || c.Password.Reveal() != "pw" || c.Folder != "INBOX" || c.Port != 993 || !c.UseSSL || !c.Configured() {
		t.Fatalf("got %+v", c)
	}
	r = newRes(t, newStore(), map[string]string{"IMAP_HOST": "h", "IMAP_USER": "u", "IMAP_PASSWORD": "p", "IMAP_FOLDER": "Bounces", "IMAP_USE_SSL": "0", "IMAP_PORT": "143"})
	c = LoadIMAP(context.Background(), r, "w1")
	if c.User != "u" || c.Password.Reveal() != "p" || c.Folder != "Bounces" || c.UseSSL || c.Port != 143 {
		t.Fatalf("got %+v", c)
	}
	r = newRes(t, newStore(), map[string]string{"IMAP_HOST": "h", "IMAP_PORT": "bad"})
	if c := LoadIMAP(context.Background(), r, "w1"); c != (IMAP{}) {
		t.Fatalf("bad port: want zero, got %+v", c)
	}
}

func TestFooterTrimmed(t *testing.T) {
	r := newRes(t, newStore(), map[string]string{"OUTREACH_FOOTER": "  1 Main St\n "})
	if got := Footer(context.Background(), r, "w1"); got != "1 Main St" {
		t.Fatalf("got %q", got)
	}
	if got := Footer(context.Background(), newRes(t, newStore(), nil), "w1"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestSetSMTPOnlyGivenFields(t *testing.T) {
	st := newStore()
	r := newRes(t, st, nil)
	host, port, tls := "h.example.com", 465, false
	if err := SetSMTP(context.Background(), r, "w1", SMTPUpdate{Host: &host, Port: &port, UseTLS: &tls}); err != nil {
		t.Fatal(err)
	}
	if len(st.ws) != 3 {
		t.Fatalf("want 3 keys written, got %v", len(st.ws))
	}
	c := LoadSMTP(context.Background(), r, "w1")
	if c.Host != "h.example.com" || c.Port != 465 || c.UseTLS {
		t.Fatalf("got %+v", c)
	}
	for _, k := range []string{"SMTP_EMAIL", "SMTP_PASSWORD", "SMTP_FROM_NAME", "SMTP_MAX_PER_HOUR"} {
		if _, ok := st.ws["w1/"+k]; ok {
			t.Fatalf("%s must not be written", k)
		}
	}
	on, mph, name, em := true, 10, "Acme", "a@b.co"
	pw := config.Secret("pw-secret")
	if err := SetSMTP(context.Background(), r, "w1", SMTPUpdate{UseTLS: &on, MaxPerHour: &mph, FromName: &name, Email: &em, Password: &pw}); err != nil {
		t.Fatal(err)
	}
	c = LoadSMTP(context.Background(), r, "w1")
	if !c.UseTLS || c.MaxPerHour != 10 || c.FromName != "Acme" || c.Email != "a@b.co" || c.Password.Reveal() != "pw-secret" {
		t.Fatalf("got %+v", c)
	}
	// stored value is ciphertext, never the plaintext password
	if strings.Contains(st.ws["w1/SMTP_PASSWORD"], "pw-secret") {
		t.Fatal("password stored in clear")
	}
}

func TestConfigNeverLogsPassword(t *testing.T) {
	r := newRes(t, newStore(), map[string]string{"SMTP_HOST": "h", "SMTP_EMAIL": "e@x.co", "SMTP_PASSWORD": "hunter2-secret", "IMAP_HOST": "i"})
	s := LoadSMTP(context.Background(), r, "w1")
	im := LoadIMAP(context.Background(), r, "w1")
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", "smtp", s, "imap", im)
	out := buf.String() + fmt.Sprintf("%v %+v %#v %v %+v %s", s, s, s, im, im, s.Password)
	if strings.Contains(out, "hunter2-secret") {
		t.Fatalf("password leaked: %s", out)
	}
}

func TestConfigIsolatesWorkspaces(t *testing.T) {
	st := newStore()
	r := newRes(t, st, nil)
	set(t, r, "wA", "SMTP_HOST", "a.example.com")
	set(t, r, "wA", "SMTP_PASSWORD", "pw-A")
	set(t, r, "wA", "IMAP_HOST", "imap-a.example.com")
	b := LoadSMTP(context.Background(), r, "wB")
	if b.Host != "" || b.Password.Reveal() != "" {
		t.Fatalf("wB sees wA: %+v", b)
	}
	if im := LoadIMAP(context.Background(), r, "wB"); im.Host != "" || im.Password.Reveal() != "" {
		t.Fatalf("wB imap sees wA: %+v", im)
	}
	if a := LoadSMTP(context.Background(), r, "wA"); a.Host != "a.example.com" || a.Password.Reveal() != "pw-A" {
		t.Fatalf("wA: %+v", a)
	}
}
