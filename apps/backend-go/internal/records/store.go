// Package records owns accepted Company/Person facts, import identities, and employment.
// Existing SQL writers enter the same operations through compatibility triggers.
package records

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/idna"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Kind string

const (
	Company Kind = "company"
	Person  Kind = "person"
)

func (k Kind) Valid() bool { return k == Company || k == Person }

var ErrNotFound = errors.New("record not found")
var ErrConflict = errors.New("record identity or version conflict")

type Record struct {
	ID           string                `json:"id"`
	Type         Kind                  `json:"record_type"`
	CompanyID    *string               `json:"company_id"`
	PersonID     *string               `json:"person_id"`
	EmploymentID *int32                `json:"employment_id"`
	Version      int64                 `json:"version"`
	UpdatedAt    string                `json:"updated_at,omitempty"`
	Fields       map[string]any        `json:"fields"`
	FieldOwners  map[string]FieldOwner `json:"field_owners,omitempty"`
	Company      map[string]any        `json:"company,omitempty"`
	Employments  []Employment          `json:"employments,omitempty"`
	ExternalIDs  []ExternalID          `json:"external_ids,omitempty"`
}
type FieldOwner struct {
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
}

type Employment struct {
	ID            int32          `json:"id"`
	CompanyID     *string        `json:"company_id"`
	CompanyName   string         `json:"company_name"`
	Fields        map[string]any `json:"fields"`
	Current       bool           `json:"current"`
	FirstObserved string         `json:"first_observed_at"`
	LastObserved  string         `json:"last_observed_at"`
}
type ExternalID struct {
	Source string `json:"source"`
	ID     string `json:"external_id"`
}
type List struct {
	Records []Record `json:"records"`
	Total   int64    `json:"total"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
}
type Write struct {
	EndEmployment     bool           `json:"end_employment,omitempty"`
	Company           map[string]any `json:"company,omitempty"`
	Person            map[string]any `json:"person,omitempty"`
	Employment        map[string]any `json:"employment,omitempty"`
	CompanyID         string         `json:"company_id,omitempty"`
	PersonID          string         `json:"person_id,omitempty"`
	CompanySource     string         `json:"company_source,omitempty"`
	CompanyExternalID string         `json:"company_external_id,omitempty"`
	ExpectedVersion   *int64         `json:"expected_version,omitempty"`
}
type Resolved struct {
	CompanyID    *string        `json:"company_id"`
	PersonID     *string        `json:"person_id"`
	EmploymentID *int32         `json:"employment_id"`
	Fields       map[string]any `json:"fields"`
}

func (r Resolved) ID(k Kind) string {
	if k == Person && r.PersonID != nil {
		return *r.PersonID
	}
	if r.CompanyID != nil {
		return *r.CompanyID
	}
	return ""
}

type Store struct {
	Pool *pgxpool.Pool
	Hook automationhook.Hook
}

func normalize(w *Write) error {
	for _, entry := range []struct {
		fields map[string]any
		keys   []string
	}{{w.Company, []string{"name", "domain", "website", "email", "phone", "linkedin_url"}}, {w.Person, []string{"full_name", "first_name", "last_name", "email", "phone", "linkedin_url"}}} {
		for _, key := range entry.keys {
			if v, ok := entry.fields[key]; ok && v != nil {
				if _, ok := v.(string); !ok {
					return fmt.Errorf("%s must be text", key)
				}
			}
		}
	}
	if v, ok := w.Company["employee_count_exact"]; ok && v != nil && v != "" {
		n, e := strconv.ParseFloat(fmt.Sprint(v), 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1000000000 || math.Trunc(n) != n {
			return errors.New("employee_count_exact must be a nonnegative integer; map employee ranges to company_size")
		}
		w.Company["employee_count_exact"] = int64(n)
	}
	if w.CompanySource != "" && w.CompanySource != "manual" && !strings.HasPrefix(w.CompanySource, "import:") && !strings.HasPrefix(w.CompanySource, "provider:") {
		w.CompanySource = "import:" + w.CompanySource
	}

	if w.Company != nil {
		for _, key := range []string{"domain", "website"} {
			v, ok := w.Company[key].(string)
			if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			raw := strings.TrimSpace(v)
			if !strings.Contains(raw, "://") {
				raw = "https://" + raw
			}
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("invalid company %s", key)
			}
			host, e := idna.Lookup.ToASCII(strings.ToLower(u.Hostname()))
			if e != nil {
				return fmt.Errorf("invalid company %s", key)
			}
			if u.Port() != "" {
				u.Host = host + ":" + u.Port()
			} else {
				u.Host = host
			}
			if key == "domain" {
				w.Company[key] = strings.TrimPrefix(host, "www.")
			} else {
				w.Company[key] = u.String()
				if existing, ok := w.Company["domain"]; !ok || existing == nil || existing == "" {
					w.Company["domain"] = strings.TrimPrefix(host, "www.")
				}
			}
		}
	}
	if value, ok := w.Company["linkedin_url"].(string); ok && strings.TrimSpace(value) != "" {
		raw := strings.TrimSpace(value)
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, e := url.Parse(raw)
		if e != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || !(strings.EqualFold(u.Hostname(), "linkedin.com") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".linkedin.com")) {
			return errors.New("company LinkedIn URL must identify a company page")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 || (parts[0] != "company" && parts[0] != "school" && parts[0] != "showcase") || parts[1] == "" {
			return errors.New("company LinkedIn URL must identify a company page")
		}
		w.Company["linkedin_url"] = "https://www.linkedin.com/" + parts[0] + "/" + strings.ToLower(parts[1])
	}
	if w.Person != nil {
		if v, ok := w.Person["email"].(string); ok {
			w.Person["email"] = strings.ToLower(strings.TrimSpace(v))
		}
		if v, ok := w.Person["linkedin_url"].(string); ok && strings.TrimSpace(v) != "" {
			raw := strings.TrimSpace(v)
			if !strings.Contains(raw, "://") {
				raw = "https://" + raw
			}
			u, e := url.Parse(raw)
			if e != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || !(strings.ToLower(u.Hostname()) == "linkedin.com" || strings.HasSuffix(strings.ToLower(u.Hostname()), ".linkedin.com")) || !strings.HasPrefix(u.Path, "/in/") {
				return errors.New("person LinkedIn URL must identify a profile")
			}
			slug := strings.Split(strings.TrimPrefix(u.Path, "/in/"), "/")[0]
			if slug == "" {
				return errors.New("person LinkedIn URL needs a profile slug")
			}
			w.Person["linkedin_url"] = "https://www.linkedin.com/in/" + strings.ToLower(slug)
		}
	}
	return nil
}

// Apply joins the caller's transaction so import, accepted facts and links commit together.
func Apply(ctx context.Context, tx pgx.Tx, ws string, kind Kind, w Write, source, externalID string, replace bool) (Resolved, error) {
	var out Resolved
	if !kind.Valid() {
		return out, errors.New("record type must be company or person")
	}
	if err := normalize(&w); err != nil {
		return out, err
	}
	raw, e := json.Marshal(w)
	if e != nil {
		return out, e
	}
	var b []byte
	e = tx.QueryRow(ctx, `SELECT opengtm_record_upsert($1,$2,$3::jsonb,$4,$5,$6)`, ws, string(kind), raw, source, externalID, replace).Scan(&b)
	if e != nil {
		return out, e
	}
	e = decodeJSON(b, &out)
	return out, e
}
func (s Store) Upsert(ctx context.Context, ws string, k Kind, w Write, source, externalID string) (Record, error) {
	var out Record
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		r, e := Apply(ctx, tx, ws, k, w, source, externalID, false)
		if e != nil {
			return e
		}
		out, e = get(ctx, tx, ws, k, r.ID(k), true)
		return e
	})
	return out, err
}
func (s Store) Patch(ctx context.Context, ws string, k Kind, id string, version int64, w Write) (Record, error) {
	var out Record
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		table := "company_entities"
		if k == Person {
			table = "person_entities"
		}
		if !k.Valid() {
			return errors.New("invalid record type")
		}
		var current int64
		if e := tx.QueryRow(ctx, `SELECT record_version FROM `+table+` WHERE id=$1 AND workspace_id=$2`, id, ws).Scan(&current); errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		} else if e != nil {
			return e
		}
		if version != current {
			return ErrConflict
		}
		w.ExpectedVersion = &version
		if k == Person {
			w.PersonID = id
		} else {
			w.CompanyID = id
		}
		r, e := Apply(ctx, tx, ws, k, w, "manual", "", true)
		if e != nil {
			return e
		}
		out, e = get(ctx, tx, ws, k, r.ID(k), true)
		return e
	})
	return out, err
}
func (s Store) Get(ctx context.Context, ws string, k Kind, id string) (Record, error) {
	var out Record
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error { var e error; out, e = get(ctx, tx, ws, k, id, true); return e })
	return out, err
}
func get(ctx context.Context, tx pgx.Tx, ws string, k Kind, id string, details bool) (Record, error) {
	r := Record{ID: id, Type: k, Fields: map[string]any{}}
	var raw []byte
	var company pgtype.Text
	table := "company_entities"
	column := "NULL::varchar"
	if k == Person {
		table = "person_entities"
		column = "company_entity_id"
	}
	if !k.Valid() {
		return r, errors.New("invalid record type")
	}
	e := tx.QueryRow(ctx, `SELECT accepted_fields,record_version,`+column+` FROM `+table+` WHERE id=$1 AND workspace_id=$2`, id, ws).Scan(&raw, &r.Version, &company)
	if errors.Is(e, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if e != nil {
		return r, e
	}
	if e = decodeJSON(raw, &r.Fields); e != nil {
		return r, e
	}
	if k == Company {
		r.CompanyID = &r.ID
	} else {
		r.PersonID = &r.ID
		if company.Valid {
			r.CompanyID = &company.String
		}
	}
	if !details {
		return r, nil
	}
	rows, e := tx.Query(ctx, `SELECT source,external_id FROM record_external_identities WHERE workspace_id=$1 AND record_type=$2 AND record_id=$3 ORDER BY source,external_id`, ws, string(k), id)
	if e != nil {
		return r, e
	}
	r.ExternalIDs = []ExternalID{}
	for rows.Next() {
		var x ExternalID
		if e = rows.Scan(&x.Source, &x.ID); e != nil {
			rows.Close()
			return r, e
		}
		r.ExternalIDs = append(r.ExternalIDs, x)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return r, e
	}
	var owners []byte
	if e = tx.QueryRow(ctx, `SELECT coalesce(jsonb_object_agg(field_name,jsonb_build_object('source',source,'updated_at',updated_at)),'{}') FROM record_field_owners WHERE workspace_id=$1 AND record_type=$2 AND record_id=$3`, ws, string(k), id).Scan(&owners); e != nil {
		return r, e
	}
	if e = decodeJSON(owners, &r.FieldOwners); e != nil {
		return r, e
	}
	if k == Person {
		if company.Valid {
			var c []byte
			e = tx.QueryRow(ctx, `SELECT accepted_fields FROM company_entities WHERE id=$1 AND workspace_id=$2`, company.String, ws).Scan(&c)
			if e != nil {
				return r, e
			}
			if e = decodeJSON(c, &r.Company); e != nil {
				return r, e
			}
		}
		erows, e := tx.Query(ctx, `SELECT e.id,e.company_entity_id,coalesce(c.accepted_fields->>'name',e.company_name,''),e.accepted_fields,e.is_current,e.first_observed_at::text,e.last_observed_at::text FROM person_employments e LEFT JOIN company_entities c ON c.id=e.company_entity_id AND c.workspace_id=e.workspace_id WHERE e.person_id=$1 AND e.workspace_id=$2 ORDER BY e.is_current DESC,e.last_observed_at DESC,e.id DESC`, id, ws)
		if e != nil {
			return r, e
		}
		r.Employments = []Employment{}
		for erows.Next() {
			var x Employment
			var cid pgtype.Text
			var f []byte
			var current int32
			if e = erows.Scan(&x.ID, &cid, &x.CompanyName, &f, &current, &x.FirstObserved, &x.LastObserved); e != nil {
				erows.Close()
				return r, e
			}
			if cid.Valid {
				x.CompanyID = &cid.String
			}
			x.Current = current != 0
			_ = decodeJSON(f, &x.Fields)
			if x.Current {
				r.EmploymentID = &x.ID
			}
			r.Employments = append(r.Employments, x)
		}
		e = erows.Err()
		erows.Close()
		if e != nil {
			return r, e
		}
	}
	return r, nil
}

type ListOptions struct {
	Search, Filter, Sort, Direction string
	Limit, Offset                   int
}

func (s Store) List(ctx context.Context, ws string, k Kind, search string, limit, offset int) (List, error) {
	return s.ListFiltered(ctx, ws, k, ListOptions{Search: search, Limit: limit, Offset: offset})
}
func (s Store) ListFiltered(ctx context.Context, ws string, k Kind, options ListOptions) (List, error) {
	search, limit, offset := options.Search, options.Limit, options.Offset
	out := List{Records: []Record{}, Limit: limit, Offset: offset}
	if !k.Valid() {
		return out, errors.New("invalid record type")
	}
	if limit < 1 || limit > 500 || offset < 0 {
		return out, errors.New("invalid pagination")
	}
	table := "company_entities"
	name := "canonical_name"
	if k == Person {
		table = "person_entities"
		name = "full_name"
	}
	filter := ""
	switch options.Filter {
	case "", "all":
	case "needs_email":
		if k != Person {
			return out, errors.New("email filter requires people")
		}
		filter = " AND lower(trim(coalesce(accepted_fields->>'email',''))) IN ('','n/a')"
	case "needs_domain":
		if k != Company {
			return out, errors.New("domain filter requires companies")
		}
		filter = " AND nullif(trim(coalesce(accepted_fields->>'domain',primary_domain,'')),'') IS NULL"
	case "qualified":
		filter = " AND accepted_fields->>'score_tier' IN ('hot','warm')"
	default:
		return out, errors.New("unsupported record filter")
	}
	order := "lower(" + name + ")"
	switch options.Sort {
	case "", "name":
	case "updated":
		order = "last_seen"
	case "score":
		order = "CASE WHEN (accepted_fields->>'score') ~ '^[0-9]{1,3}$' THEN (accepted_fields->>'score')::int END"
	default:
		return out, errors.New("unsupported record sort")
	}
	direction := " ASC"
	switch options.Direction {
	case "", "asc":
	case "desc":
		direction = " DESC"
	default:
		return out, errors.New("invalid sort direction")
	}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		where := `workspace_id=$1 AND ($2='' OR ` + name + ` ILIKE '%'||$2||'%' OR accepted_fields::text ILIKE '%'||$2||'%')` + filter
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+where, ws, search).Scan(&out.Total); e != nil {
			return e
		}
		companyExpr := "NULL::varchar, NULL::jsonb, '[]'::jsonb"
		if k == Person {
			companyExpr = "company_entity_id, (SELECT c.accepted_fields FROM company_entities c WHERE c.workspace_id=$1 AND c.id=" + table + ".company_entity_id), (SELECT coalesce(jsonb_agg(jsonb_build_object('id',e.id,'company_id',e.company_entity_id,'company_name',e.company_name,'fields',e.accepted_fields,'current',true,'first_observed_at',e.first_observed_at::text,'last_observed_at',e.last_observed_at::text)),'[]') FROM person_employments e WHERE e.workspace_id=$1 AND e.person_id=" + table + ".id AND e.is_current<>0)"
		}
		rows, e := tx.Query(ctx, `SELECT id,accepted_fields,record_version,`+companyExpr+`,last_seen::text FROM `+table+` WHERE `+where+` ORDER BY `+order+direction+` NULLS LAST,id LIMIT $3 OFFSET $4`, ws, search, limit, offset)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			r := Record{Type: k}
			var fields, companyFields, employmentFields []byte
			var cid, updated pgtype.Text
			if e = rows.Scan(&r.ID, &fields, &r.Version, &cid, &companyFields, &employmentFields, &updated); e != nil {
				return e
			}
			if e = decodeJSON(fields, &r.Fields); e != nil {
				return e
			}
			if k == Company {
				r.CompanyID = &r.ID
			} else {
				r.PersonID = &r.ID
				if cid.Valid {
					r.CompanyID = &cid.String
				}
				if len(companyFields) > 0 {
					if e = decodeJSON(companyFields, &r.Company); e != nil {
						return e
					}
				}
			}
			if len(employmentFields) > 0 {
				if e = decodeJSON(employmentFields, &r.Employments); e != nil {
					return e
				}
				if len(r.Employments) > 0 {
					r.EmploymentID = &r.Employments[0].ID
				}
			}
			r.UpdatedAt = updated.String
			out.Records = append(out.Records, r)
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return nil
	})
	return out, err
}

func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}
