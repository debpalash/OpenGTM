package sourcewb

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	// PersonSource is the row source and source kind of people_search.
	PersonSource = "people_search"
	// PersonConfidence is the CrossLinked snippet-parse confidence.
	PersonConfidence = 0.6
)

var domainRE = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?([a-z0-9-]+(?:\.[a-z0-9-]+)+)/?$`)

// PersonIdentity is people_search.person_identity: the stable "person:<sha1[:24]>" row
// identity, keyed on the LinkedIn URL when present, else name+company.
func PersonIdentity(name, company, linkedinURL string) string {
	var basis string
	if li := strip(linkedinURL); li != "" {
		basis = "li:" + strings.TrimRight(strings.ToLower(li), "/")
	} else {
		basis = "nc:" + strings.ToLower(strip(name)) + "|" + strings.ToLower(strip(company))
	}
	sum := sha1.Sum([]byte(basis))
	return "person:" + hex.EncodeToString(sum[:])[:24]
}

func splitName(full string) (string, string) {
	parts := strings.FieldsFunc(strip(full), isSpace)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	}
	return parts[0], parts[len(parts)-1]
}

// companyAndWebsite: a companies[] entry is a name or a domain; a domain also yields
// its https website.
func companyAndWebsite(entry string) (string, string) {
	e := strip(entry)
	if m := domainRE.FindStringSubmatch(e); m != nil {
		return e, "https://" + strings.ToLower(m[1])
	}
	return e, ""
}

// PersonRowData is people_search._row_data: the row snapshot of a discovered person.
func PersonRowData(companyEntry string, p Person) *wbcrud.Obj {
	name := strip(p.Name)
	title := strip(p.Title)
	if strings.ToUpper(title) == "N/A" {
		title = ""
	}
	first, last := splitName(name)
	company, website := companyAndWebsite(companyEntry)
	data := obj("company", company, "contact_person", name, "full_name", name, "first_name", first,
		"last_name", last, "source", PersonSource, "confidence", PersonConfidence)
	if title != "" {
		data.Set("contact_title", title)
		data.Set("title", title)
	}
	if website != "" {
		data.Set("website", website)
	}
	if li := strip(p.LinkedIn); li != "" {
		data.Set("linkedin_url", li)
	}
	return data
}

// resolveDataKey maps a from_column reference (column id, name or lead field) to the
// row-data key.
func resolveDataKey(cols []wbcrud.Column, from string) string {
	for _, c := range cols {
		if c.Get("id") == from || c.Get("name") == from {
			if lf := c.Get("lead_field"); truthy(lf) {
				return pyStr(lf)
			}
			if id := c.Get("id"); truthy(id) {
				return pyStr(id)
			}
			return from
		}
	}
	return from
}

// rowValue is row_execution_data(row, columns_config).get(key): stored data overlaid with
// lead_field/input aliases and complete enrichment values (failed snapshots invalidate the
// stored copy), plus the row identity keys.
func rowValue(r gen.SWRowsDataRow, cols []wbcrud.Column, key string) any {
	data, _ := wbcrud.ParseObject(nonEmptyJSON(r.Data))
	enr, _ := wbcrud.ParseObject(nonEmptyJSON(r.Enrichments))
	input := map[string]any{}
	vals := map[string]any{}
	if data != nil {
		for _, k := range data.Keys() {
			vals[k], _ = data.Get(k)
			input[k] = vals[k]
		}
	}
	values, aliases := map[string]any{}, map[string]any{}
	for _, c := range cols {
		cid := pyStr(c.Get("id"))
		name := c.Get("name")
		if t := c.Get("type"); t == "lead_field" || t == "input" {
			f := cid
			if lf := c.Get("lead_field"); truthy(lf) {
				f = pyStr(lf)
			}
			if v, ok := input[f]; ok {
				values[cid] = v
				if truthy(name) {
					aliases[pyStr(name)] = v
				}
			}
			continue
		}
		cell, ok := asObj(get(enr, cid))
		if !ok {
			continue
		}
		v, _ := cell.Get("value")
		if get(cell, "status") != "complete" || v == nil {
			delete(vals, cid)
			if truthy(name) {
				delete(vals, pyStr(name))
			}
			continue
		}
		values[cid] = v
		if truthy(name) {
			aliases[pyStr(name)] = v
		}
	}
	for k, v := range aliases {
		vals[k] = v
	}
	for k, v := range values {
		vals[k] = v
	}
	switch key {
	case "id":
		if r.LeadID.Valid {
			return int64(r.LeadID.Int32)
		}
		return int64(r.ID)
	case "__row_id":
		return int64(r.ID)
	case "__lead_id":
		if r.LeadID.Valid {
			return int64(r.LeadID.Int32)
		}
		return nil
	}
	return vals[key]
}

func (d Deps) companiesFromRows(ctx context.Context, ws, wbID, from string) ([]string, error) {
	var out []string
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		wb, err := q.SWLoadWorkbook(ctx, gen.SWLoadWorkbookParams{ID: wbID, WorkspaceID: ws})
		var cols []wbcrud.Column
		if err == nil {
			if cols, err = wbcrud.ParseColumns(wb.ColumnsConfig); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		key := resolveDataKey(cols, from)
		rows, err := q.SWRowsData(ctx, gen.SWRowsDataParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, r := range rows {
			v := strip(orStr(rowValue(r, cols, key)))
			if v != "" && !seen[strings.ToLower(v)] {
				seen[strings.ToLower(v)] = true
				out = append(out, v)
			}
		}
		return nil
	})
	return out, err
}

type discovered struct {
	company string
	person  Person
}

// materializePeople is materialize_people_search: discover people at the target companies
// (budgeted searches) and append one row per new person. The caller set status running;
// every exit resets it to draft.
func (d Deps) materializePeople(ctx context.Context, ws, wbID, colID string, col map[string]any) (*wbcrud.Obj, error) {
	fail := func(code string) (*wbcrud.Obj, error) {
		if err := d.setStatus(ctx, ws, wbID, "draft"); err != nil {
			return nil, err
		}
		return obj("error", code, "found", 0, "added", 0), nil
	}
	if !d.PeopleSearchEnabled {
		return fail("people_search_disabled")
	}
	cfg, err := runplan.ValidatePeopleSearchConfig(col)
	if err != nil {
		if msg, ok := runplan.IsPeopleSearchError(err); ok {
			return fail(string(msg))
		}
		return nil, err
	}
	companies := cfg.Companies
	if len(companies) == 0 && cfg.FromColumn != "" {
		if companies, err = d.companiesFromRows(ctx, ws, wbID, cfg.FromColumn); err != nil {
			return nil, err
		}
	}
	if len(companies) == 0 {
		return fail("no_companies")
	}

	budget := int(cfg.MaxSearches)
	used, failures := 0, 0
	var found []discovered
	for _, company := range companies {
		if used >= budget {
			d.log().Info("people_search: search budget exhausted", "workbook_id", wbID, "budget", budget)
			break
		}
		res, err := d.People.FindPeopleByTitles(ctx, PeopleQuery{Company: company, Titles: cfg.Titles, Geo: cfg.Geo,
			Seniority: cfg.Seniority, MaxPeople: int(cfg.MaxPerCompany), MaxSearches: budget - used})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			d.log().Warn("people_search discovery failed", "workbook_id", wbID, "err_type", fmt.Sprintf("%T", err))
			used++
			failures++
			continue
		}
		used += res.SearchesUsed
		failures += res.SearchFailures
		for _, p := range res.People {
			found = append(found, discovered{company, p})
		}
	}

	out, err := d.runWrites(ctx, ws, wbID, colID, len(found), func(tx pgx.Tx, q *gen.Queries, w *writeOut) error {
		present, err := entityIDs(ctx, q, ws, wbID)
		if err != nil {
			return err
		}
		stats, err := q.SWRowStats(ctx, gen.SWRowStatsParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		pos := stats.MaxPos
		for _, f := range found {
			name := strip(f.person.Name)
			if name == "" {
				w.skipped++
				continue
			}
			identity := PersonIdentity(name, f.company, f.person.LinkedIn)
			if present[identity] {
				w.skipped++
				continue
			}
			present[identity] = true
			data := PersonRowData(f.company, f.person)
			company, _ := data.Get("company")
			website, _ := data.Get("website")
			title, _ := data.Get("contact_title")
			evidence := f.person.EvidenceURL
			if evidence == "" {
				evidence = f.person.LinkedIn
			}
			pid, _, err := entities.ResolvePerson(ctx, tx, ws, entities.PersonObservation{Name: name,
				Company: orStr(company), CompanyDomain: orStr(website), Title: orStr(title),
				LinkedInURL: f.person.LinkedIn, EvidenceURL: evidence, Source: PersonSource, LegacyIDs: []string{identity}})
			if err != nil {
				return fmt.Errorf("sourcewb: resolve person: %w", err)
			}
			data.Set("canonical_person_id", pid)
			pos++
			raw, err := data.MarshalJSON()
			if err != nil {
				return err
			}
			id, err := q.SWInsertRow(ctx, gen.SWInsertRowParams{WorkbookID: wbID, WorkspaceID: ws, Position: pos, Data: raw,
				CanonicalEntityID: pgtype.Text{String: identity, Valid: true}, CorroborationCount: pgtype.Int4{Int32: 1, Valid: true}})
			if err != nil {
				return fmt.Errorf("sourcewb: insert row: %w", err)
			}
			w.added++
			w.rowIDs = append(w.rowIDs, int64(id))
			w.events = append(w.events, map[string]any{"type": "row_added", "rowId": int64(id), "data": data,
				"entityId": identity, "corroboration": 1})
		}
		return q.SWFinishWorkbook(ctx, gen.SWFinishWorkbookParams{ID: wbID, WorkspaceID: ws})
	})
	if err != nil {
		return nil, err
	}
	d.log().Info("people_search done", "workbook_id", wbID, "column_id", colID, "companies", len(companies),
		"searches", used, "found", len(found), "added", out.added, "skipped", out.skipped)
	res := obj("found", len(found), "added", out.added, "skipped", out.skipped, "added_row_ids", idsAny(out.rowIDs),
		"companies", len(companies), "searches_used", used, "search_failures", failures)
	if len(found) == 0 && used > 0 && failures >= used {
		res.Set("error", "people_search_unavailable") // every search errored: not evidence of no people
	}
	return res, nil
}

func entityIDs(ctx context.Context, q *gen.Queries, ws, wbID string) (map[string]bool, error) {
	ids, err := q.SWEntityIDs(ctx, gen.SWEntityIDsParams{WorkbookID: wbID, WorkspaceID: ws})
	if err != nil {
		return nil, fmt.Errorf("sourcewb: row identities: %w", err)
	}
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	return present, nil
}
