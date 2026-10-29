package outreachtoken

import "strings"

// URL returns the unsubscribe link: <base>/api/outreach/unsubscribe?token=<token>,
// base right-stripped of "/".
func (s *Signer) URL(workspaceID, email, sequenceID string) (string, error) {
	tok, err := s.Make(workspaceID, email, sequenceID, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s.base, "/") + "/api/outreach/unsubscribe?token=" + tok, nil
}
