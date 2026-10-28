package records

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

const MaxImportRows = 10000

// ImportRequest maps source column names to owned fields (company.*, person.*,
// employment.*) or relationship keys. An empty mapping entry explicitly ignores a
// column. Unmapped columns stay on the subject as custom fields.
type ImportRequest struct {
	Kind           Kind              `json:"record_type"`
	Source         string            `json:"source"`
	IdempotencyKey string            `json:"idempotency_key"`
	CSV            string            `json:"csv,omitempty"`
	Rows           []map[string]any  `json:"rows,omitempty"`
	Mapping        map[string]string `json:"mapping,omitempty"`
}
type ImportRow struct {
	Row       int     `json:"row"`
	Status    string  `json:"status"`
	ID        string  `json:"id,omitempty"`
	CompanyID *string `json:"company_id,omitempty"`
	Error     string  `json:"error,omitempty"`
}
type ImportResult struct {
	Preview  bool              `json:"preview"`
	Replayed bool              `json:"replayed"`
	Total    int               `json:"total"`
	Imported int               `json:"imported"`
	Rejected int               `json:"rejected"`
	Mapping  map[string]string `json:"mapping"`
	Warnings []string          `json:"warnings"`
	Rows     []ImportRow       `json:"rows"`
}

func headerKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimPrefix(s, "\ufeff"))
}
func SuggestedField(header string, k Kind) string {
	if strings.HasPrefix(header, "company.") || strings.HasPrefix(header, "person.") || strings.HasPrefix(header, "employment.") {
		return header
	}
	h := headerKey(header)
	if h == "id" {
		return "external_id"
	}
	shared := map[string]string{"company": "company.name", "companyname": "company.name", "companyemail": "company.email", "companyphone": "company.phone", "organization": "company.name", "organizationname": "company.name", "companydomain": "company.domain", "domain": "company.domain", "website": "company.website", "companywebsite": "company.website", "industry": "company.industry", "employees": "company.employee_count_exact", "employeecount": "company.employee_count_exact", "companysize": "company.company_size", "companylinkedinurl": "company.linkedin_url", "companydescription": "company.description", "companyid": "company_id", "opengtmcompanyid": "company_id", "personid": "person_id", "opengtmpersonid": "person_id", "rowid": "external_id", "clayrowid": "external_id", "externalid": "external_id", "companyexternalid": "company_external_id", "companysource": "company_source"}
	if v := shared[h]; v != "" {
		return v
	}
	if h == "opengtmid" {
		if k == Person {
			return "person_id"
		}
		return "company_id"
	}
	if k == Person {
		personal := map[string]string{"name": "person.full_name", "fullname": "person.full_name", "contactperson": "person.full_name", "firstname": "person.first_name", "lastname": "person.last_name", "email": "person.email", "workemail": "person.email", "emailaddress": "person.email", "personalemail": "person.personal_email", "phone": "person.phone", "phonenumber": "person.phone", "mobilephone": "person.mobile_phone", "linkedin": "person.linkedin_url", "linkedinurl": "person.linkedin_url", "linkedinprofile": "person.linkedin_url", "personlinkedinurl": "person.linkedin_url", "title": "employment.title", "jobtitle": "employment.title", "contacttitle": "employment.title", "seniority": "employment.seniority", "department": "employment.department"}
		if v := personal[h]; v != "" {
			return v
		}
	} else {
		if h == "name" {
			return "company.name"
		}
		if h == "email" || h == "phone" || h == "linkedinurl" || h == "description" {
			return "company." + map[string]string{"email": "email", "phone": "phone", "linkedinurl": "linkedin_url", "description": "description"}[h]
		}
	}
	for _, f := range []string{"city", "state", "country", "address", "technologies", "funding_stage", "revenue_range"} {
		if h == headerKey(f) {
			return "company." + f
		}
	}
	return string(k) + "." + strings.TrimPrefix(header, "\ufeff")
}

