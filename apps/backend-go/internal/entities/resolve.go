package entities

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Resolution thresholds of graph.py: >= MatchThreshold is the same entity; the grey band
// [GreyBand, MatchThreshold) creates a new entity and queues a review pair.
const (
	MatchThreshold = 0.85
	GreyBand       = 0.70
)

// provFields are the lead fields carried into entity provenance (graph._PROV_FIELDS).
var provFields = []string{"company", "website", "email", "phone", "city", "state", "address",
	"contact_person", "contact_title", "description", "linkedin_url", "company_size", "specialization", "industry_tags"}

// Getter is a lead-shaped record: Get returns the value of a lead field (any JSON scalar).
type Getter interface {
	Get(key string) (any, bool)
}

// Resolved is the entity a lead resolved to.
type Resolved struct {
	ID                 string
	CorroborationCount int32
}

// Clock is the observation clock (tests override it).
var Clock = time.Now

// field is lead.get(k) (None when missing).
func field(l Getter, k string) any {
	v, _ := l.Get(k)
	return v
}

// strField is `lead.get(k, "") or ""` as a string (str() of non-strings).
func strField(l Getter, k string) string {
	v := field(l, k)
	switch x := v.(type) { // Go-typed scalars truthy() does not cover
	case int64:
		if x == 0 {
			return ""
		}
	case int:
		if x == 0 {
			return ""
		}
	case float64:
		if x == 0 {
			return ""
		}
	}
	if !truthy(v) {
		return ""
	}
	return strOf(v)
}

