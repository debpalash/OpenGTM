package personintel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

func loadVectors(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// stubClient returns an egress client that may reach the given httptest
// server (test env allowlist) and the server's host:port.
func stubClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	return egress.NewClientForEnv("test", []string{u.Host})
}

func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// profileJSON re-marshals p with sets (emails, skills) sorted, for comparison
// with Python vectors whose set-ordered fields are stored sorted.
func profileJSON(t *testing.T, p *Profile) map[string]any {
	t.Helper()
	q := *p
	q.Emails = sorted(p.Emails)
	q.Skills = sorted(p.Skills)
	b, err := json.Marshal(&q)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func normalize(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var p Profile
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
	}
	return profileJSON(t, &p)
}