// FlatWrite adapts existing lead-shaped connector payloads while retaining
// explicit field ownership for new integrations.
func FlatWrite(k Kind, fields map[string]any, companyID, personID string) (Write, map[string]string, error) {
	corporate := map[string]bool{}
	for _, key := range []string{"website", "domain", "description", "city", "state", "country", "address", "industry", "industry_tags", "specialization", "company_size", "employee_count_exact", "company_size_basis", "revenue_range", "founded_year", "founding_year", "technologies", "funding_stage", "funding_amount", "funding_date", "hiring_signals"} {
		corporate[key] = true
	}
	mapping := map[string]string{}
	for key := range fields {
		target := SuggestedField(key, k)
		if corporate[key] {
			target = "company." + key
		}
		if key == "company_linkedin_url" {
			target = "company.linkedin_url"
		}
		if key == "company_email" {
			target = "company.email"
		}
		if key == "company_phone" {
			target = "company.phone"
		}
		mapping[key] = target
	}
	w, _, e := mappedWrite(fields, mapping)
	w.CompanyID = companyID
	w.PersonID = personID
	return w, mapping, e
}
func cellValue(s string) any {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var v any
		d := json.NewDecoder(strings.NewReader(t))
		d.UseNumber()
		if d.Decode(&v) == nil {
			return v
		}
	}
	return s
}
func (r *ImportRequest) prepare() ([]map[string]any, map[string]string, error) {
	if !r.Kind.Valid() {
		return nil, nil, errors.New("record_type must be company or person")
	}
	r.Source = strings.TrimSpace(r.Source)
	if r.Source == "" || len(r.Source) > 200 {
		return nil, nil, errors.New("source must name the source table (for example clay:prospects)")
	}
	if r.CSV != "" && r.Rows != nil {
		return nil, nil, errors.New("provide CSV or rows, not both")
	}
	rows := r.Rows
	headers := map[string]bool{}
	if r.CSV != "" {
		reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(r.CSV, "\ufeff")))
		reader.ReuseRecord = true
		h, err := reader.Read()
		if err != nil {
			return nil, nil, fmt.Errorf("CSV header: %w", err)
		}
		names := append([]string(nil), h...)
		if len(names) > 256 {
			return nil, nil, errors.New("CSV has more than 256 columns")
		}
		for _, name := range names {
			if strings.TrimSpace(name) == "" || headers[name] {
				return nil, nil, errors.New("CSV headers must be nonempty and unique")
			}
			headers[name] = true
		}
		for {
			values, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("CSV row %d: %w", len(rows)+2, err)
			}
			m := map[string]any{}
			for i, v := range values {
				m[names[i]] = cellValue(v)
			}
			rows = append(rows, m)
			if len(rows) > MaxImportRows {
				return nil, nil, fmt.Errorf("maximum %d rows per import", MaxImportRows)
			}
		}
	}
	if len(rows) == 0 || len(rows) > MaxImportRows {
		return nil, nil, fmt.Errorf("provide 1 to %d rows", MaxImportRows)
	}
	for _, row := range rows {
		if len(row) > 256 {
			return nil, nil, errors.New("a row has more than 256 columns")
		}
		for key := range row {
			headers[key] = true
		}
	}
	mapping := map[string]string{}
	targets := map[string]string{}
	for header := range headers {
		target, ok := r.Mapping[header]
		if !ok {
			target = SuggestedField(header, r.Kind)
		}
		if target != "" {
			if target != "external_id" && target != "company_id" && target != "person_id" && target != "company_source" && target != "company_external_id" {
				parts := strings.SplitN(target, ".", 2)
				if len(parts) != 2 || (parts[0] != "company" && parts[0] != "person" && parts[0] != "employment") || parts[1] == "" {
					return nil, nil, fmt.Errorf("invalid mapping for %q", header)
				}
				if r.Kind == Company && parts[0] != "company" {
					return nil, nil, errors.New("company imports cannot contain person or employment fields")
				}
			}
			if previous, exists := targets[target]; exists {
				return nil, nil, fmt.Errorf("%q and %q both map to %s; ignore one or give it a custom field", previous, header, target)
			}
			targets[target] = header
		}
		mapping[header] = target
	}
	return rows, mapping, nil
}
func mappedWrite(row map[string]any, mapping map[string]string) (Write, string, error) {
	w := Write{Company: map[string]any{}, Person: map[string]any{}, Employment: map[string]any{}}
	external := ""
	for header, target := range mapping {
		if target == "" {
			continue
		}
		value := row[header]
		if value == nil || value == "" {
			continue
		}
		switch target {
		case "external_id":
			switch value.(type) {
			case string, json.Number, float64, int, int64:
				external = fmt.Sprint(value)
			default:
				return w, "", errors.New("external row ID must be text or a number")
			}
			if len(external) > 255 {
				return w, "", errors.New("external row ID exceeds 255 characters")
			}
		case "company_id":
			w.CompanyID = fmt.Sprint(value)
		case "person_id":
			w.PersonID = fmt.Sprint(value)
		case "company_source":
			w.CompanySource = fmt.Sprint(value)
		case "company_external_id":
			w.CompanyExternalID = fmt.Sprint(value)
		default:
			parts := strings.SplitN(target, ".", 2)
			switch parts[0] {
			case "company":
				w.Company[parts[1]] = value
			case "person":
				w.Person[parts[1]] = value
			case "employment":
				w.Employment[parts[1]] = value
			}
		}
	}
	return w, external, normalize(&w)
}

