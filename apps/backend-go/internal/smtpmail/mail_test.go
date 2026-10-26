package smtpmail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

func loadCases(t *testing.T, name string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatalf("%s: no cases", name)
	}
	return doc.Cases
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func TestSanitizeMatchesVectors(t *testing.T) {
	for _, c := range loadCases(t, "sanitize.json") {
		maxLen := 998
		if n, ok := c["max_len"].(json.Number); ok {
			i, _ := n.Int64()
			maxLen = int(i)
		}
		got := SanitizeHeader(pyStr(c["input"]), maxLen)
		if got != c["expected"] {
			t.Errorf("%s: got %q want %q", str(c["name"]), got, c["expected"])
		}
	}
}

func TestValidRecipientMatchesVectors(t *testing.T) {
	for i, c := range loadCases(t, "validate.json") {
		in, _ := c["input"].(string) // null -> "" -> false
		got := ValidRecipient(in)
		if got != c["expected"].(bool) {
			t.Errorf("case %d %q: got %v want %v", i, in, got, c["expected"])
		}
	}
}

func TestRenderMatchesVectors(t *testing.T) {
	for _, c := range loadCases(t, "render.json") {
		vars, _ := c["variables"].(map[string]any)
		got := Render(str(c["template"]), vars)
		if got != c["expected"] {
			t.Errorf("%s: got %q want %q", str(c["name"]), got, c["expected"])
		}
	}
}

func TestLeadVariables(t *testing.T) {
	s := func(v string) *string { return &v }
	score := int64(0)
	v := LeadVariables(leadsread.Lead{Company: "Acme", ContactPerson: s("Pat"), ContactTitle: s("CTO"), Score: &score})
	got := Render("{{company}} / {{name}} / {{title}} / {{score}} / {{city}}|", v)
	if got != "Acme / Pat / CTO / 0 / |" {
		t.Fatalf("got %q", got)
	}
	for _, k := range []string{"company", "email", "name", "contact_person", "city", "phone", "website", "specialization", "company_size", "score", "title"} {
		if _, ok := v[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
}

func TestBuildMatchesVectors(t *testing.T) {
	for _, c := range loadCases(t, "build_message.json") {
		name := str(c["name"])
		exp := c["expected"].(map[string]any)
		if exp["error"] != nil {
			continue // covered by TestBuildNoFooter
		}
		eh := exp["headers"].(map[string]any)
		unsub := strings.TrimSuffix(strings.TrimPrefix(str(eh["List-Unsubscribe"]), "<"), ">")
		html, text, headers, err := Build(str(c["footer"]), unsub, str(c["body_html"]))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if html != exp["html"] {
			t.Errorf("%s html:\n got %q\nwant %q", name, html, exp["html"])
		}
		if text != exp["text"] {
			t.Errorf("%s text:\n got %q\nwant %q", name, text, exp["text"])
		}
		if len(headers) != len(eh) {
			t.Errorf("%s headers: %v", name, headers)
		}
		for k, v := range eh {
			if headers[k] != v {
				t.Errorf("%s header %s: got %q want %q", name, k, headers[k], v)
			}
		}
	}
}

func TestBuildNoFooter(t *testing.T) {
	n := 0
	for _, c := range loadCases(t, "build_message.json") {
		exp := c["expected"].(map[string]any)
		if exp["error"] == nil {
			continue
		}
		n++
		_, _, _, err := Build(str(c["footer"]), "https://x.example/u", str(c["body_html"]))
		if !errors.Is(err, ErrNoFooter) {
			t.Errorf("%s: err=%v", str(c["name"]), err)
		}
	}
	if n == 0 {
		t.Fatal("no no_footer vectors")
	}
	if _, _, _, err := Build("  \n\t", "u", "<p>x</p>"); !errors.Is(err, ErrNoFooter) {
		t.Fatalf("whitespace footer: %v", err)
	}
}

// parsed is a decoded multipart/alternative message.
type parsed struct {
	hdr   mail.Header
	parts []string
	ctype []string
	raw   string
}

func parseMsg(t *testing.T, raw []byte) parsed {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v\n%s", err, raw)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content-type %q %v", m.Header.Get("Content-Type"), err)
	}
	p := parsed{hdr: m.Header, raw: string(raw)}
	mr := multipart.NewReader(m.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		b, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "base64") {
			b, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b)), ""))
			if err != nil {
				t.Fatal(err)
			}
		}
		p.parts = append(p.parts, strings.ReplaceAll(string(b), "\r\n", "\n"))
		p.ctype = append(p.ctype, part.Header.Get("Content-Type"))
	}
	return p
}

func decodeHdr(t *testing.T, v string) string {
	t.Helper()
	d, err := new(mime.WordDecoder).DecodeHeader(v)
	if err != nil {
		t.Fatalf("decode %q: %v", v, err)
	}
	return d
}

