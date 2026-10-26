package chathistory

import "strings"

// AutoTitle is auto_title_from_message: strip, keep the first 60 characters
// and, when the UNSTRIPPED input is longer than 60 characters, cut at the last
// space (a title without a space keeps everything) and append "...".
func AutoTitle(text string) string {
	title := strings.TrimSpace(text)
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60])
	}
	if len([]rune(text)) > 60 {
		if i := strings.LastIndex(title, " "); i >= 0 {
			title = title[:i]
		}
		title += "..."
	}
	if title == "" {
		return "New Chat"
	}
	return title
}
