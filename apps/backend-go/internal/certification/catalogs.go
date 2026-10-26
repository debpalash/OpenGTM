package certification

import "sort"

// Family names the registry a subject belongs to.
type Family string

const (
	FamilyIntegration Family = "integration"
	FamilySignal      Family = "signal_source"
	FamilyAgent       Family = "agent_capability"
	FamilyAudience    Family = "audience_capability"
	FamilyGovernance  Family = "governance_capability"
	FamilyConnector   Family = "connector"
	FamilyProvider    Family = "provider"
)

// Subject is one certifiable thing. Static registry entries carry their
// definition fields (category/capabilities, signal types or features);
// connector:<name> and provider:<name> subjects only carry ID and Family.
type Subject struct {
	ID           string
	Family       Family
	Category     string   // integrations only
	Capabilities []string // integrations only
	SignalTypes  []string // signal sources only
	Features     []string // agent, audience, governance capabilities
}

// The slices below are in Python dict insertion order (catalog order).
var integrations = []Subject{
	{ID: "webhook", Category: "activation", Capabilities: []string{"outbound"}},
	{ID: "hubspot", Category: "crm", Capabilities: []string{"outbound", "inbound"}},
	{ID: "salesforce", Category: "crm", Capabilities: []string{"outbound", "inbound"}},
	{ID: "warehouse_http", Category: "warehouse", Capabilities: []string{"outbound"}},
	{ID: "meta_ads", Category: "ads", Capabilities: []string{"audience_sync"}},
	{ID: "google_ads", Category: "ads", Capabilities: []string{"audience_sync"}},
	{ID: "linkedin_ads", Category: "ads", Capabilities: []string{"audience_sync"}},
	{ID: "instantly", Category: "sequencer", Capabilities: []string{"campaign_enroll"}},
	{ID: "smartlead", Category: "sequencer", Capabilities: []string{"campaign_enroll"}},
	{ID: "google_sheets", Category: "warehouse", Capabilities: []string{"outbound"}},
	{ID: "airtable", Category: "warehouse", Capabilities: []string{"outbound"}},
	{ID: "slack", Category: "activation", Capabilities: []string{"notification"}},
}

var signalSources = []Subject{
	{ID: "jobspy", SignalTypes: []string{"hiring", "partnership_hiring"}},
	{ID: "sec_edgar", SignalTypes: []string{"funding", "leadership_change"}},
	{ID: "website_monitor", SignalTypes: []string{"website_change", "pricing_page_change"}},
	{ID: "tech_stack", SignalTypes: []string{"tech_change", "new_tech_adopted"}},
	{ID: "news_search", SignalTypes: []string{"news", "funding"}},
}

var agentCapabilities = []Subject{
	{ID: "grounded_research", Features: []string{"citations", "budget_caps", "provenance"}},
	{ID: "chained_playbooks", Features: []string{"prior_step_context", "versioned_prompts"}},
	{ID: "audience_runs", Features: []string{"bounded_profiles", "durable_results"}},
	{ID: "run_recovery", Features: []string{"cooperative_cancellation", "in_place_retry", "completed_work_preservation"}},
	{ID: "recurring_schedules", Features: []string{"single_flight", "restart_safe"}},
}

var audienceCapabilities = []Subject{
	{ID: "dynamic_materialization", Features: []string{"serialized_refresh", "exact_membership_diff", "stable_traversal"}},
	{ID: "scheduled_refresh", Features: []string{"single_flight", "restart_safe", "crash_recovery"}},
	{ID: "membership_events", Features: []string{"entry_exit", "refresh_correlation", "automation_delivery"}},
	{ID: "destination_runs", Features: []string{"bounded_delivery", "durable_ledger", "idempotent_retry"}},
}

var governanceCapabilities = []Subject{
	{ID: "oidc_sso", Features: []string{"issuer_validation", "identity_binding", "jit_provisioning", "access_enforcement"}},
	{ID: "scim_directory", Features: []string{"user_lifecycle", "group_sync", "token_revocation", "paged_directory"}},
}

