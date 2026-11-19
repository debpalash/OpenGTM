package providers

// Class says what a failure of a provider call means for the provider as a whole. It is the registry metadata the
// cooldown policy (internal/cooldown) reads, so the policy never keeps a list of provider names.
//
// A timeout is evidence about the provider only when every call goes to the same place:
//
//   - ClassUpstream: every call reaches one vendor API or one search backend (hunter_io, wikidata, ddg_company, ...).
//     A run of timeouts means that upstream is unhealthy, so the consecutive-timeout cooldown applies.
//   - ClassTargetSite: the call fetches the lead's own website or mail server (website_scraper, deep_scraper,
//     jsonld_firmographics, tech_stack, mailscout). A timeout is a property of that one lead's site and says nothing
//     about the next lead's, so it never starts a provider-wide cooldown. A rate limit still can.
//   - ClassLocal: the provider does no network I/O (company_size_heuristic, lead_scorer); it cannot time out.
type Class int

const (
	// ClassUnclassified is a provider that declared no class (declarative manifest providers and test fakes). The
	// cooldown treats it as ClassUpstream, the conservative reading for a vendor API.
	ClassUnclassified Class = iota
	ClassUpstream
	ClassTargetSite
	ClassLocal
)

func (c Class) String() string {
	switch c {
	case ClassUpstream:
		return "upstream"
	case ClassTargetSite:
		return "target_site"
	case ClassLocal:
		return "local"
	}
	return "unclassified"
}

// Classed is implemented by providers that declare their Class. Embed Upstream, TargetSite or Local.
type Classed interface{ CooldownClass() Class }

// Upstream is embedded by providers whose calls all go to one vendor API or search backend.
type Upstream struct{}

func (Upstream) CooldownClass() Class { return ClassUpstream }

// TargetSite is embedded by providers that fetch the lead's own website or mail server.
type TargetSite struct{}

func (TargetSite) CooldownClass() Class { return ClassTargetSite }

// Local is embedded by providers that do no network I/O.
type Local struct{}

func (Local) CooldownClass() Class { return ClassLocal }

// ClassOf is the declared class of p, ClassUnclassified when it declares none.
func ClassOf(p Provider) Class {
	if c, ok := p.(Classed); ok {
		return c.CooldownClass()
	}
	return ClassUnclassified
}

// ClassOfName is ClassOf for the provider registered under name (ClassUnclassified when there is none).
func ClassOfName(name string) Class {
	p, ok := Get(name)
	if !ok {
		return ClassUnclassified
	}
	return ClassOf(p)
}
