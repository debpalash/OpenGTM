package smtpmail

import "testing"

// Plan 09 final security M3: lead fields are escaped in the HTML part only.
func TestRenderHTMLEscapesValuesNotTemplate(t *testing.T) {
	vars := map[string]any{"company": `<a href="https://evil">Reset your password</a> & co`, "n": 5, "email": nil}
	got := RenderHTML(`<p>Hi {{company}} {{n}}{{email}}</p>`, vars)
	want := `<p>Hi &lt;a href=&#34;https://evil&#34;&gt;Reset your password&lt;/a&gt; &amp; co 5</p>`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if raw := Render(`{{company}}`, vars); raw != vars["company"] {
		t.Fatalf("Render must stay raw: %q", raw)
	}
}
