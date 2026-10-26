package smtpmail

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"os"
	"sort"
	"strings"
	"time"
)

// Message is the input to Compose. Values are sanitised here; callers pass raw
// strings. To must already have passed ValidRecipient.
type Message struct {
	FromEmail string
	FromName  string
	To        string
	Subject   string
	HTML      string
	Text      string // derived from HTML when empty (Python send_email)
	Headers   map[string]string
}

// Compose renders m as a multipart/alternative RFC 5322 message (plain first,
// html second, UTF-8) and returns it with the Message-ID it carries.
func Compose(m Message) (raw []byte, messageID string) {
	name := SanitizeHeader(m.FromName, 200)
	if name == "" {
		name = "OpenGTM"
	}
	text := m.Text
	if text == "" {
		text = derivePlain(m.HTML)
	}
	messageID = newMessageID(m.FromEmail)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	writePart(mw, "text/plain", text)
	writePart(mw, "text/html", m.HTML)
	_ = mw.Close()

	var out bytes.Buffer
	hdr := func(k, v string) { out.WriteString(foldHeader(k, v)) }
	hdr("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	hdr("MIME-Version", "1.0")
	hdr("From", fromHeader(name, SanitizeHeader(m.FromEmail, 998)))
	hdr("To", SanitizeHeader(m.To, 998))
	hdr("Subject", encodeValue(SanitizeHeader(m.Subject, 998)))
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if validHeaderName(k) {
			hdr(k, encodeValue(SanitizeHeader(m.Headers[k], 2000)))
		}
	}
	hdr("Message-ID", messageID)
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	return out.Bytes(), messageID
}

func writePart(mw *multipart.Writer, ctype, content string) {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ctype+`; charset="utf-8"`)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	w, _ := mw.CreatePart(h)
	qw := quotedprintable.NewWriter(w)
	_, _ = qw.Write([]byte(content))
	_ = qw.Close()
}

func validHeaderName(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// encodeValue RFC 2047-encodes values that are non-ASCII or contain a token
// too long to fold; other values pass through for foldHeader.
func encodeValue(v string) string {
	if isASCII(v) {
		long := false
		for _, w := range strings.Split(v, " ") {
			if len(w) > 900 {
				long = true
			}
		}
		if !long {
			return v
		}
	}
	return mime.QEncoding.Encode("utf-8", v)
}

func fromHeader(name, email string) string {
	if isASCII(name) {
		q := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
		return `"` + q + `" <` + email + `>`
	}
	return mime.QEncoding.Encode("utf-8", name) + " <" + email + ">"
}

// foldHeader writes "Name: value\r\n", folding at spaces past 78 columns.
func foldHeader(name, value string) string {
	var b strings.Builder
	b.WriteString(name + ":")
	line := len(name) + 1
	for _, w := range strings.Split(" "+value, " ")[1:] {
		if line+1+len(w) > 78 && line > len(name)+1 && w != "" {
			b.WriteString("\r\n")
			line = 0
		}
		b.WriteString(" " + w)
		line += 1 + len(w)
	}
	b.WriteString("\r\n")
	return b.String()
}

// newMessageID mimics email.utils.make_msgid: <unixtime.pid.random@domain>,
// with a 63-bit random component instead of Python's randint(0, 100000).
func newMessageID(fromEmail string) string {
	domain := "localhost"
	if i := strings.LastIndexByte(fromEmail, '@'); i >= 0 {
		var b strings.Builder
		for _, r := range fromEmail[i+1:] {
			if r > 32 && r < 127 && !strings.ContainsRune("<>@()[],;:\\\"", r) {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			domain = b.String()
		}
	}
	var rb [8]byte
	_, _ = rand.Read(rb[:])
	n := binary.BigEndian.Uint64(rb[:]) >> 1
	return fmt.Sprintf("<%d.%d.%d@%s>", time.Now().Unix(), os.Getpid(), n, domain)
}
