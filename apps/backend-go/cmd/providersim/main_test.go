package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func statusSeq(t *testing.T, cfg Config, n int) []int {
	t.Helper()
	srv := httptest.NewServer(NewSim(cfg))
	defer srv.Close()
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		resp, err := http.Post(srv.URL+"/email-finder", "application/json",
			strings.NewReader(`{"first_name":"Ada","last_name":"Lovelace","domain":"example.com"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		out = append(out, resp.StatusCode)
	}
	return out
}

func TestSimDeterministic(t *testing.T) {
	cfg := Config{Profile: "ratelimit", Seed: 7}
	a, b := statusSeq(t, cfg, 100), statusSeq(t, cfg, 100)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("diverged at %d: %d vs %d", i, a[i], b[i])
		}
		want := 200
		if (i+1)%10 == 0 {
			want = 429
		}
		if a[i] != want {
			t.Fatalf("call %d: got %d want %d", i+1, a[i], want)
		}
	}
}

func TestOKBodyAndStats(t *testing.T) {
	sim := NewSim(Config{Profile: "ok", Seed: 1})
	srv := httptest.NewServer(sim)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/email-finder", "application/json",
		strings.NewReader(`{"first_name":"Ada","last_name":"Lovelace","domain":"Example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["email"] != "ada.lovelace@example.com" {
		t.Fatalf("email=%q", body["email"])
	}
	r2, _ := http.Get(srv.URL + "/stats")
	var st struct {
		Calls    int            `json:"calls"`
		ByStatus map[string]int `json:"by_status"`
	}
	json.NewDecoder(r2.Body).Decode(&st)
	r2.Body.Close()
	if st.Calls != 1 || st.ByStatus["200"] != 1 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestMalformedEvery15th(t *testing.T) {
	srv := httptest.NewServer(NewSim(Config{Profile: "malformed", Seed: 1}))
	defer srv.Close()
	for i := 1; i <= 15; i++ {
		resp, _ := http.Post(srv.URL+"/email-finder", "application/json", strings.NewReader(`{}`))
		var v any
		err := json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if (i%15 == 0) != (err != nil) {
			t.Fatalf("call %d: decode err=%v", i, err)
		}
	}
}

func TestTimeoutHangsEvery20th(t *testing.T) {
	srv := httptest.NewServer(NewSim(Config{Profile: "timeout", Seed: 1, Hang: 150 * time.Millisecond}))
	defer srv.Close()
	for i := 1; i <= 20; i++ {
		start := time.Now()
		resp, _ := http.Post(srv.URL+"/email-finder", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
		slow := time.Since(start) >= 150*time.Millisecond
		if slow != (i == 20) {
			t.Fatalf("call %d slow=%v", i, slow)
		}
	}
}

func TestSlowLatencyDeterministic(t *testing.T) {
	a := NewSim(Config{Profile: "slow", Seed: 3})
	b := NewSim(Config{Profile: "slow", Seed: 3})
	for i := 0; i < 20; i++ {
		if a.plan().delay != b.plan().delay {
			t.Fatal("latency not deterministic")
		}
	}
}

type statsDoc struct {
	Calls     int            `json:"calls"`
	ByOutcome map[string]int `json:"by_outcome"`
	Profile   string         `json:"profile"`
}

func fetchStats(t *testing.T, url string) statsDoc {
	t.Helper()
	r, err := http.Get(url + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var st statsDoc
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestNoMatchEveryNth(t *testing.T) {
	srv := httptest.NewServer(NewSim(Config{Profile: "ok", Seed: 1, NoMatchEvery: 4}))
	defer srv.Close()
	for i := 1; i <= 8; i++ {
		resp, _ := http.Post(srv.URL+"/email-finder", "application/json",
			strings.NewReader(`{"first_name":"A","last_name":"B","domain":"x.com"}`))
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if (i%4 == 0) != (body["email"] == nil) {
			t.Fatalf("call %d body=%v", i, body)
		}
	}
	st := fetchStats(t, srv.URL)
	if st.ByOutcome["no_match"] != 2 || st.ByOutcome["ok"] != 6 || st.Profile != "ok" {
		t.Fatalf("stats=%+v", st)
	}
}

func TestNoMatchDefaultNever(t *testing.T) {
	srv := httptest.NewServer(NewSim(Config{Profile: "ok", Seed: 1}))
	defer srv.Close()
	for i := 0; i < 10; i++ {
		resp, _ := http.Post(srv.URL+"/email-finder", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
	}
	if st := fetchStats(t, srv.URL); st.ByOutcome["no_match"] != 0 || st.ByOutcome["ok"] != 10 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestOutcomesRateLimitMalformedTimeoutCancelled(t *testing.T) {
	rl := httptest.NewServer(NewSim(Config{Profile: "ratelimit", Seed: 1}))
	defer rl.Close()
	for i := 0; i < 10; i++ {
		resp, _ := http.Post(rl.URL+"/email-finder", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
	}
	if st := fetchStats(t, rl.URL); st.ByOutcome["ratelimited"] != 1 || st.ByOutcome["ok"] != 9 {
		t.Fatalf("ratelimit stats=%+v", st)
	}

	mf := httptest.NewServer(NewSim(Config{Profile: "malformed", Seed: 1}))
	defer mf.Close()
	for i := 0; i < 15; i++ {
		resp, _ := http.Post(mf.URL+"/email-finder", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
	}
	if st := fetchStats(t, mf.URL); st.ByOutcome["malformed"] != 1 || st.ByOutcome["ok"] != 14 {
		t.Fatalf("malformed stats=%+v", st)
	}

	to := httptest.NewServer(NewSim(Config{Profile: "timeout", Seed: 1, Hang: 5 * time.Second}))
	defer to.Close()
	for i := 0; i < 19; i++ {
		resp, _ := http.Post(to.URL+"/email-finder", "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
	}
	client := &http.Client{Timeout: 100 * time.Millisecond}
	if resp, err := client.Post(to.URL+"/email-finder", "application/json", strings.NewReader(`{}`)); err == nil {
		resp.Body.Close()
		t.Fatal("expected client timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := fetchStats(t, to.URL)
		if st.ByOutcome["timeout"] == 1 && st.ByOutcome["cancelled"] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout stats=%+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSlowPathClientCancelRecorded(t *testing.T) {
	srv := httptest.NewServer(NewSim(Config{Profile: "slow", Seed: 1}))
	defer srv.Close()
	client := &http.Client{Timeout: 20 * time.Millisecond}
	if resp, err := client.Post(srv.URL+"/email-finder", "application/json", strings.NewReader(`{}`)); err == nil {
		resp.Body.Close()
		t.Skip("slow draw finished under 20ms")
	}
	deadline := time.Now().Add(2 * time.Second)
	for fetchStats(t, srv.URL).ByOutcome["cancelled"] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("cancel not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