// isoNow is datetime.now(timezone.utc).isoformat().
func isoNow() string {
	t := Clock().UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

// blockingKeys is graph._blocking_keys (sorted for deterministic inserts).
func blockingKeys(l Getter) []string {
	keys := map[string]bool{}
	name := NormalizeCompany(strField(l, "company"))
	if r := []rune(name); len(r) >= 3 {
		keys["name:"+string(r[:3])] = true
	}
	if d := IdentityDomain(strField(l, "website")); d != "" {
		keys["domain:"+d] = true
	}
	if p := []rune(NormalizePhone(strField(l, "phone"))); len(p) >= 7 {
		keys["phone:"+string(p[len(p)-7:])] = true
	}
	city := strings.ToLower(pyStrip(strField(l, "city")))
	first := ""
	if f := strings.FieldsFunc(name, pySpace); len(f) > 0 {
		first = f[0]
	}
	if city != "" && first != "" {
		keys["city:"+city+":"+first] = true
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func observation(v any, source string) *OMap {
	o := NewOMap()
	o.Set("value", strOf(v))
	o.Set("source", source)
	o.Set("observed_at", isoNow())
	return o
}

func provValue(l Getter, f string) (any, bool) {
	v := field(l, f)
	if v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok && (s == "" || s == "N/A") {
		return nil, false
	}
	return v, true
}

// ResolveCompany is graph.resolve_company: resolve a sourced lead to its canonical
// CompanyEntity in ws (match or create) and record the observation either way. Grey-band
// near misses create a new entity and an EntityReviewPair. source is observation_source
// (empty: the lead's source, else "unknown"). tx is the caller's transaction (inside
// tenancy.InWorkspace); creation runs in a savepoint so a lost domain race lands the
// observation on the winner.
func ResolveCompany(ctx context.Context, tx pgx.Tx, ws string, lead Getter, source string) (Resolved, bool, error) {
	if source == "" {
		source = strField(lead, "source")
	}
	if source == "" {
		source = "unknown"
	}
	q := gen.New(tx)
	domain := IdentityDomain(strField(lead, "website"))

	if domain != "" {
		owner, err := identifierOwner(ctx, q, ws, domain)
		if err != nil {
			return Resolved{}, false, err
		}
		if owner != "" {
			if r, ok, err := observeIfPresent(ctx, q, ws, owner, lead, source); err != nil || ok {
				return r, false, err
			}
		}
	}

	keys := blockingKeys(lead)
	probe := Lead{Company: strField(lead, "company"), Website: domain, Phone: strField(lead, "phone"),
		Email: strField(lead, "email"), City: strField(lead, "city")}
	var best *state
	bestScore := 0.0
	if len(keys) > 0 {
		ids, err := q.BlockingCandidateIDs(ctx, gen.BlockingCandidateIDsParams{WorkspaceID: ws, Keys: keys})
		if err != nil {
			return Resolved{}, false, fmt.Errorf("entities: candidates: %w", err)
		}
		for _, id := range ids {
			row, err := q.GetCompanyEntity(ctx, gen.GetCompanyEntityParams{ID: id, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) {
				continue // tenant isolation
			}
			if err != nil {
				return Resolved{}, false, err
			}
			ent := fromRow(row)
			if s := CompareLeads(probe, ent.lead()); s > bestScore {
				best, bestScore = ent, s
			}
		}
	}

	if best != nil && bestScore >= MatchThreshold {
		if domain != "" {
			if _, err := claimIdentifier(ctx, q, ws, domain, best.ID); err != nil {
				return Resolved{}, false, err
			}
		}
		r, err := recordObservation(ctx, q, ws, best.ID, lead, source)
		return r, false, err
	}

	created, err := createEntity(ctx, tx, ws, lead, source, domain)
	if err != nil {
		return Resolved{}, false, err
	}
	if created == nil { // lost the domain race: the winner takes the observation
		owner, err := identifierOwner(ctx, q, ws, domain)
		if err != nil {
			return Resolved{}, false, err
		}
		if owner == "" {
			return Resolved{}, false, errors.New("entities: domain identifier vanished after a conflict")
		}
		r, err := recordObservation(ctx, q, ws, owner, lead, source)
		return r, false, err
	}
	if best != nil && bestScore >= GreyBand && bestScore < MatchThreshold {
		cand := NewOMap()
		cand.Set("new_entity_id", created.ID)
		cand.Set("id", created.ID)
		l := created.lead()
		cand.Set("company", l.Company)
		cand.Set("website", l.Website)
		cand.Set("phone", l.Phone)
		cand.Set("email", l.Email)
		cand.Set("city", l.City)
		if err := q.InsertEntityReviewPair(ctx, gen.InsertEntityReviewPairParams{WorkspaceID: ws, EntityID: best.ID,
			Candidate: MarshalJSON(cand), Score: math.Round(bestScore*1e4) / 1e4, Reason: "grey_band"}); err != nil {
			return Resolved{}, false, fmt.Errorf("entities: review pair: %w", err)
		}
	}
	return Resolved{ID: created.ID, CorroborationCount: *created.Corroboration}, true, nil
}

func identifierOwner(ctx context.Context, q *gen.Queries, ws, domain string) (string, error) {
	owner, err := q.CompanyIdentifierOwner(ctx, gen.CompanyIdentifierOwnerParams{WorkspaceID: ws, Kind: "domain", Value: domain})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("entities: identifier owner: %w", err)
	}
	return owner, nil
}

// claimIdentifier is graph._claim_identifier: the actual owner's id.
func claimIdentifier(ctx context.Context, q *gen.Queries, ws, domain, entityID string) (string, error) {
	owner, err := identifierOwner(ctx, q, ws, domain)
	if err != nil || owner != "" {
		return owner, err
	}
	if _, err := q.InsertCompanyIdentifier(ctx, gen.InsertCompanyIdentifierParams{WorkspaceID: ws, Kind: "domain",
		Value: domain, EntityID: entityID}); err != nil {
		return "", fmt.Errorf("entities: claim identifier: %w", err)
	}
	owner, err = identifierOwner(ctx, q, ws, domain)
	if owner == "" && err == nil {
		owner = entityID
	}
	return owner, err
}

// observeIfPresent records the observation on owner when it exists in ws.
func observeIfPresent(ctx context.Context, q *gen.Queries, ws, owner string, lead Getter, source string) (Resolved, bool, error) {
	if _, err := q.GetCompanyEntity(ctx, gen.GetCompanyEntityParams{ID: owner, WorkspaceID: ws}); errors.Is(err, pgx.ErrNoRows) {
		return Resolved{}, false, nil
	} else if err != nil {
		return Resolved{}, false, err
	}
	r, err := recordObservation(ctx, q, ws, owner, lead, source)
	return r, err == nil, err
}

// recordObservation is graph._record_observation under the entity's row lock.
func recordObservation(ctx context.Context, q *gen.Queries, ws, id string, lead Getter, source string) (Resolved, error) {
	row, err := q.LockCompanyEntity(ctx, gen.LockCompanyEntityParams{ID: id, WorkspaceID: ws})
	if err != nil {
		return Resolved{}, fmt.Errorf("entities: lock %s: %w", id, err)
	}
	s := fromRow(row)
	fields := s.Fields.Clone()
	for _, f := range provFields {
		v, ok := provValue(lead, f)
		if !ok {
			continue
		}
		cur, _ := asList(getOr(fields, f, []any{}))
		fields.Set(f, append(append([]any{}, cur...), observation(v, source)))
	}
	s.Fields = fields

	ik, ok := asObject(s.IdentityKeys)
	if ok {
		ik = ik.Clone()
	} else {
		ik = NewOMap()
	}
	add := func(key, val string) {
		if val == "" {
			return
		}
		lst, _ := asList(getOr(ik, key, []any{}))
		for _, e := range lst {
			if e == val {
				return
			}
		}
		ik.Set(key, append(append([]any{}, lst...), val))
	}
	add("domains", IdentityDomain(strField(lead, "website")))
	add("phones", NormalizePhone(strField(lead, "phone")))
	add("emails", strField(lead, "email"))
	add("name_variants", strField(lead, "company"))
	s.IdentityKeys = ik

	present := false
	for _, x := range s.Sources {
		if x == source {
			present = true
			break
		}
	}
	if source != "" && !present {
		s.Sources = append(append([]any{}, s.Sources...), source)
	}
	obs := int32(1)
	if s.Observation != nil {
		obs = *s.Observation + 1
	}
	s.Observation = &obs
	if err := ensureBlockingKeys(ctx, q, s.ID, blockingKeys(lead), s.WorkspaceID); err != nil {
		return Resolved{}, err
	}
	s.recompute()
	if err := s.save(ctx, q); err != nil {
		return Resolved{}, err
	}
	return Resolved{ID: s.ID, CorroborationCount: *s.Corroboration}, nil
}

// createEntity is graph._create_entity (+ the domain claim in the same savepoint). A nil
// state means another resolution owns the domain: the savepoint was rolled back.
func createEntity(ctx context.Context, tx pgx.Tx, ws string, lead Getter, source, domain string) (*state, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	q := gen.New(sp)
	fields := NewOMap()
	for _, f := range provFields {
		if v, ok := provValue(lead, f); ok {
			fields.Set(f, []any{observation(v, source)})
		}
	}
	company := strField(lead, "company")
	name := pyStrip(company)
	if name == "" {
		name = "(unknown)"
	}
	ik := NewOMap()
	list := func(cond bool, v string) []any {
		if cond {
			return []any{v}
		}
		return []any{}
	}
	ik.Set("domains", list(domain != "", domain))
	ik.Set("phones", list(strField(lead, "phone") != "", NormalizePhone(strField(lead, "phone"))))
	ik.Set("emails", list(strField(lead, "email") != "", strField(lead, "email")))
	ik.Set("name_variants", list(company != "", company))
	srcs := []any{}
	corr := int32(0)
	if source != "" {
		srcs, corr = []any{source}, 1
	}
	obs := int32(1)
	phone, email, city := strField(lead, "phone"), strField(lead, "email"), strField(lead, "city")
	s := &state{ID: uuid.NewString(), WorkspaceID: ws, Name: name, Domain: &domain, Phone: &phone, Email: &email, City: &city,
		IdentityKeys: ik, Fields: fields, Sources: srcs, Agreement: NewOMap(), Corroboration: &corr, Observation: &obs}
	if err := q.InsertCompanyEntity(ctx, gen.InsertCompanyEntityParams{ID: s.ID, WorkspaceID: ws, CanonicalName: s.Name,
		PrimaryDomain: pgText(s.Domain), PrimaryPhone: pgText(s.Phone), PrimaryEmail: pgText(s.Email), PrimaryCity: pgText(s.City),
		IdentityKeys: MarshalJSON(ik), Fields: MarshalJSON(fields), Sources: MarshalJSON(srcs), SourceAgreement: []byte("{}"),
		CorroborationCount: pgI32(&corr), ObservationCount: pgI32(&obs)}); err != nil {
		return nil, fmt.Errorf("entities: create: %w", err)
	}
	if err := ensureBlockingKeys(ctx, q, s.ID, blockingKeys(lead), ws); err != nil {
		return nil, err
	}
	if domain != "" {
		n, err := q.InsertCompanyIdentifier(ctx, gen.InsertCompanyIdentifierParams{WorkspaceID: ws, Kind: "domain",
			Value: domain, EntityID: s.ID})
		if err != nil {
			return nil, fmt.Errorf("entities: claim new domain: %w", err)
		}
		if n == 0 {
			return nil, nil // deferred rollback drops the entity
		}
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
