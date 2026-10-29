package govadmin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRBACMemberJSONMatchesPythonFloats(t *testing.T) {
	for _, c := range []struct {
		at   *float64
		want string
	}{
		{ptr(1788000001), `"created_at":1788000001.0,`},
		{ptr(1793035121.6262798), `"created_at":1793035121.6262798,`},
		{ptr(0), `"created_at":0.0,`},
		{nil, `"created_at":null,`},
	} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf) // as httpx.WriteJSON: no HTML escaping
		enc.SetEscapeHTML(false)
		if err := enc.Encode(RBACMember{UserID: 1, Role: "owner", CreatedAt: c.at, Username: "a<b>&c", Overrides: map[string]string{}}); err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSpace(buf.String())
		want := `{"user_id":1,"role":"owner",` + c.want + `"username":"a<b>&c","overrides":{}}`
		if got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

func ptr(f float64) *float64 { return &f }
