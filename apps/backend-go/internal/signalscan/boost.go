package signalscan

// Detected is a scanner-detected signal (monitor.Signal): LeadID 0 means none.
type Detected struct {
	ID, WorkspaceID                                            string
	LeadID                                                     int64
	Company, SignalType, Title, Description, Source, SourceURL string
	Weight                                                     int
}

// ScoreUpdate is one lead score change planned by PlanBoosts.
type ScoreUpdate struct {
	LeadID int64
	Score  int64
	Tier   string
}

// Tier is the score -> tier mapping of _apply_signal_boosts.
func Tier(score int64) string {
	switch {
	case score >= 75:
		return "hot"
	case score >= 50:
		return "warm"
	case score >= 25:
		return "cold"
	}
	return "unqualified"
}

// PlanBoosts is the pure half of monitor._apply_signal_boosts: sum the weight
// per lead (first-seen order, lead id 0 ignored), then per lead raise the score
// by that weight, capped at 100 (a negative weight lowers it). Leads the store
// does not know and unchanged scores yield no update. scoreOf returns the
// lead's current score (nil = NULL, counted as 0) and whether the lead exists.
// Like Python this re-boosts on every rescan: parity, not idempotent.
func PlanBoosts(signals []Detected, scoreOf func(leadID int64) (score *int64, ok bool)) []ScoreUpdate {
	weights := map[int64]int64{}
	var order []int64
	for _, s := range signals {
		if s.LeadID == 0 {
			continue
		}
		if _, seen := weights[s.LeadID]; !seen {
			order = append(order, s.LeadID)
		}
		weights[s.LeadID] += int64(s.Weight)
	}
	var out []ScoreUpdate
	for _, id := range order {
		cur, ok := scoreOf(id)
		if !ok {
			continue
		}
		var c int64
		if cur != nil {
			c = *cur
		}
		n := c + weights[id]
		if n > 100 {
			n = 100
		}
		if n == c {
			continue
		}
		out = append(out, ScoreUpdate{LeadID: id, Score: n, Tier: Tier(n)})
	}
	return out
}
