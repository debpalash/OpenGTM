package entities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// LinkedInKey is people.linkedin_key: a LinkedIn profile URL normalized to
// "linkedin.com/in/<slug>" (lower case), else "".
func LinkedInKey(raw string) string {
	raw = pyStrip(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "linkedin.com" && !strings.HasSuffix(host, ".linkedin.com") {
		return ""
	}
	path, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		path = u.Path
	}
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 || strings.ToLower(parts[0]) != "in" {
		return ""
	}
	return "linkedin.com/in/" + strings.ToLower(parts[1])
}

func emailKey(email string) string {
	v := strings.ToLower(pyStrip(email))
	if i := strings.LastIndex(v, "@"); i >= 0 && strings.Contains(v[i+1:], ".") {
		return v
	}
	return ""
}

func companyKey(domain, name string) string {
	if domain != "" {
		return "domain:" + domain
	}
	return "name:" + strings.Join(strings.FieldsFunc(strings.ToLower(name), pySpace), " ")
}

// PersonObservation is the input of resolve_person.
type PersonObservation struct {
	Name, Company, Source           string
	Title, CompanyDomain            string
	LinkedInURL, Email, EvidenceURL string
	LegacyIDs                       []string
	ObservedAt                      time.Time // zero: now (naive UTC)
}

// ErrPersonConflict is resolve_person's ValueError for an email owner with an incompatible
// LinkedIn profile and name.
var ErrPersonConflict = errors.New("Person identity conflict: email owner has an incompatible LinkedIn profile/name")

// ErrPersonNoKey is resolve_person's ValueError for an observation without identity.
var ErrPersonNoKey = errors.New("A person needs a LinkedIn profile, email or legacy id to be resolved")

type idKey struct{ kind, value string }

