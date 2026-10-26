package leadevents

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vectors struct {
	Events []struct {
		Raw     string `json:"raw"`
		Encoded string `json:"encoded"`
		Frame   string `json:"frame"`
	} `json:"events"`
	Invalid []struct {
		Raw string `json:"raw"`
	} `json:"invalid"`
	HeartbeatFrame string `json:"heartbeat_frame"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/encode.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEncodeEventMatchesPython(t *testing.T) {
	v := loadVectors(t)
	if len(v.Events) < 12 {
		t.Fatalf("want >=12 vectors, got %d", len(v.Events))
	}
	for i, e := range v.Events {
		got, err := EncodeEvent([]byte(e.Raw))
		if err != nil {
			t.Fatalf("vector %d %q: %v", i, e.Raw, err)
		}
		if string(got) != e.Encoded {
			t.Errorf("vector %d:\n got %s\nwant %s", i, got, e.Encoded)
		}
		if string(Frame(got)) != e.Frame {
			t.Errorf("vector %d frame: %q want %q", i, Frame(got), e.Frame)
		}
	}
	for _, in := range v.Invalid {
		if _, err := EncodeEvent([]byte(in.Raw)); err == nil {
			t.Errorf("invalid %q accepted", in.Raw)
		}
	}
}

func TestEncodeEventKeepsKeyOrderAndFloats(t *testing.T) {
	got, err := EncodeEvent([]byte(`{"z":1.0,"a":[0.1,1e16,1e-7],"m":{"b":2,"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"z": 1.0, "a": [0.1, 1e+16, 1e-07], "m": {"b": 2, "a": 1}}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestEncodeEventEdgeCases(t *testing.T) {
	cases := map[string]string{
		`-0`:                `0`,
		`-0.0`:              `-0.0`,
		`1e999`:             `Infinity`,
		` {"a" : [ ] } `:    `{"a": []}`,
		`"\ud800"`:          `"\ud800"`,
		`{"a":{},"b":[{}]}`: `{"a": {}, "b": [{}]}`,
	}
	for in, want := range cases {
		got, err := EncodeEvent([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q err %v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`01`, `{"a":1,}`, `[1,]`, "\"a\nb\"", `{"a"}`, `1 2`, `tru`, `"\x"`, strings.Repeat("[", 2000)} {
		if _, err := EncodeEvent([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad[:min(len(bad), 20)])
		}
	}
}

func TestHeartbeatBytes(t *testing.T) {
	v := loadVectors(t)
	if string(HeartbeatFrame()) != v.HeartbeatFrame || string(HeartbeatFrame()) != "data: {\"type\": \"heartbeat\"}\n\n" {
		t.Fatalf("heartbeat %q", HeartbeatFrame())
	}
	if Channel("ws-1") != "leadgen:progress:ws-1" {
		t.Fatal("channel")
	}
}
