package outreachdraft

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Vectors: testdata/drafting.json, recorded from legacy Python by internal/copilot/testdata/record_tools.py.

type selectCase struct {
	Name         string          `json:"name"`
	People       json.RawMessage `json:"people"`
	Requested    string          `json:"requested"`
	AllowRisky   bool            `json:"allow_risky"`
	Selected     json.RawMessage `json:"selected"`
	Error        string          `json:"error"`
	ContactFlags json.RawMessage `json:"contact_flags"`
}

type createCase struct {
	Name           string          `json:"name"`
	Source         json.RawMessage `json:"source"`
	SourceActionID string          `json:"source_action_id"`
	Requested      string          `json:"requested"`
	AllowRisky     bool            `json:"allow_risky"`
	Key            string          `json:"key"`
	Receipt        json.RawMessage `json:"receipt"`
	Row            struct {
		ConversationID string          `json:"conversation_id"`
		SourceActionID string          `json:"source_action_id"`
		SourceSnapshot json.RawMessage `json:"source_snapshot"`
		State          string          `json:"state"`
	} `json:"row"`
	Error        string          `json:"error"`
	ContactFlags json.RawMessage `json:"contact_flags"`
}

func loadVectors(t *testing.T) (sel []selectCase, create []createCase) {
	t.Helper()
	b, err := os.ReadFile("testdata/drafting.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Select []selectCase `json:"select"`
		Create []createCase `json:"create"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Select, v.Create
}

func tree(t *testing.T, raw []byte) any {
	t.Helper()
	o, err := wbcrud.ParseObjectCanon([]byte(`{"v": ` + string(raw) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := o.Get("v")
	return v
}

func py(t *testing.T, raw []byte) string {
	t.Helper()
	b, err := ssestream.Reformat(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dumpT(t *testing.T, v any) string {
	t.Helper()
	s, err := dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func source(t *testing.T, people json.RawMessage) *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("people", tree(t, people))
	return o
}

func TestSelectBestContactMatchesPython(t *testing.T) {
	sel, _ := loadVectors(t)
	for _, c := range sel {
		t.Run(c.Name, func(t *testing.T) {
			s, err := SelectBestContact(source(t, c.People), c.Requested, c.AllowRisky, nil)
			if c.Error != "" {
				if err == nil || err.Msg != c.Error {
					t.Fatalf("err %v, want %q", err, c.Error)
				}
				if got, want := dumpT(t, err.Flags), py(t, c.ContactFlags); got != want {
					t.Fatalf("flags:\n got %s\nwant %s", got, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, want := dumpT(t, s.Selected.Obj()), py(t, c.Selected); got != want {
				t.Fatalf("selected:\n got %s\nwant %s", got, want)
			}
			if got, want := dumpT(t, s.Flags), py(t, c.ContactFlags); got != want {
				t.Fatalf("flags:\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestSuppressedAddressIsNeverEligible(t *testing.T) {
	sel, _ := loadVectors(t)
	all := sel[0] // "all": person_a is the best candidate
	s, err := SelectBestContact(source(t, all.People), "", true, func(e string) bool { return e == "ann.lee@acme.com" })
	if err != nil || s.Selected.PersonID == "person_a" {
		t.Fatalf("suppressed address selected: %+v %v", s, err)
	}
	for _, f := range s.Flags {
		o := f.(*wbcrud.Obj)
		if id, _ := o.Get("person_id"); id == "person_a" {
			if r, _ := o.Get("reason"); r != "suppressed_address" {
				t.Fatalf("reason %v", r)
			}
		}
	}
}

func TestHeaderSafeSubject(t *testing.T) {
	if got := headerSafe("Acme\r\nBcc: x@evil.io\tInc"); got != "Acme Bcc: x@evil.io Inc" {
		t.Fatalf("%q", got)
	}
}
