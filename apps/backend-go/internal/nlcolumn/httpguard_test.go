package nlcolumn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func genHTTP(t *testing.T, answer string) Result {
	t.Helper()
	f := &fakeLLM{script: []json.RawMessage{json.RawMessage(answer)}}
	res, err := Generate(context.Background(), f, "call it", nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGeneratedHTTPColumnValidated(t *testing.T) {
	bad := map[string]string{
		"metadata":    `{"kind":"http","name":"T","http_url":"http://169.254.169.254/latest/meta-data"}`,
		"loopback":    `{"kind":"http","name":"T","http_url":"http://127.0.0.1:8080/x"}`,
		"localhost":   `{"kind":"http","name":"T","http_url":"https://localhost/x"}`,
		"private":     `{"kind":"http","name":"T","http_url":"https://10.0.0.5/x"}`,
		"decimal-ip":  `{"kind":"http","name":"T","http_url":"http://2130706433/x"}`,
		"file":        `{"kind":"http","name":"T","http_url":"file:///etc/passwd"}`,
		"ftp":         `{"kind":"http","name":"T","http_url":"ftp://x.test/a"}`,
		"userinfo":    `{"kind":"http","name":"T","http_url":"https://user:pw@x.test/a"}`,
		"secret-url":  `{"kind":"http","name":"T","http_url":"https://x.test/a?k={secret:KEY}"}`,
		"secret-hdr":  `{"kind":"http","name":"T","http_url":"https://x.test/a","http_headers":{"X-Thing":"Bearer {secret:KEY}"}}`,
		"secret-body": `{"kind":"http","name":"T","http_url":"https://x.test/a","http_method":"POST","http_body":{"a":["{SECRET:k}"]}}`,
		"authz":       `{"kind":"http","name":"T","http_url":"https://x.test/a","http_headers":{"Authorization":"Bearer abc"}}`,
		"cookie":      `{"kind":"http","name":"T","http_url":"https://x.test/a","http_headers":{"cookie":"a=b"}}`,
		"apikey":      `{"kind":"http","name":"T","http_url":"https://x.test/a","http_headers":{"X-API-Key":"abc"}}`,
		"host-tmpl":   `{"kind":"http","name":"T","http_url":"https://{domain}/a"}`,
	}
	for name, ans := range bad {
		t.Run(name, func(t *testing.T) {
			res := genHTTP(t, ans)
			if !res.Dropped || len(res.Warnings) == 0 {
				t.Fatalf("column not dropped: %+v", res)
			}
			if res.Kind != "http" {
				t.Errorf("kind = %q", res.Kind)
			}
			if strings.Contains(strings.Join(res.Warnings, " "), "169.254") {
				t.Errorf("warning echoes model-supplied url: %v", res.Warnings)
			}
		})
	}
}

func TestGeneratedHTTPColumnValidKept(t *testing.T) {
	res := genHTTP(t, `{"kind":"http","name":"T","http_url":"https://api.example.com/v1/{domain}?q={name}","http_headers":{"Accept":"application/json"}}`)
	if res.Dropped || len(res.Warnings) != 0 {
		t.Fatalf("valid column dropped: %+v", res)
	}
	if res.Column.Get("http_url") == nil {
		t.Fatal("column missing")
	}
}
