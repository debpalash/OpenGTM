package domainintel

// scoreInput is what the legitimacy score reads.
type scoreInput struct {
	ageDays       int
	hasSPF        bool
	hasDMARC      bool
	emailProvider string
	mx            int // number of MX records kept
	hosting       string
	subdomains    int
}

// score is the 0-100 legitimacy score of analyze_domain (integer arithmetic only, so there is nothing to round).
func score(in scoreInput) int {
	s := 0
	if in.ageDays > 365 {
		s += 25
	}
	if in.ageDays > 1095 {
		s += 15
	}
	if in.hasSPF {
		s += 15
	}
	if in.hasDMARC {
		s += 15
	}
	if in.emailProvider != "" {
		s += 10
	}
	if in.mx > 0 {
		s += 10
	}
	if in.hosting != "" {
		s += 10
	}
	if in.subdomains > 5 {
		s += 5
	}
	return min(s, 100)
}
