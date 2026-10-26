package domainintel

import "testing"

func TestScore(t *testing.T) {
	full := scoreInput{ageDays: 5000, hasSPF: true, hasDMARC: true, emailProvider: "x", mx: 1, hosting: "y", subdomains: 6}
	if got := score(full); got != 100 {
		t.Fatalf("full = %d", got)
	}
	for age, want := range map[int]int{-5: 0, 0: 0, 365: 0, 366: 25, 1095: 25, 1096: 40} {
		if got := score(scoreInput{ageDays: age}); got != want {
			t.Errorf("age %d = %d want %d", age, got, want)
		}
	}
	if got := score(scoreInput{subdomains: 5}); got != 0 {
		t.Errorf("5 subdomains = %d", got)
	}
	if got := score(scoreInput{subdomains: 6}); got != 5 {
		t.Errorf("6 subdomains = %d", got)
	}
}