var previewRollback = errors.New("rollback import preview")

// Import uses one transaction and per-row savepoints: a rejected row cannot
// leave a partially created company, employment or identifier behind.
func (s Store) Import(ctx context.Context, ws string, req ImportRequest, preview bool) (ImportResult, error) {
	rows, mapping, err := req.prepare()
	out := ImportResult{Preview: preview, Total: len(rows), Mapping: mapping, Rows: []ImportRow{}, Warnings: []string{"CSV contains only the values exported by Clay. Hidden object data, skipped results, provider credentials and workflow definitions are not included."}}
	if err != nil {
		return out, err
	}
	if !preview && (req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200) {
		return out, errors.New("idempotency_key is required (up to 200 characters)")
	}
	raw, _ := json.Marshal(struct {
		Kind    Kind
		Source  string
		Rows    []map[string]any
		Mapping map[string]string
	}{req.Kind, req.Source, rows, mapping})
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if !preview {
			if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+"|import|"+req.IdempotencyKey); e != nil {
				return e
			}
			var previousHash string
			var result []byte
			e := tx.QueryRow(ctx, `SELECT payload_hash,result FROM record_imports WHERE workspace_id=$1 AND idempotency_key=$2`, ws, req.IdempotencyKey).Scan(&previousHash, &result)
			if e == nil {
				if previousHash != hash {
					return ErrConflict
				}
				if e = decodeJSON(result, &out); e != nil {
					return e
				}
				out.Replayed = true
				return nil
			}
			if !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
		}
		for i, row := range rows {
			r := ImportRow{Row: i + 1, Status: "rejected"}
			w, external, e := mappedWrite(row, mapping)
			if e == nil {
				sp, beginErr := tx.Begin(ctx)
				if beginErr != nil {
					return beginErr
				}
				var ids Resolved
				ids, e = Apply(ctx, sp, ws, req.Kind, w, "import:"+req.Source, external, false)
				if e == nil {
					e = sp.Commit(ctx)
					r.ID = ids.ID(req.Kind)
					r.CompanyID = ids.CompanyID
				} else {
					_ = sp.Rollback(context.WithoutCancel(ctx))
				}
			}
			if e != nil {
				r.Error = PublicError(e)
				out.Rejected++
			} else {
				r.Status = "accepted"
				out.Imported++
			}
			out.Rows = append(out.Rows, r)
		}
		if preview {
			return previewRollback
		}
		result, e := json.Marshal(out)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO record_imports(workspace_id,idempotency_key,payload_hash,source,result) VALUES($1,$2,$3,$4,$5)`, ws, req.IdempotencyKey, hash, req.Source, result)
		return e
	})
	if errors.Is(err, previewRollback) {
		err = nil
	}
	return out, err
}

// CSV uses stable, Clay-friendly identity columns and JSON-encoded custom values.
// It streams in bounded pages rather than truncating exports at a UI page limit.
func (s Store) ExportCSV(ctx context.Context, ws string, k Kind, w io.Writer) error {
	if !k.Valid() {
		return errors.New("record type must be company or person")
	}
	tx, e := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, e = tx.Exec(ctx, `SELECT set_config('app.workspace_id',$1,true)`, ws); e != nil {
		return e
	}
	type field struct{ owner, key, label string }
	fields := []field{}
	table := "company_entities"
	owner := "company"
	if k == Person {
		table, owner = "person_entities", "person"
	}
	query := `SELECT DISTINCT $2::text AS owner,key FROM ` + table + ` r CROSS JOIN LATERAL jsonb_object_keys(r.accepted_fields) key WHERE r.workspace_id=$1`
	if k == Person {
		query += ` UNION SELECT DISTINCT 'employment',key FROM person_employments e CROSS JOIN LATERAL jsonb_object_keys(e.accepted_fields) key WHERE e.workspace_id=$1 AND e.is_current<>0`
	}
	keys, e := tx.Query(ctx, query, ws, owner)
	if e != nil {
		return e
	}
	for keys.Next() {
		var f field
		if e = keys.Scan(&f.owner, &f.key); e != nil {
			keys.Close()
			return e
		}
		fields = append(fields, f)
	}
	e = keys.Err()
	keys.Close()
	if e != nil {
		return e
	}
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].owner == fields[j].owner {
			return fields[i].key < fields[j].key
		}
		return fields[i].owner < fields[j].owner
	})
	labels := map[string]string{"company.name": "Company Name", "company.domain": "Domain", "company.website": "Website", "company.industry": "Industry", "company.city": "City", "company.country": "Country", "company.email": "Company Email", "company.phone": "Company Phone", "company.linkedin_url": "Company LinkedIn URL", "person.full_name": "Full Name", "person.first_name": "First Name", "person.last_name": "Last Name", "person.email": "Work Email", "person.phone": "Phone", "person.linkedin_url": "LinkedIn URL", "employment.title": "Job Title", "employment.department": "Department", "employment.seniority": "Seniority"}
	headers := []string{"OpenGTM ID"}
	if k == Person {
		headers = append(headers, "Company ID", "Company Name", "Company Domain")
	}
	for i := range fields {
		f := &fields[i]
		f.label = f.owner + "." + f.key
		if label, ok := labels[f.label]; ok {
			f.label = label
		}
		headers = append(headers, f.label)
	}
	var data pgx.Rows
	if k == Company {
		data, e = tx.Query(ctx, `SELECT id,id,accepted_fields,'{}'::jsonb,'{}'::jsonb FROM company_entities WHERE workspace_id=$1 ORDER BY id`, ws)
	} else {
		data, e = tx.Query(ctx, `SELECT p.id,p.company_entity_id,p.accepted_fields,coalesce(c.accepted_fields,'{}'),coalesce(e.accepted_fields,'{}') FROM person_entities p LEFT JOIN company_entities c ON c.workspace_id=p.workspace_id AND c.id=p.company_entity_id LEFT JOIN LATERAL (SELECT accepted_fields FROM person_employments WHERE workspace_id=p.workspace_id AND person_id=p.id AND is_current<>0 ORDER BY last_observed_at DESC,id DESC LIMIT 1) e ON true WHERE p.workspace_id=$1 ORDER BY p.id`, ws)
	}
	if e != nil {
		return e
	}
	defer data.Close()
	writer := csv.NewWriter(w)
	if e = writer.Write(headers); e != nil {
		return e
	}
	for data.Next() {
		var id string
		var cid *string
		var subject, company, employment []byte
		if e = data.Scan(&id, &cid, &subject, &company, &employment); e != nil {
			return e
		}
		values := map[string]map[string]any{}
		var own, corp, job map[string]any
		if e = decodeJSON(subject, &own); e != nil {
			return e
		}
		if e = decodeJSON(company, &corp); e != nil {
			return e
		}
		if e = decodeJSON(employment, &job); e != nil {
			return e
		}
		values[string(k)] = own
		values["employment"] = job
		if k == Person {
			values["company"] = corp
		}
		line := []string{id}
		if k == Person {
			companyID := ""
			if cid != nil {
				companyID = *cid
			}
			line = append(line, companyID)
			line = append(line, textValue(corp["name"]), textValue(corp["domain"]))
		}
		for _, f := range fields {
			line = append(line, textValue(values[f.owner][f.key]))
		}
		if e = writer.Write(line); e != nil {
			return e
		}
	}
	if e = data.Err(); e != nil {
		return e
	}
	data.Close()
	writer.Flush()
	if e = writer.Error(); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func textValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
