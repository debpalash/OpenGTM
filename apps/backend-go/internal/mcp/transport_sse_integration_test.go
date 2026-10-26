//go:build integration

package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sseFixture struct {
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"response"`
	Token *string `json:"token"`
}

// TestSSEFixturesReplay replays the Python-recorded auth/ and sse/ HTTP cases
// against a real database seeded with the recorded token table.
func TestSSEFixturesReplay(t *testing.T) {
	e := newAuthEnv(t)
	var idx struct {
		Tokens map[string]struct {
			UserID       int      `json:"user_id"`
			WorkspaceID  string   `json:"workspace_id"`
			Capabilities []string `json:"capabilities"`
			ExpiresAt    *string  `json:"expires_at"`
			RevokedAt    *string  `json:"revoked_at"`
		} `json:"tokens"`
	}
	raw, err := os.ReadFile("../../testdata/mcp/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	users := map[int]int32{}
	wss := map[string]string{}
	plain := map[string]string{}
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
	n := 0
	for alias, tk := range idx.Tokens {
		n++
		if _, ok := users[tk.UserID]; !ok {
			users[tk.UserID] = e.user(t, fmt.Sprintf("u%d", tk.UserID))
		}
		if _, ok := wss[tk.WorkspaceID]; !ok {
			owner := users[tk.UserID]
			wss[tk.WorkspaceID] = e.workspace(t, strings.TrimPrefix(tk.WorkspaceID, "ws-"), &owner, "")
		}
		if alias != "removed_member" {
			e.member(t, wss[tk.WorkspaceID], users[tk.UserID], "member")
		}
		plain[alias] = "ycp_seed_" + alias + "_" + e.sfx
		e.token(t, fmt.Sprintf("k%d", n), plain[alias], wss[tk.WorkspaceID], users[tk.UserID], tk.Capabilities, parse(tk.ExpiresAt), parse(tk.RevokedAt))
	}

	d := e.d
	d.Cfg = Config{RequireAuth: true}
	h := SSEHandler(NewServer(d, NewRegistry(nil, nil)), d)

	files, _ := filepath.Glob("../../testdata/mcp/auth/read__*.json")
	more, _ := filepath.Glob("../../testdata/mcp/sse/read__*.json")
	files = append(files, more...)
	if len(files) < 20 {
		t.Fatalf("fixtures missing: %d", len(files))
	}
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f)) + "/" + filepath.Base(f)
		if strings.Contains(name, "tools_call_find_leads") {
			continue // needs the production LeadPort and seeded leads (Task 17)
		}
		t.Run(name, func(t *testing.T) {
			b, _ := os.ReadFile(f)
			var fx sseFixture
			if err := json.Unmarshal(b, &fx); err != nil {
				t.Fatal(err)
			}
			var body string
			if s := string(fx.Request.Body); s != "null" && s != "" {
				body = s
			}
			r := httptest.NewRequest(fx.Request.Method, fx.Request.Path, strings.NewReader(body))
			if _, has := fx.Request.Headers["Authorization"]; has {
				scheme := "Bearer "
				switch {
				case strings.Contains(name, "basic_scheme"):
					scheme = "Basic "
				case strings.Contains(name, "lowercase_scheme"):
					scheme = "bearer "
				}
				tok := ""
				switch {
				case strings.Contains(name, "bearer_empty"):
				case strings.Contains(name, "invalid_token"):
					tok = "ycp_not_a_real_token"
				case fx.Token != nil:
					tok = plain[*fx.Token]
				}
				r.Header.Set("Authorization", scheme+tok)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != fx.Response.Status {
				t.Fatalf("status %d want %d body %s", w.Code, fx.Response.Status, w.Body.String())
			}
			for k, v := range fx.Response.Headers {
				if got := w.Header().Get(k); got != v {
					t.Fatalf("header %s = %q want %q", k, got, v)
				}
			}
			var want any
			if err := json.Unmarshal(fx.Response.Body, &want); err != nil {
				t.Fatal(err)
			}
			if s, isStr := want.(string); isStr {
				if w.Body.String() != s {
					t.Fatalf("body %q want %q", w.Body.String(), s)
				}
				return
			}
			var got any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %q: %v", w.Body.String(), err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("body\n got %s\nwant %s", w.Body.String(), fx.Response.Body)
			}
		})
	}
	_ = http.StatusOK
}
