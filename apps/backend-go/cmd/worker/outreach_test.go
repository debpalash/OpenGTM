package main

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

func TestWorkerTimeoutsPerType(t *testing.T) {
	cfg := config.Config{JobTimeout: 1800 * time.Second}
	for typ, want := range map[string]time.Duration{
		"send": 300 * time.Second, "outreach_inbound_poll": 600 * time.Second, "run_workbook": 1800 * time.Second,
	} {
		if got := jobLimit(cfg, typ); got != want {
			t.Errorf("%s: %v, want %v", typ, got, want)
		}
	}
}

func TestOutreachMasterKeyInsecureDefault(t *testing.T) {
	ins := config.InsecureDefaultSecretKey
	for _, tc := range []struct {
		name string
		cfg  config.Config
		err  bool
	}{
		{"insecure prod", config.Config{SecretKey: ins, AppEnv: "production"}, true},
		{"insecure substring prod", config.Config{SecretKey: "x-" + ins + "-y", AppEnv: "prod"}, true},
		{"insecure dev", config.Config{SecretKey: ins, AppEnv: "dev"}, false},
		{"insecure dev padded and cased", config.Config{SecretKey: ins, AppEnv: " Development "}, false},
		{"strong prod", config.Config{SecretKey: "a-strong-secret-key-of-32-bytes-or-more", AppEnv: "production"}, false},
		{"master set wins", config.Config{SecretKey: ins, SecretsMasterKey: "m", AppEnv: "production"}, false},
	} {
		k, err := outreachMasterKey(tc.cfg)
		if tc.err {
			if !errors.Is(err, secrets.ErrInsecureMasterKey) {
				t.Errorf("%s: err %v", tc.name, err)
			}
			continue
		}
		if err != nil || len(k) == 0 {
			t.Errorf("%s: key %d err %v", tc.name, len(k), err)
		}
	}
}

func TestProductionOutreachWiringHasNoTestKnobs(t *testing.T) {
	cfg := config.Config{AppEnv: "production", SecretKey: "a-strong-secret-key-of-32-bytes-or-more"}
	cfg.Flags.AutomationsEnabled = true
	send, inbound := outreachHandlers(cfg, nil, telemetry.NewLogger(io.Discard), nil, []byte("k"), outreachOpts{})
	m, ok := send.Mailer.(*outreachsend.SMTPMailer)
	if !ok {
		t.Fatalf("mailer %T", send.Mailer)
	}
	if len(m.Client.PlainHosts) != 0 || m.Client.RootCAs != nil || m.Dial == nil {
		t.Fatalf("production smtp client has test knobs or no guarded dial: %+v", m.Client)
	}
	if inbound.NewClient != nil || inbound.Dial == nil {
		t.Fatal("production inbound handler must dial through egress and have no client override")
	}
	if send.Caps == nil {
		t.Fatal("paid sends need the autocaps adapter wired")
	}
	if !send.Automations || !inbound.Automations {
		t.Fatal("AUTOMATIONS_ENABLED not propagated")
	}
}

func TestOutreachRegistersBothTypesEvenWithoutResolver(t *testing.T) {
	// CREDENTIALS_SOURCE=http: no resolver; both types still get a (hand-back) handler.
	cfg := config.Config{AppEnv: "production", SecretKey: "a-strong-secret-key-of-32-bytes-or-more"}
	h := buildOutreach(cfg, nil, telemetry.NewLogger(io.Discard), nil, outreachOpts{})
	for _, typ := range []string{"send", "outreach_inbound_poll"} {
		if h[typ] == nil {
			t.Errorf("no handler for %s", typ)
		}
	}
}
