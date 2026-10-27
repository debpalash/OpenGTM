package leadpeople

import "testing"

func TestNormalizeDomainVectors(t *testing.T) {
	var cases []struct{ In, Out string }
	loadVec(t, "domains.json", &cases)
	for _, c := range cases {
		if got := NormalizeDomain(c.In); got != c.Out {
			t.Errorf("NormalizeDomain(%q) = %q want %q", c.In, got, c.Out)
		}
	}
}
