package dsnparse

import "strings"

// parseARF is _parse_arf (RFC 5965 feedback report): always one complaint.
func parseARF(root *part) ([]Record, error) {
	fb, orig := findParts(root, "message/feedback-report")
	mid, err := originalMessageID(orig)
	if err != nil {
		return nil, err
	}
	recipient := ""
	feedbackType := "abuse"
	if fb != nil {
		for _, block := range statusBlocks(fb) {
			v, err := headerStr(block, "original-rcpt-to")
			if err != nil {
				return nil, err
			}
			if a := addr(v); a != "" {
				recipient = a
			}
			ft, err := headerStr(block, "feedback-type")
			if err != nil {
				return nil, err
			}
			if ft != "" {
				feedbackType = ft
			}
			feedbackType = pyStrip(feedbackType)
		}
	}
	if recipient == "" {
		if recipient, err = embeddedRecipient(orig); err != nil {
			return nil, err
		}
	}
	return []Record{{Recipient: recipient, Kind: "complaint", Diagnostic: "arf feedback-type=" + feedbackType, OriginalMessageID: mid}}, nil
}

// embeddedRecipient is _embedded_recipient: the To address of the embedded original.
func embeddedRecipient(orig *part) (string, error) {
	if orig == nil || orig.contentType() != "message/rfc822" {
		return "", nil
	}
	sub := firstChild(orig)
	if sub == nil {
		return "", nil
	}
	to, err := headerStr(sub, "to")
	if err != nil {
		return "", err
	}
	return strings.ToLower(emailRE.FindString(to)), nil
}