func personOwner(ctx context.Context, q *gen.Queries, ws string, k idKey) (string, error) {
	id, err := q.PersonIdentifierOwner(ctx, gen.PersonIdentifierOwnerParams{WorkspaceID: ws, Kind: k.kind, Value: k.value})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ResolvePerson is people.resolve_person: resolve (or create) the canonical person of ws
// and record this employment observation. It returns the person id and whether it was
// created. tx is the caller's transaction (inside tenancy.InWorkspace).
func ResolvePerson(ctx context.Context, tx pgx.Tx, ws string, o PersonObservation) (string, bool, error) {
	if ws == "" {
		return "", false, errors.New("workspace_id is required")
	}
	observed := o.ObservedAt
	if observed.IsZero() {
		observed = Clock().UTC()
	}
	observed = observed.UTC().Truncate(time.Microsecond)
	var keys []idKey
	li := LinkedInKey(o.LinkedInURL)
	if li != "" {
		keys = append(keys, idKey{"linkedin", li})
	}
	seen := map[string]bool{}
	for _, v := range o.LegacyIDs {
		if !seen[v] && pyStrip(v) != "" {
			keys = append(keys, idKey{"legacy_id", v})
		}
		seen[v] = true
	}
	em := emailKey(o.Email)
	if em != "" {
		keys = append(keys, idKey{"email", em})
	}
	if len(keys) == 0 {
		return "", false, ErrPersonNoKey
	}
	q := gen.New(tx)
	personID, matched := "", ""
	for _, k := range keys {
		owner, err := personOwner(ctx, q, ws, k)
		if err != nil {
			return "", false, err
		}
		if owner == "" {
			continue
		}
		ok, err := q.PersonEntityExists(ctx, gen.PersonEntityExistsParams{ID: owner, WorkspaceID: ws})
		if err != nil {
			return "", false, err
		}
		if ok {
			personID, matched = owner, k.kind
			break
		}
	}
	if personID != "" && matched == "email" && li != "" {
		if err := checkEmailConflict(ctx, q, ws, personID, li, o.Name); err != nil {
			return "", false, err
		}
	}
	created := false
	if personID == "" {
		id, ok, err := createPerson(ctx, tx, ws, o.Name, keys[0])
		if err != nil {
			return "", false, err
		}
		if ok {
			personID, created = id, true
		} else if personID, err = personOwner(ctx, q, ws, keys[0]); err != nil {
			return "", false, err
		} else if personID == "" {
			return "", false, errors.New("entities: person identifier vanished after a conflict")
		}
	}
	for _, k := range keys {
		owner, err := personOwner(ctx, q, ws, k)
		if err != nil {
			return "", false, err
		}
		if owner == "" {
			if _, err := q.InsertPersonIdentifier(ctx, gen.InsertPersonIdentifierParams{WorkspaceID: ws, Kind: k.kind,
				Value: k.value, PersonID: personID}); err != nil {
				return "", false, fmt.Errorf("entities: claim person identifier: %w", err)
			}
		}
	}

	company := map[string]any{}
	if o.Company != "" {
		company["name"] = o.Company
	}
	if o.CompanyDomain != "" {
		company["domain"] = o.CompanyDomain
	}
	person := map[string]any{}
	if o.Name != "" {
		person["full_name"] = o.Name
	}
	if em != "" {
		person["email"] = em
	}
	if li != "" {
		person["linkedin_url"] = "https://" + li
	}
	employment := map[string]any{}
	if o.Title != "" {
		employment["title"] = o.Title
	}
	if o.EvidenceURL != "" {
		employment["evidence_url"] = o.EvidenceURL
	}
	payload := map[string]any{"company": company, "person": person, "employment": employment, "person_id": personID, "observed_at": observed.Format(time.RFC3339Nano)}
	raw, e := json.Marshal(payload)
	if e != nil {
		return "", false, e
	}
	var result []byte
	e = tx.QueryRow(ctx, `SELECT opengtm_record_upsert($1,'person',$2::jsonb,$3,'',false)`, ws, raw, "provider:"+o.Source).Scan(&result)
	if e != nil {
		return "", false, e
	}
	// Keep the graph's source list as observations, independent of accepted facts.
	_, e = tx.Exec(ctx, `UPDATE person_entities SET fields=jsonb_set(fields::jsonb,'{sources}',(SELECT coalesce(jsonb_agg(DISTINCT x),'[]') FROM jsonb_array_elements(coalesce(fields::jsonb->'sources','[]')||to_jsonb(ARRAY[$3::text])) x),true) WHERE id=$1 AND workspace_id=$2`, personID, ws, o.Source)
	return personID, created, e
}

func checkEmailConflict(ctx context.Context, q *gen.Queries, ws, personID, li, name string) error {
	row, err := q.LockPersonEntity(ctx, gen.LockPersonEntityParams{ID: personID, WorkspaceID: ws})
	if err != nil {
		return err
	}
	profiles, err := q.PersonLinkedInValues(ctx, gen.PersonLinkedInValuesParams{WorkspaceID: ws, PersonID: personID})
	if err != nil {
		return err
	}
	norm := func(s string) string { return strings.Join(strings.FieldsFunc(strings.ToLower(s), pySpace), " ") }
	known := map[string]bool{}
	names := []any{row.FullName}
	if l, ok := asList(getOr(objOr(row.IdentityKeys), "name_variants", nil)); ok {
		names = append(names, l...)
	}
	for _, n := range names {
		if s, ok := n.(string); ok && s != "" && s != "(unknown)" {
			known[norm(s)] = true
		}
	}
	has := false
	for _, p := range profiles {
		if p == li {
			has = true
		}
	}
	if len(profiles) > 0 && !has && !known[norm(name)] {
		return ErrPersonConflict
	}
	return nil
}

// createPerson creates the person and claims its strongest identifier in a savepoint; ok is
// false when a concurrent resolution claimed it first.
func createPerson(ctx context.Context, tx pgx.Tx, ws, name string, k idKey) (string, bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	q := gen.New(sp)
	full := pyStrip(name)
	if full == "" {
		full = "(unknown)"
	}
	id := uuid.NewString()
	if err := q.InsertPersonEntity(ctx, gen.InsertPersonEntityParams{ID: id, WorkspaceID: ws, FullName: full}); err != nil {
		return "", false, fmt.Errorf("entities: create person: %w", err)
	}
	n, err := q.InsertPersonIdentifier(ctx, gen.InsertPersonIdentifierParams{WorkspaceID: ws, Kind: k.kind, Value: k.value, PersonID: id})
	if err != nil {
		return "", false, fmt.Errorf("entities: claim person identifier: %w", err)
	}
	if n == 0 {
		return "", false, nil
	}
	return id, true, sp.Commit(ctx)
}

func naiveISO(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func ts(t time.Time) pgtype.Timestamp { return pgtype.Timestamp{Time: t, Valid: true} }

// recordEmployment is people._record_employment; it returns the person's current
// company entity.
func recordEmployment(ctx context.Context, tx pgx.Tx, ws, personID string, o PersonObservation, observed time.Time) (pgtype.Text, error) {
	q := gen.New(tx)
	domain := ""
	if o.CompanyDomain != "" {
		domain = IdentityDomain(o.CompanyDomain)
	}
	var companyEntity pgtype.Text
	if domain != "" {
		owner, err := identifierOwner(ctx, q, ws, domain)
		if err != nil {
			return pgtype.Text{}, err
		}
		if owner != "" {
			companyEntity = pgtype.Text{String: owner, Valid: true}
		}
	}
	key := companyKey(domain, o.Company)
	title := pyStrip(o.Title)
	entry := NewOMap()
	entry.Set("title", title)
	entry.Set("observed_at", naiveISO(observed))
	entry.Set("source", o.Source)
	titles := []any{}
	if title != "" {
		titles = []any{entry}
	}
	n, err := q.InsertPersonEmployment(ctx, gen.InsertPersonEmploymentParams{WorkspaceID: ws, PersonID: personID,
		CompanyKey: key, CompanyEntityID: companyEntity, CompanyName: pyStrip(o.Company), CompanyDomain: domain,
		Title: title, Titles: MarshalJSON(titles), Source: o.Source, EvidenceUrl: o.EvidenceURL, ObservedAt: ts(observed)})
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("entities: employment: %w", err)
	}
	if n == 0 { // existing employment: extend it
		e, err := q.PersonEmploymentByKey(ctx, gen.PersonEmploymentByKeyParams{WorkspaceID: ws, PersonID: personID, CompanyKey: key})
		if err != nil {
			return pgtype.Text{}, fmt.Errorf("entities: load employment: %w", err)
		}
		last, first := e.LastObservedAt.Time, e.FirstObservedAt.Time
		isLatest := !observed.Before(last)
		if observed.After(last) {
			last = observed
		}
		if observed.Before(first) {
			first = observed
		}
		upd := gen.UpdatePersonEmploymentParams{CompanyEntityID: e.CompanyEntityID, Title: e.Title, Titles: e.Titles,
			EvidenceUrl: e.EvidenceUrl, FirstObservedAt: ts(first), LastObservedAt: ts(last), ID: e.ID, WorkspaceID: ws}
		if title != "" && title != e.Title.String {
			if isLatest || e.Title.String == "" {
				upd.Title = pgtype.Text{String: title, Valid: true}
			}
			prev, _ := asList(parseOr(e.Titles, nil))
			upd.Titles = MarshalJSON(append(append([]any{}, prev...), entry))
		}
		if o.EvidenceURL != "" && (isLatest || e.EvidenceUrl.String == "") {
			upd.EvidenceUrl = pgtype.Text{String: o.EvidenceURL, Valid: true}
		}
		if companyEntity.Valid && !(e.CompanyEntityID.Valid && e.CompanyEntityID.String != "") {
			upd.CompanyEntityID = companyEntity
		}
		if err := q.UpdatePersonEmployment(ctx, upd); err != nil {
			return pgtype.Text{}, fmt.Errorf("entities: update employment: %w", err)
		}
	}
	cur, err := q.MarkCurrentEmployment(ctx, gen.MarkCurrentEmploymentParams{WorkspaceID: ws, PersonID: personID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.Text{}, nil
	}
	return cur, err
}
