package signals_test

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signals"
)

type cursorVectors struct {
	Encode []struct {
		Repr   string `json:"created_at_repr"`
		ID     string `json:"id"`
		Cursor string `json:"cursor"`
	} `json:"encode"`
	Decode []struct {
		Name   string `json:"name"`
		Cursor string `json:"cursor"`
		OK     bool   `json:"ok"`
		Repr   string `json:"created_at_repr"`
		ID     string `json:"id"`
		Detail string `json:"detail"`
	} `json:"decode"`
}

func pyRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s, _ := canonjson.FloatRepr(f)
	return s
}

func TestCursorVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/cursors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v cursorVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, e := range v.Encode {
		f, err := strconv.ParseFloat(e.Repr, 64)
		if err != nil {
			t.Fatal(err)
		}
		if e.Repr == "1788000000" { // Python encoded an int here; Go columns are always float
			continue
		}
		if got := signals.EncodeCursor(f, e.ID); got != e.Cursor {
			t.Errorf("encode(%s,%q) = %s want %s", e.Repr, e.ID, got, e.Cursor)
		}
	}
	for _, d := range v.Decode {
		t.Run(d.Name, func(t *testing.T) {
			ts, id, err := signals.DecodeCursor(d.Cursor)
			if !d.OK {
				if err != signals.ErrBadCursor {
					t.Fatalf("want ErrBadCursor, got %v (%v,%q)", err, ts, id)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected %v", err)
			}
			if pyRepr(ts) != d.Repr || id != d.ID {
				t.Fatalf("got (%s,%q) want (%s,%q)", pyRepr(ts), id, d.Repr, d.ID)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := signals.EncodeCursor(1788000000.5, "sig-é")
	ts, id, err := signals.DecodeCursor(c)
	if err != nil || ts != 1788000000.5 || id != "sig-é" {
		t.Fatalf("%v %v %v", ts, id, err)
	}
	// missing padding accepted (encoder never pads), padded input accepted
	if _, _, err := signals.DecodeCursor(c + "=="); err != nil {
		t.Fatal(err)
	}
}
