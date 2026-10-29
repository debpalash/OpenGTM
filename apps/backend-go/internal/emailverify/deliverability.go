package emailverify

import "context"

// Confidence tags written to a lead's email_confidence.
const (
	ConfVerified = "verified" // the mailbox is confirmed deliverable
	ConfRisky    = "risky"    // catch-all domain, or a role mailbox
	ConfUnknown  = "unknown"  // could not be determined
)

// DeliverabilityResult is DeliverabilityResult of email_deliverability.py, in
// Python's field order. Keep is derived (Confidence != "").
type DeliverabilityResult struct {
	Email                string    `json:"email"`
	Confidence           string    `json:"confidence"` // verified | risky | unknown | "" (drop)
	Status               string    `json:"status"`     // raw cascade status
	Source               string    `json:"source"`
	VerificationConf     float64   `json:"verification_confidence"`
	IsRole               bool      `json:"is_role"`
	IsFree               bool      `json:"is_free"`
	IsDisposable         bool      `json:"is_disposable"`
	CatchAll             bool      `json:"catch_all"`
	VerificationAttempts []Attempt `json:"verification_attempts"`
	Keep                 bool      `json:"keep"`
}

// Deliverability is verify_deliverability: classify (no network), hard-drop bad
// syntax and disposable domains without any probe, otherwise run the cascade and
// map the verdict to a confidence tag. It never fails because verification did
// (that is "unknown"); the error is non-nil only when ctx ends. The brief's
// (domain) signature was widened to the address: that is what Python (and the
// vectors) take, and MX/SPF/DMARC records play no part in the verdict.
func Deliverability(ctx context.Context, d Deps, workspaceID, email string) (DeliverabilityResult, error) {
	cls := Classify(email)
	res := DeliverabilityResult{Email: email, IsRole: cls.IsRole, IsFree: cls.IsFree, IsDisposable: cls.IsDisposable,
		Confidence: ConfUnknown, VerificationAttempts: []Attempt{}}
	finish := func() (DeliverabilityResult, error) { res.Keep = res.Confidence != ""; return res, nil }
	if !cls.ValidSyntax {
		res.Confidence, res.Status, res.Source = "", StatusInvalid, "syntax"
		return finish()
	}
	if cls.IsDisposable {
		res.Confidence, res.Status, res.Source = "", StatusInvalid, "disposable"
		return finish()
	}
	cascade := Cascade
	if d.cascade != nil {
		cascade = d.cascade
	}
	verdict, err := cascade(ctx, d, workspaceID, email)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return DeliverabilityResult{}, cerr
		}
		res.Confidence, res.Status = ConfUnknown, StatusUnknown
		return finish()
	}
	res.Status, res.Source = verdict.Status, verdict.Source
	res.VerificationConf = verdict.Confidence
	res.CatchAll = verdict.Status == StatusCatchAll
	if verdict.Attempts != nil {
		res.VerificationAttempts = append([]Attempt{}, verdict.Attempts...)
	}
	switch verdict.Status {
	case StatusInvalid:
		res.Confidence = "" // hard reject: drop
	case StatusCatchAll:
		res.Confidence = ConfRisky
	case StatusValid:
		res.Confidence = ConfVerified
		if cls.IsRole {
			res.Confidence = ConfRisky // a role account is deliverable but not a person
		}
	default:
		res.Confidence = ConfUnknown
	}
	return finish()
}

// hasEmail is Lead.has_email.
func hasEmail(lead map[string]any) bool {
	e, _ := lead["email"].(string)
	return e != "" && e != "N/A" && e != "nan" && containsAt(e)
}

func containsAt(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			return true
		}
	}
	return false
}

// TagEmailConfidence is tag_email_confidence: verify lead["email"] and write the
// tag to lead["email_confidence"]. A hard reject (invalid, disposable, bad
// syntax) clears the email, its provider and the confidence so a known-bad
// address is never shipped. Returns nil when the lead has no email.
func TagEmailConfidence(ctx context.Context, d Deps, workspaceID string, lead map[string]any) (*DeliverabilityResult, error) {
	if !hasEmail(lead) {
		return nil, nil
	}
	email, _ := lead["email"].(string)
	res, err := Deliverability(ctx, d, workspaceID, email)
	if err != nil {
		return nil, err
	}
	if !res.Keep {
		lead["email"], lead["email_confidence"], lead["email_provider"] = "", "", ""
		return &res, nil
	}
	lead["email_confidence"] = res.Confidence
	return &res, nil
}
