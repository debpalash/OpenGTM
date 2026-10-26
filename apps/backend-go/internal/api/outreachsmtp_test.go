package api

import (
	"strings"
	"testing"
)

func TestParseSMTPConfigHostRules(t *testing.T) {
	bad := []string{`{"smtp_host":"http://x.example"}`, `{"smtp_host":"x.example/p"}`, `{"smtp_host":"a b"}`, `{"smtp_host":"a@b.example"}`,
		`{"smtp_host":"x.example:25"}`, `{"smtp_host":""}x`, `{"smtp_port":0}`, `{"smtp_port":65536}`, `{"smtp_max_per_hour":0}`, `{"smtp_max_per_hour":100001}`}
	for _, b := range bad {
		if _, errs := parseSMTPConfig([]byte(b)); len(errs) == 0 {
			t.Errorf("%s accepted", b)
		}
	}
	u, errs := parseSMTPConfig([]byte(`{"smtp_host":" Smtp.Example.com ","smtp_port":"587","smtp_use_tls":false,"smtp_email":null}`))
	if len(errs) != 0 || u.Host == nil || *u.Host != "Smtp.Example.com" || u.Port == nil || *u.Port != 587 || u.UseTLS == nil || *u.UseTLS || u.Email != nil {
		t.Fatalf("%+v %v", u, errs)
	}
	if _, errs := parseSMTPConfig([]byte(`{"smtp_host":"` + strings.Repeat("a", 300) + `"}`)); len(errs) == 0 {
		t.Error("overlong host accepted")
	}
}
