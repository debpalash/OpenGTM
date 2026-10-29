package copilot

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
)

type vecMsg struct {
	Role     string  `json:"role"`
	Content  string  `json:"content"`
	ToolData *string `json:"tool_data"`
}

func toMsgs(in []vecMsg) []chathistory.Message {
	out := make([]chathistory.Message, len(in))
	for i, m := range in {
		out[i] = chathistory.Message{Role: m.Role, Content: m.Content, ToolData: m.ToolData}
	}
	return out
}

type ctxVectors struct {
	Context []struct {
		Name     string   `json:"name"`
		Messages []vecMsg `json:"messages"`
		Limit    *int     `json:"limit"`
		Expected string   `json:"expected"`
	} `json:"context"`
	Latest []struct {
		Name     string         `json:"name"`
		Messages []vecMsg       `json:"messages"`
		Names    []string       `json:"names"`
		Expected map[string]any `json:"expected"`
	} `json:"latest_result"`
	ByAction []struct {
		Name     string         `json:"name"`
		Messages []vecMsg       `json:"messages"`
		Tool     string         `json:"tool"`
		ActionID string         `json:"action_id"`
		Expected map[string]any `json:"expected"`
	} `json:"result_by_action_id"`
}

func loadCtxVectors(t *testing.T) ctxVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/tool_context.json")
	if err != nil {
		t.Fatal(err)
	}
	var v ctxVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestToolContextVectors(t *testing.T) {
	v := loadCtxVectors(t)
	if len(v.Context) < 20 {
		t.Fatalf("only %d vectors", len(v.Context))
	}
	for _, c := range v.Context {
		t.Run(c.Name, func(t *testing.T) {
			limit := 3 // Python default when limit is not passed
			if c.Limit != nil {
				limit = *c.Limit
			}
			got := ToolContext(toMsgs(c.Messages), limit)
			if got != c.Expected {
				t.Fatalf("got %d bytes, want %d\n got tail %q\nwant tail %q", len(got), len(c.Expected), tail(got), tail(c.Expected))
			}
		})
	}
}

func tail(s string) string {
	if len(s) > 60 {
		return s[len(s)-60:]
	}
	return s
}

// The cut counts code points of the ASCII-escaped JSON, so the serialized part is exactly 24000 characters
// and the whole result stays ASCII-only after the (ASCII) preface.
func TestToolContextCutsAt24000CodePoints(t *testing.T) {
	big := strings.Repeat("日", 9000) // 9000 * 6 escaped chars = 54000 > 24000
	td := `{"name":"t","result":{"v":"` + big + `"}}`
	got := ToolContext([]chathistory.Message{{Role: "tool", ToolData: &td}}, 3)
	i := strings.Index(got, "here.\n")
	body := got[i+len("here.\n"):]
	if utf8.RuneCountInString(body) != 24000 || len(body) != 24000 {
		t.Fatalf("body is %d runes / %d bytes, want 24000", utf8.RuneCountInString(body), len(body))
	}
	for _, r := range body {
		if r > 127 {
			t.Fatalf("non-ascii rune %q in context", r)
		}
	}
}

func TestToolContextLimitZeroIsEmpty(t *testing.T) {
	td := `{"name":"t","result":{"a":1}}`
	msgs := []chathistory.Message{{Role: "tool", ToolData: &td}}
	if got := ToolContext(msgs, 0); got != "" {
		t.Fatalf("limit 0 = %q (python breaks before the first row)", got)
	}
	if got := ToolContext(msgs, -1); got != "" {
		t.Fatalf("limit -1 = %q", got)
	}
}

func TestLatestToolResultVectors(t *testing.T) {
	for _, c := range loadCtxVectors(t).Latest {
		t.Run(c.Name, func(t *testing.T) {
			name, res, ok := LatestToolResult(toMsgs(c.Messages), c.Names...)
			if c.Expected == nil {
				if ok {
					t.Fatalf("got %s %v, want none", name, res)
				}
				return
			}
			if !ok || name != c.Expected["name"] {
				t.Fatalf("got ok=%v name=%q, want %v", ok, name, c.Expected["name"])
			}
			assertJSONEq(t, res, c.Expected["result"])
		})
	}
}

func TestToolResultByActionIDVectors(t *testing.T) {
	for _, c := range loadCtxVectors(t).ByAction {
		t.Run(c.Name, func(t *testing.T) {
			res, ok := ToolResultByActionID(toMsgs(c.Messages), c.Tool, c.ActionID)
			if c.Expected == nil {
				if ok {
					t.Fatalf("got %v, want none", res)
				}
				return
			}
			if !ok {
				t.Fatal("want a result")
			}
			assertJSONEq(t, res, c.Expected)
		})
	}
}

func assertJSONEq(t *testing.T, got, want any) {
	t.Helper()
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("got %s want %s", a, b)
	}
}

// Decoded results keep big integers exact (UseNumber).
func TestLatestToolResultKeepsBigIntegers(t *testing.T) {
	td := `{"name":"t","result":{"big":12345678901234567890}}`
	_, res, ok := LatestToolResult([]chathistory.Message{{Role: "tool", ToolData: &td}}, "t")
	if !ok {
		t.Fatal("no result")
	}
	if n, isNum := res["big"].(json.Number); !isNum || n.String() != "12345678901234567890" {
		t.Fatalf("big = %#v", res["big"])
	}
}
