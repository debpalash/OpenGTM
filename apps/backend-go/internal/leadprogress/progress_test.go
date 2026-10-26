package leadprogress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---- vector plumbing -------------------------------------------------------

var sentinel = regexp.MustCompile(`(?s)^<(float|int|NoneType|datetime|date|Decimal|PurePosixPath|set|bytes|Unprintable|ValueError|frozenset):(.*)>$`)

func decodeSentinel(s string, key bool) any {
	m := sentinel.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	switch {
	case m[1] == "float" && !key:
		switch m[2] {
		case "nan":
			return math.NaN()
		case "inf":
			return math.Inf(1)
		case "-inf":
			return math.Inf(-1)
		}
	case m[1] == "NoneType":
		return "null"
	}
	return m[2]
}

// parseOrdered decodes JSON keeping key order (Obj) and numbers (json.Number),
// translating the recorder's "<Type:repr>" sentinels back into the value or
// the string Python's default=str / key coercion produced.
func parseOrdered(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var rd func() any
	rd = func() any {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		switch x := tok.(type) {
		case json.Delim:
			if x == '[' {
				out := []any{}
				for dec.More() {
					out = append(out, rd())
				}
				dec.Token()
				return out
			}
			out := Obj{}
			for dec.More() {
				kt, _ := dec.Token()
				out = append(out, Field{decodeSentinel(kt.(string), true).(string), rd()})
			}
			dec.Token()
			return out
		case string:
			return decodeSentinel(x, false)
		}
		return tok
	}
	return rd()
}

