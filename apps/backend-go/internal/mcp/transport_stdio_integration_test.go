//go:build integration

package mcp

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// resultText returns the text payload and the envelope id of a tools/call answer.
func resultText(t *testing.T, body string) (id, text string) {
	t.Helper()
	var r struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || len(r.Result.Content) != 1 {
		t.Fatalf("bad answer %q: %v", body, err)
	}
	return string(r.ID), r.Result.Content[0].Text
}

// The stdio fixtures that run a read tool against the seeded database.
func TestStdioReadToolFixturesAgainstDatabase(t *testing.T) {
	e := newReadEnv(t, 4)
	env := newStdioEnv(t)
	env.resolv = func(string, int) (Ctx, error) { return e.ctxFor("read"), nil }
	for _, name := range []string{"read__find_leads.json", "read__unicode_result.json"} {
		fx := loadStdioFixture(t, name)
		in := fx.Request.Stdin
		if strings.Contains(in, `"lead_id": 6`) { // seeded fixture id -> this run's id
			body := strings.Replace(parseFrames(t, in)[0], `"lead_id": 6`, `"lead_id": `+strconv.FormatInt(e.ids[5], 10), 1)
			in = frame(body)
		}
		got, err := run(t, e.server, e.d, in)
		if err != nil {
			t.Fatal(err)
		}
		gb, wb := parseFrames(t, got), parseFrames(t, fx.Response.Stdout)
		if len(gb) != 1 || len(wb) != 1 {
			t.Fatalf("%s: frames got %d want %d", name, len(gb), len(wb))
		}
		gid, gtext := resultText(t, gb[0])
		wid, wtext := resultText(t, wb[0])
		if gid != wid || gtext != e.norm(wtext) {
			t.Errorf("%s:\n got %s\nwant %s", name, gtext, e.norm(wtext))
		}
		// Byte parity of the framing: the header length matches the body.
		if !strings.HasPrefix(got, "Content-Length: "+strconv.Itoa(len(gb[0]))+"\r\n\r\n") {
			t.Errorf("%s: header %q", name, got[:40])
		}
	}
}
