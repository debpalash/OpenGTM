package leadscore

// Tier is get_tier: the first SCORE_TIERS entry with low <= score <= high, else "unqualified".
func Tier(score int) string { return TierFloat(float64(score)) }

// TierFloat is get_tier for a non-integer score (Python compares floats the same way, so 24.5 falls
// between the integer tiers and answers "unqualified").
func TierFloat(score float64) string {
	for _, t := range scoreTiers {
		if t.Low <= score && score <= t.Hig {
			return t.Name
		}
	}
	return "unqualified"
}
