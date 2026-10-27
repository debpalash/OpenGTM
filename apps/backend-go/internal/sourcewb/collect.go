package sourcewb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// leadLimit is get_leads(collection_job_id=..., limit=10000).
const leadLimit = 10000

// rowFields are the lead fields snapshotted into a workbook row (source_engine._ROW_FIELDS).
var rowFields = []string{"company", "website", "email", "phone", "city", "state", "address",
	"contact_person", "contact_title", "specialization", "company_size", "description", "linkedin_url",
	"twitter_url", "facebook_url", "score", "score_tier", "source", "source_url", "industry_tags",
	"technologies", "technographics", "hiring_signals", "field_provenance", "collection_job_id",
	"created_at", "updated_at"}

// sourceStages are the collection stages reported as exhausted sources.
var sourceStages = map[string]bool{"maps": true, "web": true, "directories": true, "linkedin": true,
	"job_boards": true, "review_sites": true, "registry_sources": true}

func txt(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

func i4(i pgtype.Int4) any {
	if !i.Valid {
		return nil
	}
	return int64(i.Int32)
}

// nowISO is datetime.now(timezone.utc).isoformat() (Lead.__post_init__ default).
func nowISO() string { return collectledger.Isoformat(time.Now().UTC()) }

// leadObj is dataclasses.asdict(Lead.from_dict(row)): Lead field order, NULL as None,
// empty created_at/updated_at stamped with now (__post_init__).
func leadObj(r gen.LeadsByCollectionJobRow) *wbcrud.Obj {
	ts := func(t pgtype.Text) any {
		if !t.Valid || t.String == "" {
			return nowISO()
		}
		return t.String
	}
	return obj(
		"id", int64(r.ID), "company", r.Company, "website", txt(r.Website),
		"email", txt(r.Email), "email_confidence", txt(r.EmailConfidence), "email_provider", txt(r.EmailProvider),
		"phone", txt(r.Phone), "phone_provider", txt(r.PhoneProvider),
		"contact_person", txt(r.ContactPerson), "contact_title", txt(r.ContactTitle),
		"city", txt(r.City), "state", txt(r.State), "address", txt(r.Address),
		"specialization", txt(r.Specialization), "company_size", txt(r.CompanySize),
		"employee_count_exact", i4(r.EmployeeCountExact), "description", txt(r.Description),
		"revenue_range", txt(r.RevenueRange), "founded_year", txt(r.FoundedYear),
		"industry_tags", txt(r.IndustryTags), "technologies", txt(r.Technologies),
		"technographics", txt(r.Technographics), "funding_stage", txt(r.FundingStage),
		"company_size_basis", txt(r.CompanySizeBasis),
		"linkedin_url", txt(r.LinkedinUrl), "twitter_url", txt(r.TwitterUrl), "facebook_url", txt(r.FacebookUrl),
		"secondary_emails", txt(r.SecondaryEmails), "secondary_phones", txt(r.SecondaryPhones),
		"decision_makers", txt(r.DecisionMakers), "glassdoor_rating", txt(r.GlassdoorRating),
		"hiring_signals", txt(r.HiringSignals), "enrichment_attempts", i4(r.EnrichmentAttempts),
		"enrichment_waterfall", txt(r.EnrichmentWaterfall), "field_provenance", txt(r.FieldProvenance),
		"source", txt(r.Source), "source_url", txt(r.SourceUrl), "collection_job_id", txt(r.CollectionJobID),
		"workspace_id", r.WorkspaceID, "score", i4(r.Score), "score_tier", txt(r.ScoreTier), "status", txt(r.Status),
		"yupcha_value_prop", txt(r.YupchaValueProp), "company_need", txt(r.CompanyNeed), "notes", txt(r.Notes),
		"created_at", ts(r.CreatedAt), "updated_at", ts(r.UpdatedAt), "last_enriched_at", txt(r.LastEnrichedAt),
	)
}

func intOr(v any, def int64) int64 {
	if !truthy(v) {
		return def
	}
	if n, ok := pyInt(v); ok {
		return n
	}
	return def
}

// exhausted is the exhausted_sources list of a run's source stages.
func exhausted(stages []collectledger.Stage) []any {
	out := []any{}
	for _, s := range stages {
		if !sourceStages[s.Stage] {
			continue
		}
		status := s.Status
		if status == "" {
			status = "unknown"
		}
		class := ""
		if s.Status == "failed" {
			class = "stage_failed"
		}
		out = append(out, obj("source", s.Stage, "status", status, "output_count", int64(s.OutputCount), "error_class", class))
	}
	return out
}

func retryOptions(status string) []any {
	if status == "partial" {
		return []any{"retry_same_brief", "relax_one_filter", "add_sources"}
	}
	return []any{}
}

// materializeCollection is the ICP path of _materialize_source_impl: run the collector,
// then snapshot the job's leads into rows, deduplicated through the company entity graph.
func (d Deps) materializeCollection(ctx context.Context, ws, wbID, colID string, col wbcrud.Column) (*wbcrud.Obj, error) {
	icp := col.Get("icp")
	targetRows := intOr(col.Get("target_rows"), 0)
	brief, _ := asObj(col.Get("account_discovery_brief"))
	if err := d.setStatus(ctx, ws, wbID, "running"); err != nil {
		return nil, err
	}
	query := BuildQuery(icp)
	if query == "" {
		if err := d.setStatus(ctx, ws, wbID, "draft"); err != nil {
			return nil, err
		}
		return obj("error", "empty_icp", "found", 0, "added", 0), nil
	}

	var stages []collectledger.Stage
	jobID, err := d.Collector.Submit(ctx, ws, query)
	if err == nil {
		err = tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) (e error) {
			stages, e = collectledger.Stages(ctx, tx, jobID)
			if errors.Is(e, collectledger.ErrNotFound) {
				stages, e = nil, nil
			}
			return e
		})
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.log().Error("source run failed", "workbook_id", wbID, "column_id", colID, "err", err)
		if serr := d.setStatus(ctx, ws, wbID, "draft"); serr != nil {
			return nil, serr
		}
		return obj("error", truncRunes(err.Error(), 200), "found", 0, "added", 0, "query", query), nil
	}

	var leads []*wbcrud.Obj
	if err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).LeadsByCollectionJob(ctx, gen.LeadsByCollectionJobParams{JobID: jobID, PageLimit: leadLimit})
		for _, r := range rows {
			leads = append(leads, leadObj(r))
		}
		return err
	}); err != nil {
		d.log().Warn("failed to fetch sourced leads", "workbook_id", wbID, "err_type", fmt.Sprintf("%T", err))
		leads = nil
	}

	found := len(leads)
	rejected := wbcrud.NewObj()
	total := int64(0)
	requested := intOr(get(brief, "requested_count"), 0)
	if requested == 0 {
		requested = targetRows
	}
	status := func(total int64) string {
		if requested == 0 || total >= requested {
			return "complete"
		}
		return "partial"
	}
	exh := exhausted(stages)
	out, err := d.runWrites(ctx, ws, wbID, colID, found, func(tx pgx.Tx, q *gen.Queries, w *writeOut) error {
		rejected = wbcrud.NewObj()
		present, err := entityIDs(ctx, q, ws, wbID)
		if err != nil {
			return err
		}
		stats, err := q.SWRowStats(ctx, gen.SWRowStatsParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		pos := stats.MaxPos
		for _, lead := range leads {
			if targetRows > 0 && stats.N+int64(w.added) >= targetRows {
				break
			}
			if strip(orStr(get(lead, "company"))) == "" {
				w.skipped++
				continue
			}
			if ok, _ := d.Gate.ValidateLight(lead); !ok {
				w.skipped++
				continue
			}
			var fit *wbcrud.Obj
			if brief != nil && brief.Len() > 0 {
				if fit, err = d.Gate.AccountFit(lead, brief); err != nil {
					return fmt.Errorf("sourcewb: account fit: %w", err)
				}
				if !truthy(get(fit, "accepted")) {
					w.skipped++
					reasons := iterate(get(fit, "rejection_reasons"))
					if len(reasons) == 0 {
						reasons = []any{"criteria_not_met"}
					}
					for _, r := range reasons {
						k := pyStr(r)
						n, _ := rejected.Get(k)
						c, _ := n.(int)
						rejected.Set(k, c+1)
					}
					continue
				}
			}
			ent, _, err := entities.ResolveCompany(ctx, tx, ws, lead, orStr(get(lead, "source")))
			if err != nil {
				return fmt.Errorf("sourcewb: resolve company: %w", err)
			}
			if present[ent.ID] { // already a row in this workbook: corroborate, don't duplicate
				w.skipped++
				continue
			}
			present[ent.ID] = true
			data := wbcrud.NewObj()
			for _, k := range rowFields {
				if v := get(lead, k); !emptyValue(v) {
					data.Set(k, v)
				}
			}
			var recordID pgtype.Text
			if fit != nil {
				data.Set("account_id", ent.ID)
				for _, kv := range [][2]string{{"canonical_domain", "canonical_domain"}, {"fit_reasons", "fit_reasons"},
					{"criteria_evidence", "criteria_evidence"}, {"evidence_urls", "evidence_urls"},
					{"retrieved_at", "retrieved_at"}, {"field_confidence", "field_confidence"},
					{"fit_evaluated_at", "evaluated_at"}} {
					data.Set(kv[0], get(fit, kv[1]))
				}
				recordID = pgtype.Text{String: pyStr(get(fit, "canonical_domain")), Valid: true}
			}
			provider := orStr(get(lead, "source"))
			if provider == "" {
				provider = "account_discovery"
			}
			raw, err := data.MarshalJSON()
			if err != nil {
				return err
			}
			pos++
			var leadID pgtype.Int4
			if id, ok := get(lead, "id").(int64); ok {
				leadID = pgtype.Int4{Int32: int32(id), Valid: true}
			}
			id, err := q.SWInsertRow(ctx, gen.SWInsertRowParams{WorkbookID: wbID, WorkspaceID: ws, Position: pos, Data: raw,
				LeadID: leadID, CanonicalEntityID: pgtype.Text{String: ent.ID, Valid: true},
				CorroborationCount: pgtype.Int4{Int32: ent.CorroborationCount, Valid: true},
				SourceProvider:     pgtype.Text{String: provider, Valid: true}, SourceRecordID: recordID})
			if err != nil {
				return fmt.Errorf("sourcewb: insert row: %w", err)
			}
			w.added++
			w.rowIDs = append(w.rowIDs, int64(id))
			w.events = append(w.events, map[string]any{"type": "row_added", "rowId": int64(id), "data": data,
				"entityId": ent.ID, "corroboration": ent.CorroborationCount})
		}

		cfgRaw, err := q.SWLockWorkbook(ctx, gen.SWLockWorkbookParams{ID: wbID, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if total, err = q.SWCountRows(ctx, gen.SWCountRowsParams{WorkbookID: wbID, WorkspaceID: ws}); err != nil {
			return err
		}
		st := status(total)
		summary := obj("status", st, "requested_count", requested, "delivered_count", total,
			"shortfall", max(0, requested-total), "found_count", found, "added_count", w.added,
			"skipped_count", w.skipped, "rejected_by_reason", rejected, "exhausted_sources", exh,
			"retry_options", retryOptions(st), "job_id", jobID)
		cfg, err := wbcrud.ParseObject(nonEmptyJSON(cfgRaw))
		if err != nil {
			cfg = wbcrud.NewObj()
		}
		cfg.Set("last_source_run", summary)
		rawCfg, err := cfg.MarshalJSON()
		if err != nil {
			return err
		}
		return q.SWFinishWorkbook(ctx, gen.SWFinishWorkbookParams{ID: wbID, WorkspaceID: ws, SourceConfig: rawCfg})
	})
	if err != nil {
		return nil, err
	}
	d.log().Info("source done", "workbook_id", wbID, "column_id", colID, "found", found, "added", out.added, "skipped", out.skipped)
	st := status(total)
	return obj("status", st, "requested_count", requested, "delivered_count", total,
		"shortfall", max(0, requested-total), "found", found, "added", out.added, "added_row_ids", idsAny(out.rowIDs),
		"skipped", out.skipped, "rejected_by_reason", rejected, "exhausted_sources", exh,
		"retry_options", retryOptions(st), "query", query, "job_id", jobID), nil
}
