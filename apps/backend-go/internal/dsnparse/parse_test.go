package dsnparse

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const corpusDir = "../../testdata/dsn"

type expectedRecord struct {
	Diagnostic        string `json:"diagnostic"`
	Kind              string `json:"kind"`
	OriginalMessageID string `json:"original_message_id"`
	Recipient         string `json:"recipient"`
}

type expected struct {
	Name            string           `json:"name"`
	PythonError     *string          `json:"python_error"`
	Records         []expectedRecord `json:"records"`
	SourceMessageID string           `json:"source_message_id"`
}

type manifest struct {
	Files []struct {
		Name      string `json:"name"`
		GoBounded bool   `json:"go_bounded"`
	} `json:"files"`
}

func readJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func corpus(t testing.TB) (names []string, bounded map[string]bool) {
	t.Helper()
	var m manifest
	readJSON(t, "testdata/manifest.json", &m)
	bounded = map[string]bool{}
	for _, f := range m.Files {
		names = append(names, f.Name)
		bounded[f.Name] = f.GoBounded
	}
	if len(names) != 59 {
		t.Fatalf("corpus has %d files, want 59", len(names))
	}
	return names, bounded
}

func toRecords(in []expectedRecord) []Record {
	var out []Record
	for _, r := range in {
		out = append(out, Record{Recipient: r.Recipient, Kind: r.Kind, Diagnostic: r.Diagnostic, OriginalMessageID: r.OriginalMessageID})
	}
	return out
}

func TestParseMatchesPythonCorpus(t *testing.T) {
	names, bounded := corpus(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(corpusDir, name+".eml"))
			if err != nil {
				t.Fatal(err)
			}
			var exp expected
			readJSON(t, filepath.Join(corpusDir, name+".json"), &exp)
			got := Parse(raw)
			switch {
			case bounded[name]:
				// Python walks 12 wrappers; Go refuses beyond depth 8 (D6) so the
				// nested DSN is ignored and only the heuristic on the top-level text remains.
				want := []Record{{Kind: "unknown", Diagnostic: "heuristic plain-text bounce"}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("bounded: got %+v, want %+v", got, want)
				}
			case exp.PythonError != nil:
				// Python raises (AttributeError: a non-ASCII header value is an
				// email.header.Header with no str methods) and the exception escapes
				// parse_message; Go returns no records, the observable outcome.
				if got != nil {
					t.Fatalf("python raised %s, Go must return nil, got %+v", *exp.PythonError, got)
				}
			default:
				if want := toRecords(exp.Records); !reflect.DeepEqual(got, want) {
					t.Fatalf("records differ\n got: %+v\nwant: %+v", got, want)
				}
			}
		})
	}
}

func TestSourceMessageIDMatchesCorpus(t *testing.T) {
	names, _ := corpus(t)
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(corpusDir, name+".eml"))
		if err != nil {
			t.Fatal(err)
		}
		var exp expected
		readJSON(t, filepath.Join(corpusDir, name+".json"), &exp)
		if got := SourceMessageID(raw); got != exp.SourceMessageID {
			t.Errorf("%s: SourceMessageID = %q, want %q", name, got, exp.SourceMessageID)
		}
	}
}

func TestClassifyStatusVectors(t *testing.T) {
	var vectors []struct {
		Status string  `json:"status"`
		Action string  `json:"action"`
		Kind   *string `json:"kind"`
	}
	readJSON(t, "testdata/classify_vectors.json", &vectors)
	if len(vectors) < 100 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	for _, v := range vectors {
		kind, ok := classifyStatus(v.Status, v.Action)
		switch {
		case v.Kind == nil && ok:
			t.Errorf("classifyStatus(%q,%q) = %q, want none", v.Status, v.Action, kind)
		case v.Kind != nil && (!ok || kind != *v.Kind):
			t.Errorf("classifyStatus(%q,%q) = %q/%v, want %q", v.Status, v.Action, kind, ok, *v.Kind)
		}
	}
}

