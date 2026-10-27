package firmo

// TechAdoption is one tech_adoption_signal entry of analyze_job_text.
type TechAdoption struct{ Tech, Category, Intent string }

// HiringVelocity is compute_hiring_velocity(open_roles) with no recency
// (recent_roles 0, recency_days None).
type HiringVelocity struct {
	OpenRoles int
	Level     string
}

// AnalyzeJobText is job_tech_intent.analyze_job_text(text, open_roles=open):
// the detected technologies, the adoption signals and the hiring velocity.
// The jobspy provider (internal/providers/scrape) uses it.
func AnalyzeJobText(text string, open int) (technologies []string, signals []TechAdoption, velocity HiringVelocity) {
	technologies, signals = []string{}, []TechAdoption{}
	for _, a := range detectTechAdoption(text) {
		technologies = append(technologies, a.tech)
		signals = append(signals, TechAdoption{Tech: a.tech, Category: a.category, Intent: a.intent})
	}
	v := hiringVelocity(open)
	level, _ := v[len(v)-1].v.(string)
	return technologies, signals, HiringVelocity{OpenRoles: open, Level: level}
}
