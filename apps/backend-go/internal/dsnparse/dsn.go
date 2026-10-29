package dsnparse

import "strings"

// classifyStatus is _classify_status; ok is false for Python's None.
func classifyStatus(status, action string) (string, bool) {
	if strings.HasPrefix(status, "5.") {
		return "hard", true
	}
	if strings.HasPrefix(status, "4.") {
		return "soft", true
	}
	switch action {
	case "failed":
		return "hard", true
	case "delayed":
		return "soft", true
	}
	return "", false
}

// findParts returns the first message/<primary> part and the first original
// message part (message/rfc822 or text/rfc822-headers) in walk order.
func findParts(root *part, primary string) (main, orig *part) {
	walk(root, func(p *part) {
		switch ct := p.contentType(); {
		case ct == primary && main == nil:
			main = p
		case (ct == "message/rfc822" || ct == "text/rfc822-headers") && orig == nil:
			orig = p
		}
	})
	return main, orig
}

// parseDSN is _parse_dsn (RFC 3464 per-recipient blocks).
func parseDSN(root *part) ([]Record, error) {
	ds, orig := findParts(root, "message/delivery-status")
	if ds == nil {
		return nil, nil
	}
	mid, err := originalMessageID(orig)
	if err != nil {
		return nil, err
	}
	var recs []Record
	for _, block := range statusBlocks(ds) {
		st, err := headerStr(block, "status")
		if err != nil {
			return nil, err
		}
		status := pyStrip(st)
		ac, err := headerStr(block, "action")
		if err != nil {
			return nil, err
		}
		action := strings.ToLower(pyStrip(ac))
		fr, err := headerStr(block, "final-recipient")
		if err != nil {
			return nil, err
		}
		if fr == "" {
			if fr, err = headerStr(block, "original-recipient"); err != nil {
				return nil, err
			}
		}
		rcpt := addr(fr)
		dg, err := headerStr(block, "diagnostic-code")
		if err != nil {
			return nil, err
		}
		diag := pyStrip(dg)
		if len(diag) > 500 {
			diag = diag[:500]
		}
		kind, ok := classifyStatus(status, action)
		if !ok {
			continue // per-message block (Reporting-MTA only)
		}
		if diag == "" {
			diag = "dsn status=" + status + " action=" + action
		}
		recs = append(recs, Record{Recipient: rcpt, Kind: kind, Diagnostic: diag, OriginalMessageID: mid})
	}
	return recs, nil
}
