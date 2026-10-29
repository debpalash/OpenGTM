package smtpmail

import (
	"errors"
	"html"
	"regexp"
	"strings"
)

// ErrNoFooter is returned when the mandatory physical-address footer is
// missing; sending is blocked (Python ValueError("no_footer")).
var ErrNoFooter = errors.New("no_footer")

var (
	tagRe       = regexp.MustCompile(`<[^>]+>`)
	wsBeforeNL  = regexp.MustCompile(`[` + pySpaceClass + `]+\n`)
	spacesTabs  = regexp.MustCompile(`[ \t]+`)
	anyWSRun    = regexp.MustCompile(`[` + pySpaceClass + `]+`)
	pyEscaper   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;")
	footerStyle = `<p style="color:#999;font-size:12px">`
)

// pyHTMLEscape is Python html.escape(s, quote=True).
func pyHTMLEscape(s string) string { return pyEscaper.Replace(s) }

// Build mirrors sending.build_message: it returns the HTML (body + rule +
// escaped footer + unsubscribe link), the plain-text part carrying the same
// footer and link, and the List-Unsubscribe headers. The footer is trimmed.
func Build(footer, unsubURL, bodyHTML string) (htmlOut, text string, headers map[string]string, err error) {
	footer = pyStrip(footer)
	if footer == "" {
		return "", "", nil, ErrNoFooter
	}
	t := tagRe.ReplaceAllString(bodyHTML, "")
	t = html.UnescapeString(t)
	t = wsBeforeNL.ReplaceAllString(t, "\n")
	t = spacesTabs.ReplaceAllString(t, " ")
	t = pyStrip(t)

	htmlOut = bodyHTML +
		`<hr style="border:none;border-top:1px solid #eee;margin:16px 0">` +
		footerStyle + pyHTMLEscape(footer) + `</p>` +
		footerStyle + `<a href="` + pyHTMLEscape(unsubURL) + `">Unsubscribe</a></p>`
	text = t + "\n\n--\n" + footer + "\n\nUnsubscribe: " + unsubURL + "\n"
	headers = map[string]string{
		"List-Unsubscribe":      "<" + unsubURL + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}
	return htmlOut, text, headers, nil
}

// derivePlain is the send_email fallback used when no text part is supplied.
func derivePlain(bodyHTML string) string {
	t := tagRe.ReplaceAllString(bodyHTML, "")
	t = html.UnescapeString(t)
	t = anyWSRun.ReplaceAllString(t, " ")
	return pyStrip(t)
}