func TestMalformedInputNeverPanics(t *testing.T) {
	names, _ := corpus(t)
	rng := rand.New(rand.NewSource(1))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(corpusDir, name+".eml"))
		if err != nil {
			t.Fatal(err)
		}
		// Every prefix of small files, sampled prefixes of large ones.
		step := 1
		if len(raw) > 2048 {
			step = len(raw)/512 + 1
		}
		for n := 0; n <= len(raw); n += step {
			Parse(raw[:n])
			SourceMessageID(raw[:n])
		}
		// Byte-level mutations.
		for i := 0; i < 20 && len(raw) > 0; i++ {
			m := append([]byte(nil), raw...)
			for k := 0; k < 1+rng.Intn(8); k++ {
				m[rng.Intn(len(m))] = byte(rng.Intn(256))
			}
			Parse(m)
			SourceMessageID(m)
		}
	}
	for _, s := range []string{"", "\r", "\n", "--", ":", "From ", "Content-Type: multipart/report; boundary=", "Content-Type: multipart/report; boundary=\"\n\n--\n--"} {
		Parse([]byte(s))
	}
}

func FuzzParse(f *testing.F) {
	names, _ := corpus(f)
	for _, name := range names {
		if raw, err := os.ReadFile(filepath.Join(corpusDir, name+".eml")); err == nil && len(raw) < 4096 {
			f.Add(raw)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		Parse(raw)
		SourceMessageID(raw)
	})
}

func TestOversizedInputBounded(t *testing.T) {
	head := "From: a@example.com\r\nSubject: hello\r\nContent-Type: text/plain\r\n\r\n"
	// The hard token sits after the 1 MiB cut-off, so it must never be seen.
	body := strings.Repeat("filler line without any marker\r\n", (1<<20)/32+10)
	raw := []byte(head + body + "550 user unknown\r\n")
	if len(raw) <= 1<<20 {
		t.Fatal("test input too small")
	}
	start := time.Now()
	if got := Parse(raw); got != nil {
		t.Fatalf("content past 1 MiB influenced the result: %+v", got)
	}
	// A huge input is cut, not rejected: a marker inside the first MiB still counts.
	raw2 := []byte(head + "550 user unknown\r\n" + body + body + body + body)
	got := Parse(raw2)
	if len(got) != 1 || got[0].Kind != "hard" {
		t.Fatalf("marker inside the first MiB not found: %+v", got)
	}
	if !reflect.DeepEqual(got, Parse(raw2[:1<<20])) {
		t.Fatal("Parse must equal Parse of the first MiB")
	}
	// Pathological shapes within the bound finish quickly.
	for _, raw := range [][]byte{
		[]byte("Content-Type: multipart/report; boundary=b" + strings.Repeat("; x=\"", 100000) + "\r\n\r\n"),
		[]byte(strings.Repeat("X-A: 1\r\n", 200000) + "\r\n550"),
		[]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" + strings.Repeat("--b\r\n", 200000)),
		[]byte(strings.Repeat("\r", 1<<20)),
	} {
		Parse(raw)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("bounded parsing took %v", d)
	}
}

// nested wraps a DSN in n multipart/mixed levels beneath a multipart/report root.
func nested(n int) []byte {
	var b strings.Builder
	b.WriteString("From: mailer-daemon@mx.example.net\r\nSubject: nested\r\nMessage-ID: <nested@mx.example.net>\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"B0\"\r\n\r\n--B0\r\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"B%d\"\r\n\r\n--B%d\r\n", i, i)
	}
	b.WriteString("Content-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.example.net\r\n\r\n" +
		"Final-Recipient: rfc822; deep@example.org\r\nAction: failed\r\nStatus: 5.1.1\r\nDiagnostic-Code: smtp; 550 5.1.1 deep\r\n\r\n")
	for i := n; i >= 0; i-- {
		fmt.Fprintf(&b, "--B%d--\r\n", i)
	}
	return []byte(b.String())
}

func TestDeepNestingBounded(t *testing.T) {
	// The delivery-status part sits at depth n+1 (root is 0): parsed up to 8, ignored beyond.
	for n, wantDSN := range map[int]bool{0: true, 3: true, 6: true, 7: true, 8: false, 9: false, 40: false, 3000: false} {
		got := Parse(nested(n))
		isDSN := len(got) == 1 && got[0].Recipient == "deep@example.org" && got[0].Kind == "hard"
		if isDSN != wantDSN {
			t.Errorf("depth %d: dsn parsed = %v, want %v (records %+v)", n, isDSN, wantDSN, got)
		}
	}
	// message/rfc822 chains are bounded the same way.
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("Content-Type: message/rfc822\r\n\r\n")
	}
	b.WriteString("Subject: x\r\n\r\n550 user unknown\r\n")
	Parse([]byte(b.String()))
}
