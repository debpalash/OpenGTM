package outreachsend

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
)

func TestIdemMatchesPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/idem.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Cases []struct {
			WS           string `json:"ws"`
			SequenceID   string `json:"sequence_id"`
			EnrollmentID int    `json:"enrollment_id"`
			Step         int    `json:"step"`
			Expected     string `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || len(d.Cases) == 0 {
		t.Fatalf("vectors: %v n=%d", err, len(d.Cases))
	}
	for _, c := range d.Cases {
		if got := SequenceStepIdem(c.WS, c.SequenceID, c.EnrollmentID, c.Step); got != c.Expected {
			t.Errorf("idem(%q,%q,%d,%d)=%q want %q", c.WS, c.SequenceID, c.EnrollmentID, c.Step, got, c.Expected)
		}
	}
}

type winCase struct {
	Error    *string        `json:"error"`
	Expected any            `json:"expected"`
	Name     string         `json:"name"`
	Now      string         `json:"now"`
	Seq      map[string]any `json:"seq"`
}

func vecSeq(m map[string]any) (outreachstore.Seq, bool) {
	var s outreachstore.Seq
	num := func(k string) (*int, bool) {
		v, ok := m[k]
		if !ok || v == nil {
			return nil, true
		}
		f, isNum := v.(float64)
		if !isNum {
			return nil, false
		}
		i := int(f)
		return &i, true
	}
	var ok bool
	if s.SendWindowStart, ok = num("send_window_start"); !ok {
		return s, false
	}
	if s.SendWindowEnd, ok = num("send_window_end"); !ok {
		return s, false
	}
	if v, has := m["send_window_tz"]; has && v != nil {
		str, isStr := v.(string)
		if !isStr {
			return s, false
		}
		s.SendWindowTZ = &str
	}
	return s, true
}

func TestWindowAndNextSlotMatchVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/window.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		In   []winCase `json:"in_send_window"`
		Next []winCase `json:"next_window_slot"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range d.In {
		seq, ok := vecSeq(c.Seq)
		if c.Error != nil || !ok {
			continue // Python TypeError/ValueError on inputs Go's typed Seq cannot hold
		}
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatal(err)
		}
		if got := InSendWindow(seq, now); got != c.Expected.(bool) {
			t.Errorf("InSendWindow %s: got %v want %v", c.Name, got, c.Expected)
		}
		checked++
	}
	for _, c := range d.Next {
		seq, ok := vecSeq(c.Seq)
		if c.Error != nil || !ok {
			continue
		}
		now, _ := time.Parse(time.RFC3339, c.Now)
		want, err := time.Parse(time.RFC3339Nano, c.Expected.(string))
		if err != nil {
			t.Fatal(err)
		}
		if got := NextWindowSlot(seq, now); !got.Equal(want) {
			t.Errorf("NextWindowSlot %s: got %s want %s", c.Name, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		checked++
	}
	if checked < 2000 {
		t.Fatalf("only %d vectors checked", checked)
	}
}

// Production wiring dials through the SSRF guard with no plaintext allow-list and
// no extra trust roots (those are test-only knobs on smtpclient.Client).
func TestProductionMailerHasNoTestKnobs(t *testing.T) {
	m := NewSMTPMailer("production")
	if m.Client == nil || m.Dial == nil {
		t.Fatal("mailer not fully wired")
	}
	if len(m.Client.PlainHosts) != 0 || m.Client.RootCAs != nil {
		t.Fatalf("test knobs set in production wiring: %v %v", m.Client.PlainHosts, m.Client.RootCAs)
	}
	// the guard refuses loopback even in production wiring
	_, err := m.Send(context.Background(), MailConfig{Host: "127.0.0.1", Port: 25, Email: "a@b.example", Password: "x", UseTLS: true}, "a@b.example", "c@d.example", []byte("x"))
	if err == nil {
		t.Fatal("loopback dial was allowed")
	}
}