func init() {
	for _, g := range []struct {
		list []Subject
		f    Family
	}{
		{integrations, FamilyIntegration}, {signalSources, FamilySignal}, {agentCapabilities, FamilyAgent},
		{audienceCapabilities, FamilyAudience}, {governanceCapabilities, FamilyGovernance},
	} {
		for i := range g.list {
			g.list[i].Family = g.f
		}
	}
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func union(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

var (
	integrationChecks = set("authentication", "external_write", "idempotency", "retry_recovery", "tenant_isolation")
	adsChecks         = set("consent_enforcement", "identifier_hashing", "add_reconciliation", "remove_reconciliation", "partial_failure_accounting")
	warehouseChecks   = set("streaming_upload", "manifest_checksum", "bounded_memory")
	integrationExtra  = map[string]map[string]bool{
		"hubspot":       set("conflict_policy"),
		"salesforce":    set("conflict_policy"),
		"instantly":     set("campaign_enrollment"),
		"smartlead":     set("campaign_enrollment"),
		"google_sheets": set("idempotent_upsert"),
		"airtable":      set("atomic_upsert"),
		"slack":         set("notification_delivery"),
	}
	signalChecks = set("external_read", "provenance", "deduplication", "failure_recovery", "tenant_isolation")
	signalExtra  = map[string]map[string]bool{
		"jobspy":          set("employment_signal_normalization"),
		"sec_edgar":       set("cik_resolution", "filing_cursor"),
		"website_monitor": set("content_change_detection"),
		"tech_stack":      set("technology_fingerprint"),
		"news_search":     set("source_attribution"),
	}
	agentChecks = set("external_execution", "grounding", "budget_enforcement", "failure_recovery", "tenant_isolation")
	agentExtra  = map[string]map[string]bool{
		"grounded_research":   set("citation_validation", "provenance"),
		"chained_playbooks":   set("prior_step_context", "prompt_versioning"),
		"audience_runs":       set("bounded_traversal", "durable_results"),
		"run_recovery":        set("cooperative_cancellation", "in_place_retry", "completed_work_preservation"),
		"recurring_schedules": set("single_flight", "restart_recovery"),
	}
	audienceChecks = set("live_materialization", "tenant_isolation", "observability", "failure_recovery")
	audienceExtra  = map[string]map[string]bool{
		"dynamic_materialization": set("serialized_refresh", "exact_membership_diff", "stable_traversal"),
		"scheduled_refresh":       set("single_flight", "restart_recovery", "due_only_bootstrap"),
		"membership_events":       set("entry_exit", "refresh_correlation", "automation_delivery"),
		"destination_runs":        set("bounded_delivery", "durable_ledger", "idempotent_retry"),
	}
	connectorChecks  = set("authentication", "external_read", "normalization", "failure_recovery", "tenant_isolation")
	providerChecks   = set("external_read", "normalization", "provenance", "quality_sample", "failure_recovery", "tenant_isolation")
	governanceChecks = map[string]map[string]bool{
		"oidc_sso":       set("external_authentication", "identity_binding", "jit_provisioning", "access_enforcement", "failure_recovery", "tenant_isolation"),
		"scim_directory": set("external_provisioning", "user_lifecycle", "group_sync", "token_revocation", "paged_directory", "tenant_isolation"),
	}
)

func find(list []Subject, id string) (Subject, bool) {
	for _, s := range list {
		if s.ID == id {
			return s, true
		}
	}
	return Subject{}, false
}

// dynamicSubject reports whether id is connector:<name> or provider:<name>
// with name matching [a-z][a-z0-9_-]{0,79} (Python re.fullmatch).
func dynamicSubject(id string) (Family, bool) {
	var prefix Family
	var rest string
	switch {
	case len(id) > 10 && id[:10] == "connector:":
		prefix, rest = FamilyConnector, id[10:]
	case len(id) > 9 && id[:9] == "provider:":
		prefix, rest = FamilyProvider, id[9:]
	default:
		return "", false
	}
	if len(rest) == 0 || len(rest) > 80 || rest[0] < 'a' || rest[0] > 'z' {
		return "", false
	}
	for i := 1; i < len(rest); i++ {
		c := rest[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "", false
		}
	}
	return prefix, true
}

// Known reports whether id is a certifiable subject (_known_subject).
func Known(id string) bool {
	_, ok := lookup(id)
	return ok
}

func lookup(id string) (Subject, bool) {
	for _, l := range [][]Subject{integrations, signalSources, agentCapabilities, audienceCapabilities, governanceCapabilities} {
		if s, ok := find(l, id); ok {
			return s, true
		}
	}
	if f, ok := dynamicSubject(id); ok {
		return Subject{ID: id, Family: f}, true
	}
	return Subject{}, false
}

func requiredSet(id string) map[string]bool {
	s, ok := lookup(id)
	if !ok {
		return nil
	}
	switch s.Family {
	case FamilyIntegration:
		req := union(integrationChecks, integrationExtra[id])
		for _, c := range s.Capabilities {
			if c == "inbound" {
				req["inbound_reconciliation"] = true
			}
		}
		if s.Category == "ads" {
			req = union(req, adsChecks)
		}
		if id == "warehouse_http" {
			req = union(req, warehouseChecks)
		}
		return req
	case FamilySignal:
		return union(signalChecks, signalExtra[id])
	case FamilyAgent:
		return union(agentChecks, agentExtra[id])
	case FamilyAudience:
		return union(audienceChecks, audienceExtra[id])
	case FamilyGovernance:
		return union(governanceChecks[id])
	case FamilyConnector:
		return union(connectorChecks)
	default:
		return union(providerChecks)
	}
}

// RequiredChecks is required_checks_for_subject: the sorted evidence contract
// for a subject. Python raises for an unknown subject; Go returns nil (use
// Known to tell the cases apart). A known subject never returns an empty list.
func RequiredChecks(subjectID string) []string {
	req := requiredSet(subjectID)
	if req == nil {
		return nil
	}
	out := make([]string, 0, len(req))
	for k := range req {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Certification is the public projection of a valid certificate. Values are
// copied verbatim from the certificate (Python copies the same fields).
type Certification struct {
	ValidatedAt        any `json:"validated_at"`
	ExpiresAt          any `json:"expires_at"`
	BuildSHA           any `json:"build_sha"`
	ValidationRunID    any `json:"validation_run_id"`
	EvidenceURL        any `json:"evidence_url"`
	EvidenceSHA256     any `json:"evidence_sha256"`
	Mode               any `json:"mode"`
	SubjectBuildSHA256 any `json:"subject_build_sha256,omitempty"` // connector subjects only
}

// Status is one certification_statuses value.
type Status struct {
	RequiredChecks []string       `json:"required_checks"`
	Maturity       string         `json:"maturity"`
	Certification  *Certification `json:"certification"`
}

// IntegrationEntry, SignalSourceEntry and CapabilityEntry keep the Python
// key order (id, definition fields, required_checks, maturity, certification).
type IntegrationEntry struct {
	ID             string         `json:"id"`
	Category       string         `json:"category"`
	Capabilities   []string       `json:"capabilities"`
	RequiredChecks []string       `json:"required_checks"`
	Maturity       string         `json:"maturity"`
	Certification  *Certification `json:"certification"`
}

type SignalSourceEntry struct {
	ID             string         `json:"id"`
	SignalTypes    []string       `json:"signal_types"`
	RequiredChecks []string       `json:"required_checks"`
	Maturity       string         `json:"maturity"`
	Certification  *Certification `json:"certification"`
}

type CapabilityEntry struct {
	ID             string         `json:"id"`
	Features       []string       `json:"features"`
	RequiredChecks []string       `json:"required_checks"`
	Maturity       string         `json:"maturity"`
	Certification  *Certification `json:"certification"`
}

func maturity(c *Certification) string {
	if c != nil {
		return "supported"
	}
	return "beta"
}

func cp(s []string) []string { return append([]string{}, s...) }

// IntegrationCatalog is integration_catalog: missing, invalid or expired
// evidence stays beta.
func IntegrationCatalog(o Options) []IntegrationEntry {
	valid := o.loadValid(nil, nil)
	out := make([]IntegrationEntry, 0, len(integrations))
	for _, s := range integrations {
		c := projection(valid[s.ID])
		out = append(out, IntegrationEntry{ID: s.ID, Category: s.Category, Capabilities: cp(s.Capabilities),
			RequiredChecks: RequiredChecks(s.ID), Maturity: maturity(c), Certification: c})
	}
	return out
}

// SignalSourceCatalog is signal_source_catalog.
func SignalSourceCatalog(o Options) []SignalSourceEntry {
	valid := o.loadValid(nil, nil)
	out := make([]SignalSourceEntry, 0, len(signalSources))
	for _, s := range signalSources {
		c := projection(valid[s.ID])
		out = append(out, SignalSourceEntry{ID: s.ID, SignalTypes: cp(s.SignalTypes),
			RequiredChecks: RequiredChecks(s.ID), Maturity: maturity(c), Certification: c})
	}
	return out
}

func capabilityCatalog(o Options, list []Subject) []CapabilityEntry {
	valid := o.loadValid(nil, nil)
	out := make([]CapabilityEntry, 0, len(list))
	for _, s := range list {
		c := projection(valid[s.ID])
		out = append(out, CapabilityEntry{ID: s.ID, Features: cp(s.Features),
			RequiredChecks: RequiredChecks(s.ID), Maturity: maturity(c), Certification: c})
	}
	return out
}

// AgentCapabilityCatalog is agent_capability_catalog.
func AgentCapabilityCatalog(o Options) []CapabilityEntry {
	return capabilityCatalog(o, agentCapabilities)
}

// AudienceCapabilityCatalog is audience_capability_catalog.
func AudienceCapabilityCatalog(o Options) []CapabilityEntry {
	return capabilityCatalog(o, audienceCapabilities)
}

// GovernanceCapabilityCatalog is governance_capability_catalog.
func GovernanceCapabilityCatalog(o Options) []CapabilityEntry {
	return capabilityCatalog(o, governanceCapabilities)
}
