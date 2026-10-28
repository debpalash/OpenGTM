package aicol

import "testing"

// Final security Low 9: research fetch without a URLCheck still vets URLs (fail closed), it never allows all.
func TestCheckURLWithoutDepsFailsClosed(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1/", "http://169.254.169.254/latest/meta-data/", "file:///etc/passwd"} {
		if ok, _ := (Deps{}).checkURL(u); ok {
			t.Errorf("%s allowed without a URLCheck", u)
		}
	}
}
