package destvalidate

import "testing"

// S-L24: substituted values are escaped for Slack mrkdwn, the template itself is not.
func TestRenderSlackTemplateEscapesValues(t *testing.T) {
	got, err := RenderSlackTemplate("<{co}> {note}", map[string]any{"co": "A&B", "note": "<!channel> <https://x|y>"})
	if err != nil {
		t.Fatal(err)
	}
	want := "<A&amp;B> &lt;!channel&gt; &lt;https://x|y&gt;"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