func TestComposeHeadersAndStructure(t *testing.T) {
	ok := 0
	for _, c := range loadCases(t, "mime.json") {
		res := c["result"].(map[string]any)
		if res["success"] != true {
			continue
		}
		ok++
		name := str(c["name"])
		cfg := c["config"].(map[string]any)
		var hdrs map[string]string
		if h, _ := c["headers"].(map[string]any); h != nil {
			hdrs = map[string]string{}
			for k, v := range h {
				hdrs[k] = str(v)
			}
		}
		raw, _ := Compose(Message{
			FromEmail: str(cfg["email"]), FromName: str(cfg["from_name"]), To: str(c["to_email"]),
			Subject: str(c["subject"]), HTML: str(c["body_html"]), Text: str(c["body_text"]), Headers: hdrs,
		})
		got := parseMsg(t, raw)
		py := parseMsg(t, []byte(c["smtp"].(map[string]any)["data"].(string)))

		if len(got.parts) != 2 {
			t.Fatalf("%s: parts=%d", name, len(got.parts))
		}
		if !strings.HasPrefix(got.ctype[0], "text/plain") || !strings.HasPrefix(got.ctype[1], "text/html") {
			t.Errorf("%s: part types %v", name, got.ctype)
		}
		for i := range got.ctype {
			if _, p, _ := mime.ParseMediaType(got.ctype[i]); !strings.EqualFold(p["charset"], "utf-8") {
				t.Errorf("%s: charset %q", name, got.ctype[i])
			}
		}
		for i := range got.parts {
			if got.parts[i] != py.parts[i] {
				t.Errorf("%s: part %d:\n got %q\nwant %q", name, i, got.parts[i], py.parts[i])
			}
		}
		if g, w := decodeHdr(t, got.hdr.Get("Subject")), decodeHdr(t, py.hdr.Get("Subject")); g != w {
			t.Errorf("%s: subject %q vs %q", name, g, w)
		}
		if got.hdr.Get("To") != str(c["to_email"]) {
			t.Errorf("%s: To %q", name, got.hdr.Get("To"))
		}
		wantName := SanitizeHeader(str(cfg["from_name"]), 200)
		if wantName == "" {
			wantName = "OpenGTM"
		}
		if str(cfg["email"]) == "justlocal" {
			if !strings.HasSuffix(got.hdr.Get("From"), "<justlocal>") {
				t.Errorf("%s: From %q", name, got.hdr.Get("From"))
			}
		} else {
			a, err := mail.ParseAddress(got.hdr.Get("From"))
			if err != nil || a.Name != wantName || a.Address != str(cfg["email"]) {
				t.Errorf("%s: From %q -> %+v %v want name %q", name, got.hdr.Get("From"), a, err, wantName)
			}
		}
		allowed := map[string]bool{"Content-Type": true, "Mime-Version": true, "From": true, "To": true, "Subject": true, "Message-Id": true}
		for k := range hdrs {
			allowed[canon(k)] = true
		}
		for k := range got.hdr {
			if !allowed[k] {
				t.Errorf("%s: unexpected header %q (injection?)", name, k)
			}
		}
		for k := range hdrs {
			if g, w := decodeHdr(t, got.hdr.Get(k)), decodeHdr(t, py.hdr.Get(k)); g != w {
				t.Errorf("%s: header %s %q vs %q", name, k, g, w)
			}
		}
		if strings.Contains(got.raw, "\nBcc:") || strings.Contains(got.raw, "\nInjected:") {
			t.Errorf("%s: header injection", name)
		}
		mid := got.hdr.Get("Message-Id")
		wantDom := str(res["message_id"])
		wantDom = wantDom[strings.LastIndex(wantDom, "@")+1 : len(wantDom)-1]
		if !strings.HasSuffix(mid, "@"+wantDom+">") {
			t.Errorf("%s: Message-ID %q want domain %q", name, mid, wantDom)
		}
	}
	if ok < 15 {
		t.Fatalf("only %d vectors", ok)
	}
}

func canon(k string) string {
	parts := strings.Split(strings.ToLower(k), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

func TestComposeCRLFInjectionAttempts(t *testing.T) {
	raw, _ := Compose(Message{
		FromEmail: "s@example.com", FromName: "Evil\r\nBcc: x@example.com", To: "to@example.com",
		Subject: "a\r\nBcc: evil@example.com", HTML: "<p>x</p>", Text: "x",
		Headers: map[string]string{"X-A": "v\r\nInjected: 1", "Bad Name\r\nX": "z"},
	})
	p := parseMsg(t, raw)
	for _, k := range []string{"Bcc", "Injected", "X"} {
		if _, ok := p.hdr[k]; ok {
			t.Fatalf("injected header %s", k)
		}
	}
	if got := p.hdr.Get("Subject"); got != "aBcc: evil@example.com" {
		t.Fatalf("subject %q", got)
	}
}

func TestComposeDerivesPlainText(t *testing.T) {
	raw, _ := Compose(Message{FromEmail: "s@example.com", To: "to@example.com", Subject: "S", HTML: "<p>Hi&nbsp;<b>there</b> &amp; you</p>\n<p>bye</p>"})
	p := parseMsg(t, raw)
	if p.parts[0] != "Hi there & you bye" {
		t.Fatalf("%q", p.parts[0])
	}
}

var msgIDRe = regexp.MustCompile(`^<(\d+)\.(\d+)\.(\d+)@([^<>@\s]+)>$`)

func TestMessageIDShapeAndDomain(t *testing.T) {
	for email, dom := range map[string]string{"a@mail.example.org": "mail.example.org", "justlocal": "localhost", "x@y@z.com": "z.com"} {
		_, id := Compose(Message{FromEmail: email, To: "t@example.com", Subject: "s", HTML: "x", Text: "x"})
		m := msgIDRe.FindStringSubmatch(id)
		if m == nil || m[4] != dom {
			t.Errorf("%s: id %q", email, id)
		}
		raw, _ := Compose(Message{FromEmail: email, To: "t@example.com", Subject: "s", HTML: "x", Text: "x"})
		if !strings.Contains(string(raw), "Message-ID: <") {
			t.Errorf("no Message-ID header")
		}
	}
}

func TestMessageIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		_, id := Compose(Message{FromEmail: "a@example.com", To: "t@example.com", Subject: "s", HTML: "x", Text: "x"})
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %s at %d", id, i)
		}
		seen[id] = struct{}{}
	}
}
