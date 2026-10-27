// Package collect is the Go collection pipeline (plan 15 WP-COLLECT): the legacy
// leadgen JobRunner (apps/api/services/leadgen/job_runner.py) with its seven discovery
// strategies, AI stages (ai_stages.py), lead validator (lead_validator.py), in-batch dedup
// (pipeline.deduplicate_leads), source registry (source_registry.py), source-reliability
// ledger (source_stats.py), the enrichment stages it runs and the bulk_enrich job. Page and
// search work goes through internal/leadweb and internal/browserclient; LLM calls through
// internal/llm; ledger rows through collection_jobs / collection_job_stages.
package collect

import (
	"encoding/json"
	"time"
)

// Lead is the legacy leadgen Lead dataclass (models.py). Field order and JSON names are the
// dataclass's; Python's empty-string / zero defaults are the zero values except ScoreTier
// ("unqualified") and Status ("new"), which NewLead sets.
type Lead struct {
	ID                  int64  `json:"id,omitempty"`
	Company             string `json:"company"`
	Website             string `json:"website"`
	Email               string `json:"email"`
	EmailConfidence     string `json:"email_confidence"`
	EmailProvider       string `json:"email_provider"`
	Phone               string `json:"phone"`
	PhoneProvider       string `json:"phone_provider"`
	ContactPerson       string `json:"contact_person"`
	ContactTitle        string `json:"contact_title"`
	City                string `json:"city"`
	State               string `json:"state"`
	Address             string `json:"address"`
	Specialization      string `json:"specialization"`
	CompanySize         string `json:"company_size"`
	EmployeeCountExact  int    `json:"employee_count_exact"`
	Description         string `json:"description"`
	RevenueRange        string `json:"revenue_range"`
	FoundedYear         string `json:"founded_year"`
	IndustryTags        string `json:"industry_tags"`
	Technologies        string `json:"technologies"`
	Technographics      string `json:"technographics"`
	FundingStage        string `json:"funding_stage"`
	CompanySizeBasis    string `json:"company_size_basis"`
	LinkedinURL         string `json:"linkedin_url"`
	TwitterURL          string `json:"twitter_url"`
	FacebookURL         string `json:"facebook_url"`
	SecondaryEmails     string `json:"secondary_emails"`
	SecondaryPhones     string `json:"secondary_phones"`
	DecisionMakers      string `json:"decision_makers"`
	GlassdoorRating     string `json:"glassdoor_rating"`
	HiringSignals       string `json:"hiring_signals"`
	EnrichmentAttempts  int    `json:"enrichment_attempts"`
	EnrichmentWaterfall string `json:"enrichment_waterfall"`
	FieldProvenance     string `json:"field_provenance"`
	Source              string `json:"source"`
	SourceURL           string `json:"source_url"`
	CollectionJobID     string `json:"collection_job_id"`
	WorkspaceID         string `json:"workspace_id"`
	Score               int    `json:"score"`
	ScoreTier           string `json:"score_tier"`
	Status              string `json:"status"`
	YupchaValueProp     string `json:"yupcha_value_prop"`
	CompanyNeed         string `json:"company_need"`
	Notes               string `json:"notes"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
	LastEnrichedAt      string `json:"last_enriched_at"`
}

// now is the clock of Lead construction (a test seam).
var now = time.Now

// NewLead returns l with the dataclass defaults filled in (score_tier, status) and the
// __post_init__ timestamps (now, isoformat with +00:00) when unset.
func NewLead(l Lead) *Lead {
	if l.ScoreTier == "" {
		l.ScoreTier = "unqualified"
	}
	if l.Status == "" {
		l.Status = "new"
	}
	ts := isoformat(now().UTC())
	if l.CreatedAt == "" {
		l.CreatedAt = ts
	}
	if l.UpdatedAt == "" {
		l.UpdatedAt = ts
	}
	return &l
}

func notNA(s string) bool { return s != "" && s != "N/A" && s != "nan" }

// HasWebsite .. HasContactPerson are the Lead properties.
func (l *Lead) HasWebsite() bool       { return notNA(l.Website) }
func (l *Lead) HasEmail() bool         { return notNA(l.Email) && containsByte(l.Email, '@') }
func (l *Lead) HasPhone() bool         { return notNA(l.Phone) }
func (l *Lead) HasLinkedin() bool      { return notNA(l.LinkedinURL) }
func (l *Lead) HasContactPerson() bool { return notNA(l.ContactPerson) }

func containsByte(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

// Map is Lead.to_dict() without id (the shape emailverify and the provider RPC take).
func (l *Lead) Map() map[string]any {
	raw, _ := json.Marshal(l)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "id")
	for k, v := range m { // JSON numbers come back as float64; the dataclass ints stay ints
		if f, ok := v.(float64); ok {
			m[k] = int(f)
		}
	}
	return m
}

// SetFromMap copies the string / int fields of m (a Map() the caller mutated) back into l.
func (l *Lead) SetFromMap(m map[string]any) {
	clean := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string, int, int32, int64, float64:
			clean[k] = x
		}
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, l)
}

// Get makes *Lead a leadscore.Lead (the dataclass attributes by name).
func (l *Lead) Get(key string) (any, bool) {
	v, ok := l.Map()[key]
	return v, ok
}

// isoformat is datetime.isoformat() of an aware UTC datetime.
func isoformat(t time.Time) string {
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}
