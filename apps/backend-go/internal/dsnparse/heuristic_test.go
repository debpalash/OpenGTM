package dsnparse

import "testing"

func heurMsg(subject, body string) []byte {
	return []byte("From: p@mx.example.net\r\nSubject: " + subject + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

// F24: reply codes and phrases count only on diagnostic lines.
func TestHeuristicHardNeedsDiagnosticLine(t *testing.T) {
	kinds := func(raw []byte) []string {
		var ks []string
		for _, r := range Parse(raw) {
			ks = append(ks, r.Kind)
		}
		return ks
	}
	cases := []struct {
		name, subject, body, want string
	}{
		{"order number", "Undeliverable: order", "Your order #5501 could not be delivered.\nSee you soon", "unknown"},
		{"phrase in prose", "Undeliverable: x", "The page does not exist anymore, sorry.", "unknown"},
		{"reply code line", "Undeliverable: x", "Your message could not be delivered.\n550 5.1.1 no such mailbox", "hard"},
		{"diagnostic-code field", "Undeliverable: x", "Diagnostic-Code: smtp; 554 5.7.1 rejected", "hard"},
		{"version string", "Undeliverable: x", "Status: v15.1.100 build", "unknown"},
		{"phrase-led line", "Undeliverable: x", "text\nUser unknown\ntext", "hard"},
		{"gmail 550-", "Undeliverable: x", "550-5.1.1 The email account that you tried to reach does not exist.", "hard"},
	}
	for _, c := range cases {
		got := kinds(heurMsg(c.subject, c.body))
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %v, want [%s]", c.name, got, c.want)
		}
	}
}
