package pyworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSourceListGetsNames(t *testing.T) {
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"sources":[{"name":"a","x":1},{"name":"b"},{"nope":1}]}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, testToken, nil).SourceList(context.Background(), time.Second)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("%v %v", got, err)
	}
	if method != "GET" || path != "/internal/leadgen/source-list" || auth != "Bearer "+testToken {
		t.Fatal(method, path, auth)
	}
}

func TestSourceProbeBodyAndNulls(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if r.Method != "POST" || r.URL.Path != "/internal/leadgen/source-probe" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"yields":{"a":3,"b":null}}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL, testToken, nil).SourceProbe(context.Background(), []string{"a", "b"}, 90*time.Second)
	if err != nil || got["a"] == nil || *got["a"] != 3 || got["b"] != nil {
		t.Fatalf("%v %v", got, err)
	}
	if body["timeout_s"].(float64) != 90 || len(body["sources"].([]any)) != 2 {
		t.Fatal(body)
	}
}

func TestSourceRPCErrorsAreOpaqueAndBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "source-list") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("secret " + testToken))
			return
		}
		_, _ = w.Write([]byte(`{"yields":` + strings.Repeat(" ", maxLeadgenBody+10) + `{}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, testToken, nil)
	if _, err := c.SourceList(context.Background(), time.Second); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v", err)
	}
	if _, err := c.SourceProbe(context.Background(), []string{"a"}, time.Second); err == nil {
		t.Fatal("oversize body must fail")
	}
	if _, err := New("", "", nil).SourceList(context.Background(), time.Second); err != ErrUnavailable {
		t.Fatal(err)
	}
}
