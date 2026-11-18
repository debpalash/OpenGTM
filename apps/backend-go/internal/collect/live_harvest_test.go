package collect

import "testing"

// The honest harvest class (role/free/found from tagHarvestedEmails) must survive a
// deliverability pass that produced no verdict: "unknown" means no verification
// vendor was configured or the cascade failed — it says nothing about the address,
// so it must not erase what we already know. Every real verdict still wins.
func TestKeepHarvestClass(t *testing.T) {
	cases := []struct {
		name string
		prev string
		keep bool
		conf string
		want string
	}{
		{"unknown keeps role class", "role", true, "unknown", "role"},
		{"unknown keeps free class", "free", true, "unknown", "free"},
		{"unknown keeps found class", "found", true, "unknown", "found"},
		{"unknown with no prev stays unknown", "", true, "unknown", "unknown"},
		{"unknown does not resurrect unknown", "unknown", true, "unknown", "unknown"},
		{"verified wins over role", "role", true, "verified", "verified"},
		{"risky wins over found", "found", true, "risky", "risky"},
		{"verified wins over empty", "", true, "verified", "verified"},
	}
	for _, c := range cases {
		if got := keepHarvestClass(c.prev, c.keep, c.conf); got != c.want {
			t.Errorf("%s: keepHarvestClass(%q,%v,%q)=%q want %q", c.name, c.prev, c.keep, c.conf, got, c.want)
		}
	}
}
