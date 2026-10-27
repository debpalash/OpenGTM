//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const adminPW = "correct horse battery staple 42"

// needPkg skips a scenario until its Go package exists (a directory under internal/ with a non-test Go file), so the
// harness grows as packages land. Today's packages are listed in the plan 15 work packages.
func needPkg(t *testing.T, pkgs ...string) {
	t.Helper()
	for _, p := range pkgs {
		files, _ := filepath.Glob(filepath.Join("..", p, "*.go"))
		real := 0
		for _, f := range files {
			if !strings.HasSuffix(f, "_test.go") {
				real++
			}
		}
		if real == 0 {
			t.Skipf("package internal/%s has not landed", p)
		}
	}
}

func skipIfNoRoute(t *testing.T, code int, raw json.RawMessage, what string) {
	t.Helper()
	if code == http.StatusNotFound && strings.Contains(string(raw), "Not Found") {
		t.Fatalf("%s: route is not registered in the Go API (404 %s)", what, raw)
	}
}

func TestNoPython(t *testing.T) {
	s := startStack(t, nil)
	var ses *session
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // http column + webhook sink
		s.recordHook(r)
		_, _ = w.Write([]byte(`{"ok":true,"company_size":42}`))
	}))
	defer hook.Close()

	run := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := s.t
			s.t = t
			defer func() { s.t = parent }()
			s.py.scenario(name)
			fn(t)
			if c := s.py.calls(name); len(c) > 0 {
				t.Errorf("scenario called Python: %s", pyList(c))
			}
		})
	}

	run("setup_login", func(t *testing.T) {
		st := s.must(nil, "GET", "/auth/setup/status", nil, 200)
		if st["setup_required"] != true {
			t.Fatalf("status %v", st)
		}
		s.must(nil, "POST", "/auth/setup", map[string]any{"username": "admin", "password": adminPW, "workspace_name": "Main", "setup_token": setupToken}, 200)
		ses = s.login("admin", adminPW)
		ses.ws = s.workspaceID()
		s.must(ses, "GET", "/auth/me", nil, 200)
		if st := s.must(nil, "GET", "/auth/setup/status", nil, 200); st["setup_required"] != false {
			t.Fatalf("setup still open: %v", st)
		}
	})

	run("provider_setup", func(t *testing.T) { // the fake LLM is the workspace default provider (stored through the settings API)
		if ses == nil {
			t.Skip("needs setup_login")
		}
		s.must(ses, "PUT", "/api/settings/providers/groq", map[string]any{"api_key": "gsk-e2e-e2e-e2e-e2e", "base_url": s.llm.srv.URL + "/v1", "model": "fake", "set_default": true}, 200)
	})

	// runWB creates a workbook, runs it and reports which executor took the job; a Python-executor job fails fast.
	runWB := func(t *testing.T, name string, cols []map[string]any, wait time.Duration) []string {
		t.Helper()
		all := append([]map[string]any{{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"},
			{"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website"},
			{"id": "email", "name": "Email", "type": "lead_field", "lead_field": "email"}}, cols...)
		wb := s.must(ses, "POST", "/api/workbooks/", map[string]any{"name": name, "columns_config": all}, 200)
		id := str(wb, "id")
		s.must(ses, "POST", "/api/workbooks/"+id+"/rows", map[string]any{"rows": []map[string]any{
			{"company": "Globex", "website": "https://globex.example.com", "email": "hello@globex.example.com"},
			{"company": "Initech", "website": "https://initech.example.com", "email": ""},
		}}, 200)
		before := s.maxJob()
		code, raw := s.do(ses, "POST", "/api/workbooks/"+id+"/run", map[string]any{})
		if code != 200 && code != 202 {
			t.Fatalf("run = %d %.300s", code, raw)
		}
		jobs := s.jobsSince(before, "run_workbook")
		for _, j := range jobs {
			if strings.HasPrefix(j, "run_workbook/python") {
				t.Fatalf("run_workbook stays on the Python executor: %s", j)
			}
		}
		jobs = s.drain(before, wait)
		t.Logf("jobs: %v", jobs)
		for _, j := range jobs {
			if strings.Contains(j, "/failed") || strings.Contains(j, "/pending") || strings.Contains(j, "/processing") {
				t.Errorf("job did not complete: %s", j)
			}
		}
		_, raw = s.do(ses, "GET", "/api/workbooks/"+id, nil)
		t.Logf("workbook: %.500s", raw)
		return jobs
	}
	cols := map[string]map[string]any{
		"formula":      {"id": "calc_len", "name": "Name length", "type": "formula", "formula": "len({company})"},
		"http":         {"id": "size", "name": "Size", "type": "http", "http_url": hook.URL + "/size?c={company}", "http_method": "GET", "http_extract": "company_size"},
		"email_verify": {"id": "verified", "name": "Verified email", "type": "waterfall", "waterfall": []string{"hunter_io"}, "target_field": "email", "verify": true},
		"waterfall":    {"id": "wf", "name": "Waterfall", "type": "waterfall", "waterfall": []string{"hunter_io"}, "target_field": "email"},
		"enrichment":   {"id": "enr", "name": "Hunter", "type": "enrichment", "provider": "hunter_io", "target_field": "email"},
		"output":       {"id": "push", "name": "Push", "type": "output", "destination": "webhook", "destination_config": map[string]any{"url": hook.URL + "/out", "method": "POST", "body": `{"company":"{company}"}`}},
		"ai_formula":   {"id": "summary", "name": "Summary", "type": "ai_formula", "prompt": "Summarize {company}", "input_columns": []string{"company"}},
	}
	for _, name := range []string{"formula", "http", "enrichment", "waterfall", "email_verify", "output", "ai_formula"} {
		name := name
		run("workbook_run_"+name, func(t *testing.T) {
			if ses == nil {
				t.Skip("needs setup_login")
			}
			if name == "ai_formula" {
				needPkg(t, "aicol")
			}
			hookMu.Lock()
			hookReqs = nil
			hookMu.Unlock()
			runWB(t, "wb "+name, []map[string]any{cols[name]}, 20*time.Second)
			if name == "output" && s.hookCount() == 0 {
				t.Errorf("the output column never reached the sink")
			}
		})
	}
	run("workbook_run_all_columns", func(t *testing.T) {
		if ses == nil {
			t.Skip("needs setup_login")
		}
		needPkg(t, "aicol")
		runWB(t, "wb all", []map[string]any{cols["formula"], cols["http"], cols["email_verify"], cols["output"], cols["ai_formula"]}, 30*time.Second)
	})

	run("collect_job", func(t *testing.T) {
		if ses == nil {
			t.Skip("needs setup_login")
		}
		needPkg(t, "collect")
		before := s.maxJob()
		code, raw := s.do(ses, "POST", "/api/collect", map[string]any{"query": "IT staffing companies in Pune"})
		skipIfNoRoute(t, code, raw, "POST /api/collect")
		if code != 200 {
			t.Fatalf("collect = %d %.300s", code, raw)
		}
		jobs := s.drain(before, 20*time.Second)
		t.Logf("jobs: %v", jobs)
		for _, j := range jobs {
			if strings.HasPrefix(j, "collect/python") || strings.Contains(j, "/failed") {
				t.Errorf("collect not served by Go: %s", j)
			}
		}
	})

	run("bulk_enrich", func(t *testing.T) {
		if ses == nil {
			t.Skip("needs setup_login")
		}
		needPkg(t, "collect")
		var leadID int
		_ = s.owner.QueryRow(t.Context(), `INSERT INTO leads (company, workspace_id, website, status) VALUES ('Globex', $1, 'https://globex.example.com', 'new') RETURNING id`, ses.ws).Scan(&leadID)
		before := s.maxJob()
		code, raw := s.do(ses, "POST", "/api/leads/bulk-enrich", map[string]any{"action": "find_emails", "lead_ids": []int{leadID}})
		skipIfNoRoute(t, code, raw, "POST /api/leads/bulk-enrich")
		if code != 200 && code != 202 {
			t.Fatalf("bulk-enrich = %d %.300s", code, raw)
		}
		jobs := s.drain(before, 20*time.Second)
		t.Logf("jobs: %v", jobs)
		for _, j := range jobs {
			if strings.HasPrefix(j, "bulk_enrich/python") || strings.Contains(j, "/failed") {
				t.Errorf("bulk_enrich not served by Go: %s", j)
			}
		}
	})

	run("copilot_chat_tool", func(t *testing.T) {
		if ses == nil {
			t.Skip("needs setup_login")
		}
		s.llm.setTool("get_lead_stats", "{}")
		code, raw := s.do(ses, "POST", "/api/copilotkit", map[string]any{"messages": []map[string]any{{"role": "user", "content": "how many leads do I have?"}}})
		skipIfNoRoute(t, code, raw, "POST /api/copilotkit")
		if code != 200 {
			t.Fatalf("copilot = %d %.400s", code, raw)
		}
		body := string(raw)
		t.Logf("sse: %.600s", body)
		if !strings.Contains(body, "get_lead_stats") {
			t.Errorf("stream has no get_lead_stats tool frame")
		}
		if !strings.Contains(body, "All done.") {
			t.Errorf("stream has no final assistant text")
		}
	})

	run("automation_webhook", func(t *testing.T) {
		if ses == nil {
			t.Skip("needs setup_login")
		}
		code, raw := s.do(ses, "POST", "/api/automations/triggers", map[string]any{
			"name": "On enter", "trigger_type": "on_audience_enter", "trigger_config": map[string]any{"audience_ids": []string{"x"}},
			"actions": []map[string]any{{"type": "webhook", "config": map[string]any{"url": hook.URL + "/automation"}}},
		})
		skipIfNoRoute(t, code, raw, "POST /api/automations/triggers")
		if code != 200 {
			t.Fatalf("create trigger = %d %.300s", code, raw)
		}
		var tr map[string]any
		_ = json.Unmarshal(raw, &tr)
		before := s.maxJob()
		code, raw = s.do(ses, "POST", "/api/automations/triggers/"+str(tr, "id")+"/run", map[string]any{})
		if code >= 300 {
			t.Fatalf("run trigger = %d %.300s", code, raw)
		}
		t.Logf("run: %.300s jobs=%v", raw, s.drain(before, 20*time.Second))
	})

	run("audience_refresh_destination", func(t *testing.T) {
		if ses == nil {
			t.Skip("needs setup_login")
		}
		aud := map[string]any{}
		code, raw := s.do(ses, "POST", "/api/audiences", map[string]any{"name": "Everyone"})
		skipIfNoRoute(t, code, raw, "POST /api/audiences")
		if code != 201 && code != 200 {
			t.Fatalf("audience = %d %.300s", code, raw)
		}
		_ = json.Unmarshal(raw, &aud)
		dest := s.must(ses, "POST", "/api/audience-destinations", map[string]any{"audience_id": str(aud, "id"), "destination_type": "webhook", "name": "hook", "config": map[string]any{"url": hook.URL + "/dest"}}, 200)
		before := s.maxJob()
		code, raw = s.do(ses, "POST", "/api/audiences/"+str(aud, "id")+"/refresh", map[string]any{})
		if code >= 300 {
			t.Fatalf("refresh = %d %.300s", code, raw)
		}
		jobs := s.drain(before, 30*time.Second)
		t.Logf("dest %v jobs: %v", str(dest, "id"), jobs)
		for _, j := range jobs {
			if strings.Contains(j, "/python/") || strings.Contains(j, "/failed") {
				t.Errorf("audience job not served by Go: %s", j)
			}
		}
	})
	_ = os.Stdout
	_ = fmt.Sprint
}

var (
	hookMu   sync.Mutex
	hookReqs []string
)

func (s *stack) recordHook(r *http.Request) {
	hookMu.Lock()
	hookReqs = append(hookReqs, r.Method+" "+r.URL.Path)
	hookMu.Unlock()
}

func (s *stack) hookCount() int {
	hookMu.Lock()
	defer hookMu.Unlock()
	return len(hookReqs)
}