type emitVec struct {
	Name     string          `json:"name"`
	Bound    [][2]string     `json:"bound"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Time     float64         `json:"time"`
	RedisOps [][]any         `json:"redis_ops"`
}

type recentVec struct {
	Name      string            `json:"name"`
	List      []string          `json:"redis_list_newest_first"`
	Limit     int               `json:"limit"`
	Workspace string            `json:"workspace_id"`
	Local     []map[string]any  `json:"local_events"`
	RedisDown bool              `json:"redis_down"`
	Lrange    [][]any           `json:"lrange_calls"`
	Result    []json.RawMessage `json:"result"`
}

type vectors struct {
	Emit   []emitVec   `json:"emit"`
	Recent []recentVec `json:"recent"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/progress_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// resolveWorkspace is the caller-side half of Python's _workspace_for: a bound
// job wins, else the data's workspace_id, else unscoped.
func resolveWorkspace(v emitVec, data Obj) string {
	if jid, ok := data.get("job_id"); ok && jid != nil && fmt.Sprint(jid) != "" {
		for _, b := range v.Bound {
			if b[0] == fmt.Sprint(jid) {
				return b[1]
			}
		}
	}
	if w, ok := data.get("workspace_id"); ok {
		if s, ok := w.(string); ok {
			return s
		}
	}
	return ""
}

// ---- unit tests ------------------------------------------------------------

func TestEmitBytesMatchPythonVectors(t *testing.T) {
	for _, v := range loadVectors(t).Emit {
		t.Run(v.Name, func(t *testing.T) {
			data, _ := parseOrdered(t, v.Data).(Obj)
			ws := resolveWorkspace(v, data)
			ev := buildEvent(ws, v.Type, v.Time, data)
			ops := buildOps(ws, ev)
			if len(ops) != len(v.RedisOps) {
				t.Fatalf("ops = %d, want %d", len(ops), len(v.RedisOps))
			}
			for i, want := range v.RedisOps {
				got := ops[i]
				if got.cmd != want[0] || got.key != want[1].(string) {
					t.Fatalf("op %d = %s %q, want %v", i, got.cmd, got.key, want[:2])
				}
				if got.cmd == "ltrim" {
					if float64(got.from) != want[2].(float64) || float64(got.to) != want[3].(float64) {
						t.Fatalf("ltrim range %d %d", got.from, got.to)
					}
				} else if got.arg != want[2].(string) {
					t.Fatalf("op %d bytes\n got: %s\nwant: %s", i, got.arg, want[2])
				}
			}
		})
	}
}

func TestEmitNeverUnscoped(t *testing.T) {
	// No workspace: nothing is built for Redis, and a live client is not
	// touched (a nil-deref would panic on a Bus with a bogus client).
	if ops := buildOps("", Obj{{"type", "x"}}); ops != nil {
		t.Fatalf("unscoped ops: %v", ops)
	}
	b := NewBus(nil)
	ev := b.Emit(context.Background(), "", "x", map[string]any{"workspace_id": "ws-evil", "a": int64(1)})
	if ev["type"] != "x" || ev["workspace_id"] != "ws-evil" {
		// empty server-side ws leaves the data key alone (Python: falsy -> unscoped)
		t.Fatalf("event = %v", ev)
	}
	got, _ := b.Recent(context.Background(), "ws-evil", 10)
	if len(got) != 1 {
		t.Fatalf("local deque keyed by event workspace: %v", got)
	}
	// ...but an empty tenant never reads other tenants' local events.
	if all, _ := b.Recent(context.Background(), "", 10); len(all) != 0 {
		t.Fatalf("empty workspace read: %v", all)
	}
}

func TestEmitServerWorkspaceWinsOverData(t *testing.T) {
	ev := buildEvent("ws-a", "x", 1, Obj{{"job_id", "j"}, {"workspace_id", "ws-evil"}})
	if v, _ := ev.get("workspace_id"); v != "ws-a" {
		t.Fatalf("workspace_id = %v", v)
	}
	if string(dumps(ev)) != `{"type":"x","ts":1.0,"job_id":"j","workspace_id":"ws-a"}` {
		t.Fatalf("%s", dumps(ev))
	}
}

func TestEmitRedisDownIsNoop(t *testing.T) {
	b := NewBus(nil)
	ev := b.Emit(context.Background(), "ws-1", "x", map[string]any{"message": "m"})
	if ev["workspace_id"] != "ws-1" {
		t.Fatalf("%v", ev)
	}
	rows, err := b.Recent(context.Background(), "ws-1", 5)
	if err != nil || len(rows) != 1 || rows[0]["message"] != "m" {
		t.Fatalf("local fallback: %v %v", rows, err)
	}
	// local deque is capped at 500 and tenant filtered
	for i := 0; i < 600; i++ {
		b.Emit(context.Background(), "ws-2", "t", map[string]any{"i": int64(i)})
	}
	rows, _ = b.Recent(context.Background(), "ws-2", 1000)
	if len(rows) != 500 || rows[499]["i"] != int64(599) || rows[0]["i"] != int64(100) {
		t.Fatalf("deque cap: %d", len(rows))
	}
}

func TestDumpsDefaultStrAndOrder(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 0, 0, 5000, time.UTC)
	got := string(dumps(Obj{{"b", ts}, {"a", fmt.Errorf("bad")}, {"b", int64(2)}, {"n", math.NaN()}}))
	if got != `{"b":2,"a":"bad","n":NaN}` {
		t.Fatalf("%s", got)
	}
	if string(dumps(ts)) != `"2026-08-29 10:00:00.000005+00:00"` {
		t.Fatalf("%s", dumps(ts))
	}
	if string(dumps("<&>")) != `"<&>"`[0:0]+`"<&>"` {
		t.Fatalf("html must not be escaped: %s", dumps("<&>"))
	}
	if !strings.Contains(string(dumps("é ")), `é `) {
		t.Fatal("ensure_ascii")
	}
}

func TestRecentClampsLimit(t *testing.T) {
	b := NewBus(nil)
	for i := 0; i < 3; i++ {
		b.Emit(context.Background(), "ws", "t", map[string]any{"i": int64(i)})
	}
	for _, c := range []struct{ limit, want int }{{-1, 0}, {0, 0}, {1, 1}, {2, 2}, {100000, 3}} {
		rows, err := b.Recent(context.Background(), "ws", c.limit)
		if err != nil || len(rows) != c.want {
			t.Fatalf("limit %d: %d rows %v", c.limit, len(rows), err)
		}
	}
}
