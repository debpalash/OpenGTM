package retention

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// retention_vectors.json comes from Python (testdata/gen_vectors.py.txt).
type vectors struct {
	Now       string `json:"now"`
	Normalize []struct {
		In    map[string]any `json:"in"`
		Out   map[string]int `json:"out"`
		Error string         `json:"error"`
	} `json:"normalize"`
	Rows []struct {
		Table   string  `json:"table"`
		WS      string  `json:"ws"`
		AgeDays float64 `json:"age_days"`
	} `json:"rows"`
	Cases []struct {
		Name       string                                 `json:"name"`
		Days       map[string]any                         `json:"days"`
		Normalized map[string]int                         `json:"normalized"`
		Cutoffs    map[string]any                         `json:"cutoffs"`
		Expected   map[string]map[string]map[string]int64 `json:"expected"`
	} `json:"cases"`
	Schedule []struct {
		TZ              string           `json:"tz"`
		Scenario        string           `json:"scenario"`
		Returned        *string          `json:"returned"`
		PolicyNextRunAt *string          `json:"policy_next_run_at"`
		Mirror          map[string]any   `json:"mirror"`
		Jobs            []map[string]any `json:"jobs"`
	} `json:"schedule"`
}

func loadVectors(t *testing.T, ws map[string]string) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/retention_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for from, to := range ws {
		s = strings.ReplaceAll(s, from, to)
	}
	var v vectors
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNormalizeDaysVectors(t *testing.T) {
	v := loadVectors(t, nil)
	if len(v.Normalize) < 10 {
		t.Fatalf("only %d vectors", len(v.Normalize))
	}
	for _, c := range v.Normalize {
		got, err := NormalizeDays(c.In)
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("%v: err %v want %q", c.In, err, c.Error)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(map[string]int(got), c.Out) {
			t.Errorf("%v: got %v %v want %v", c.In, got, err, c.Out)
			continue
		}
		// JSON key order is DefaultDays order, like Python's dict(DEFAULT_DAYS) overlay.
		b, _ := json.Marshal(got)
		if !strings.HasPrefix(string(b), `{"audit":`) || strings.Index(string(b), `"signals"`) > strings.Index(string(b), `"activation"`) {
			t.Errorf("order: %s", b)
		}
	}
}

func TestNormalizeDaysHardening(t *testing.T) {
	for _, in := range []map[string]any{
		{"audit": "90"}, {"audit": true}, {"audit": 90.5}, {"audit": nil}, {"audit": 1e300}, {"audit": -1},
	} {
		if _, err := NormalizeDays(in); err == nil {
			t.Errorf("%v accepted", in)
		}
	}
	if _, err := NormalizeDays(map[string]any{"audit": int64(90), "signals": float64(31)}); err != nil {
		t.Fatal(err)
	}
	// Two violations: ordered variant reports the first in request order (Python behaviour).
	_, err := NormalizeDaysOrdered(map[string]any{"audit": 1, "signals": 1}, []string{"signals", "audit"})
	if err == nil || err.Error() != "signals retention must be 30..3650 days" {
		t.Fatal(err)
	}
}

func TestDefaultsAndMinimums(t *testing.T) {
	b, _ := json.Marshal(Days(DefaultDays))
	if string(b) != `{"audit":365,"llm_usage":365,"signals":365,"activation":180,"audience_history":365,"agent_results":180,"outreach_history":365}` {
		t.Fatal(string(b))
	}
	if MinDays["audit"] != 90 || MinDays["llm_usage"] != 30 || len(MinDays) != 7 {
		t.Fatal(MinDays)
	}
}

func TestCutoffMatchesPythonVectors(t *testing.T) {
	v := loadVectors(t, nil)
	now, _ := time.Parse(time.RFC3339, v.Now)
	targets := map[string]Target{}
	for _, tg := range Targets() {
		targets[tg.Table] = tg
	}
	if len(Targets()) != 8 || len(targets) != 8 {
		t.Fatalf("targets %v", Targets())
	}
	for _, c := range v.Cases {
		for table, want := range c.Cutoffs {
			tg, ok := targets[table]
			if !ok {
				t.Fatalf("no target %s", table)
			}
			got := Cutoff(tg, c.Normalized[tg.Category], now)
			switch tg.Kind {
			case KindEpoch:
				if f, ok := got.(float64); !ok || f != want.(float64) {
					t.Errorf("%s/%s: %v want %v", c.Name, table, got, want)
				}
			case KindDate:
				if got != want {
					t.Errorf("%s/%s: %v want %v", c.Name, table, got, want)
				}
			default:
				w, _ := time.Parse("2006-01-02T15:04:05Z07:00", want.(string))
				if g, ok := got.(time.Time); !ok || !g.Equal(w) {
					t.Errorf("%s/%s: %v want %v", c.Name, table, got, w)
				}
			}
		}
	}
	// Python's TargetS order and categories.
	want := []string{"audit:governance_audit_events", "llm_usage:llm_usage_daily", "signals:signals", "activation:destination_deliveries", "activation:destination_inbound_receipts", "audience_history:audience_membership_events", "agent_results:playbook_results", "outreach_history:outreach_sends"}
	var got []string
	for _, tg := range Targets() {
		got = append(got, tg.Category+":"+tg.Table)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestCutoffSubMicrosecondTruncated(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 999_999_999, time.UTC)
	tg := Targets()[0]
	got := Cutoff(tg, 90, now).(time.Time)
	if got.Nanosecond() != 999_999_000 {
		t.Fatal(got)
	}
}

func TestExecutorForCohortOnly(t *testing.T) {
	ex := func(en bool, ws ...string) config.ExecutorSettings {
		e := config.ExecutorSettings{Enabled: en, Workspaces: map[string]bool{}}
		for _, w := range ws {
			e.Workspaces[w] = true
		}
		return e
	}
	for _, c := range []struct {
		name string
		ex   config.ExecutorSettings
		g    config.GovernanceSettings
		ws   string
		want string
	}{
		{"all on", ex(true, "w1"), config.GovernanceSettings{GoExecutorRetention: true}, "w1", "go"},
		{"not in cohort", ex(true, "w1"), config.GovernanceSettings{GoExecutorRetention: true}, "w2", "python"},
		{"cohort flag off", ex(false, "w1"), config.GovernanceSettings{GoExecutorRetention: true}, "w1", "python"},
		{"retention flag off", ex(true, "w1"), config.GovernanceSettings{GoExecutorPlaybooks: true}, "w1", "python"},
		{"empty ws", ex(true, ""), config.GovernanceSettings{GoExecutorRetention: true}, "", "python"},
	} {
		if got := ExecutorFor(c.ex, c.g, c.ws); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
}
