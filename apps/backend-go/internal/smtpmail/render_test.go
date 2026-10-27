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

// S-L16: a placeholder that starts an href or src value must be an http(s) or mailto URL.
func TestRenderHTMLDropsUnsafeURLSchemes(t *testing.T) {
	cases := []struct{ tpl, val, want string }{
		{`<a href="{{website}}">x</a>`, `javascript:alert(1)`, `<a href="">x</a>`},
		{`<a href='{{website}}'>x</a>`, `JaVaScRiPt:alert(1)`, `<a href=''>x</a>`},
		{`<a href={{website}}>x</a>`, `data:text/html,x`, `<a href=>x</a>`},
		{`<img SRC = "{{website}}">`, ` javascript:x`, `<img SRC = "">`},
		{`<a class="b" href="{{website}}">x</a>`, `vbscript:x`, `<a class="b" href="">x</a>`},
		{`<a href="{{website}}">x</a>`, `https://ok.example/a?b=1&c=2`, `<a href="https://ok.example/a?b=1&amp;c=2">x</a>`},
		{`<a href="{{website}}">x</a>`, `mailto:a@b.co`, `<a href="mailto:a@b.co">x</a>`},
		{`<a href="https://x.example/{{website}}">x</a>`, `javascript:1`, `<a href="https://x.example/javascript:1">x</a>`},
		{`<p title="{{website}}">{{website}}</p>`, `javascript:1`, `<p title="javascript:1">javascript:1</p>`},
	}
	for _, c := range cases {
		if got := RenderHTML(c.tpl, map[string]any{"website": c.val}); got != c.want {
			t.Errorf("%s with %q: got %q want %q", c.tpl, c.val, got, c.want)
		}
	}
}
