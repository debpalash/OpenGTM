package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// StageResult is complete_stage's arguments.
type StageResult struct {
	Status        string // "done" (default), "failed", "skipped"
	InputCount    int
	OutputCount   int
	RejectedCount int
	Details       string // JSON text ("{}" when empty)
}

// Ledger is the job/stage ledger of one workspace (production: collection_jobs and
// collection_job_stages through PgLedger).
type Ledger interface {
	MarkRunning(ctx context.Context, jobID string) error
	MarkFailed(ctx context.Context, jobID, msg string) error
	CreateStage(ctx context.Context, jobID, stage string) (int64, error)
	CompleteStage(ctx context.Context, stageID int64, r StageResult) error
	// CompleteJob and FailJob move only a running job (fix: legacy overwrote a cancelled one).
	CompleteJob(ctx context.Context, jobID string, leadsFound int) error
	FailJob(ctx context.Context, jobID, msg string) error
	Status(ctx context.Context, jobID string) (string, error)
}

// SourceCounters is source_stats.SourceRunCounters.
type SourceCounters struct{ Emitted, SurvivedDedup, Validated int }

// Store is the workspace's lead table and the global source-reliability ledger.
type Store interface {
	// ExistingCompanies is the set of lower-cased, stripped company names already stored in the
	// workspace (fix: legacy compared against the top 10000 leads by score only).
	ExistingCompanies(ctx context.Context) (map[string]bool, error)
	// Upsert is PgLeadStore.upsert_lead (insert, or overwrite the (workspace, company, city) row).
	Upsert(ctx context.Context, l *Lead) error
	RecordSourceRun(ctx context.Context, region string, counters []SourceRun) error
	Reliability(ctx context.Context, region string) (map[string]float64, error)
}

// SourceRun is one source's tally of a job (keys in first-seen order, as the Python dict).
type SourceRun struct {
	Source string
	SourceCounters
}

// Runner runs collection jobs. All fields but Sleep and Now are required.
type Runner struct {
	Web       Web
	Discovery Discovery
	Sources   Sources
	Enrich    Enrichers
	Knobs     Knobs
	// NewLLM returns an LLM client for the job's workspace; it is called once for discovery and
	// once for scoring (the score stage reports only its own usage).
	NewLLM func() LLM
	Sleep  func(ctx context.Context, d time.Duration) error
}

// Job is one collection run.
type Job struct {
	ID, Query, Intent, WorkspaceID string
	Ledger                         Ledger
	Store                          Store
	Emit                           Emitter
}

type job struct {
	r      *Runner
	id     string
	ws     string
	query  string
	llm    LLM
	ledger Ledger
	store  Store
	em     Emitter
}

func (j *job) emit(typ string, fields ...any) {
	if j.em != nil {
		j.em.Emit(context.Background(), typ, obj(fields...))
	}
}

// ErrCancelled is returned when the job was cancelled (ledger status or context).
var ErrCancelled = errors.New("collect: job cancelled")

// Process is JobRunner._process_job. It returns nil when the run ended in a terminal ledger state
// it set itself (done, intent-blocked failure), ErrCancelled when the ledger says the job was
// cancelled, ctx's error when ctx ended, and otherwise the pipeline error after fail_job. Ledger
// write errors abort the run without fail_job.
func (r *Runner) Process(ctx context.Context, jb Job) error {
	j := &job{r: r, id: jb.ID, ws: jb.WorkspaceID, query: jb.Query, ledger: jb.Ledger, store: jb.Store, em: jb.Emit}
	var intent *string
	if jb.Intent != "" {
		intent = &jb.Intent
	}
	if d := collectintent.Decide(jb.Query, intent); !d.CanCollect() {
		msg := "collection_intent_blocked: " + d.Reason
		if err := j.ledger.MarkFailed(ctx, j.id, msg); err != nil {
			return err
		}
		j.emit("job_failed", "job_id", j.id, "error", msg,
			"message", "Collection blocked before source execution; choose a specialized GTM workflow.")
		return nil
	}
	err := j.run(ctx)
	if ctx.Err() != nil { // worker cancel, lease loss, deadline or shutdown: the queue decides
		return ctx.Err()
	}
	if errors.Is(err, ErrCancelled) {
		return ErrCancelled
	}
	var lerr *ledgerError
	if errors.As(err, &lerr) {
		return lerr.err
	}
	if err != nil {
		if ferr := j.ledger.FailJob(context.WithoutCancel(ctx), j.id, err.Error()); ferr != nil {
			return ferr
		}
		j.emit("job_failed", "job_id", j.id, "error", err.Error(), "message", "❌ Failed: "+err.Error())
		return err
	}
	return nil
}

// ledgerError marks a ledger write failure (it aborts the run without fail_job).
type ledgerError struct{ err error }

func (e *ledgerError) Error() string { return e.err.Error() }

func (j *job) createStage(ctx context.Context, name string) (int64, error) {
	id, err := j.ledger.CreateStage(ctx, j.id, name)
	if err != nil {
		return 0, &ledgerError{err}
	}
	return id, nil
}

func (j *job) completeStage(ctx context.Context, id int64, r StageResult) error {
	if r.Details == "" {
		r.Details = "{}"
	}
	if r.Status == "" {
		r.Status = "done"
	}
	if err := j.ledger.CompleteStage(context.WithoutCancel(ctx), id, r); err != nil {
		return &ledgerError{err}
	}
	return nil
}

func (j *job) cancelled(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	st, err := j.ledger.Status(ctx, j.id)
	if err != nil {
		return false, &ledgerError{err}
	}
	return st == "cancelled", nil
}

type strategy struct {
	name string
	run  func(context.Context) []*Lead
}

func (j *job) run(ctx context.Context) error {
	if err := j.ledger.MarkRunning(ctx, j.id); err != nil {
		return &ledgerError{err}
	}
	j.emit("job_started", "job_id", j.id, "query", j.query, "message", "🚀 Starting collection: '"+j.query+"'")
	j.llm = j.r.NewLLM()
	src := j.r.Sources
	on := func(id string) bool { return src.Enabled(ctx, id) }
	noop := func(context.Context) []*Lead { return nil }
	pick := func(ok bool, f func(context.Context) []*Lead) func(context.Context) []*Lead {
		if ok {
			return f
		}
		return noop
	}
	tasks := []strategy{
		{"maps", pick(IsLocationQuery(j.query) && on("google_maps"), j.searchMaps)},
		{"web", pick(on("duckduckgo"), j.searchAndScrape)},
		{"directories", pick(on("directories"), j.searchDirectories)},
		{"linkedin", pick(on("linkedin"), j.searchLinkedin)},
		{"job_boards", pick(on("job_boards"), j.searchJobBoards)},
		{"review_sites", pick(on("ambitionbox"), j.searchReviewSites)},
	}
	if on("directories") {
		tasks = append(tasks, strategy{"registry_sources", j.searchRegistrySources})
	}
	j.progress("parallel", fmt.Sprintf("⚡ Running %d strategies in parallel...", len(tasks)))
	stageIDs := make([]int64, len(tasks))
	for i, t := range tasks {
		id, err := j.createStage(ctx, t.name)
		if err != nil {
			return err
		}
		stageIDs[i] = id
	}
	results := make([][]*Lead, len(tasks))
	errs := make([]error, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(i int, t strategy) {
			defer wg.Done()
			results[i], errs[i] = j.runStrategy(ctx, t, stageIDs[i])
		}(i, t)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	var all []*Lead
	for _, r := range results {
		all = append(all, r...)
	}
	for _, l := range all {
		if l.SourceURL == "" {
			if l.Source == "linkedin" && l.LinkedinURL != "" {
				l.SourceURL = l.LinkedinURL
			} else if l.Website != "" {
				l.SourceURL = l.Website
			}
		}
	}
	tally := newTally()
	for _, l := range all {
		tally.get(l.Source).Emitted++
	}
	region := DetectRegion(j.query, "")
	if c, err := j.cancelled(ctx); err != nil || c {
		return j.stopCancelled(err)
	}

	// light validation
	vid, err := j.createStage(ctx, "validate")
	if err != nil {
		return err
	}
	j.progress("validate", fmt.Sprintf("🔍 Light-validating %d leads (names only)...", len(all)))
	valid, rejected := ValidateAll(all, true)
	var reasons counter
	if len(rejected) > 0 {
		for _, r := range rejected {
			reasons.add(r.Reason)
		}
		j.progress("validate", fmt.Sprintf("🗑️ Rejected %d garbage: %s", len(rejected), reprCounts(reasons.dict())))
	}
	var rejNames []any
	for _, r := range slice2(rejected, 20) {
		rejNames = append(rejNames, r.Lead.Company)
	}
	if err := j.completeStage(ctx, vid, StageResult{InputCount: len(all), OutputCount: len(valid), RejectedCount: len(rejected),
		Details: pyDumps(dict{{"reasons", reasons.dict()}, {"rejected_names", orEmpty(rejNames)}})}); err != nil {
		return err
	}

	// dedup (in batch, then against the workspace)
	did, err := j.createStage(ctx, "dedup")
	if err != nil {
		return err
	}
	unique := Dedupe(valid, DedupThreshold)
	existing, err := j.store.ExistingCompanies(ctx)
	if err != nil {
		existing = nil // legacy swallowed the read error (no cross-job dedup)
	}
	crossRemoved := 0
	var dedupRemoved []string
	if len(existing) > 0 {
		var kept []*Lead
		for _, l := range unique {
			if existing[pyStrip(pyLower(l.Company))] {
				dedupRemoved = append(dedupRemoved, l.Company)
			} else {
				kept = append(kept, l)
			}
		}
		crossRemoved = len(unique) - len(kept)
		unique = kept
	}
	uniqueSet := map[string]bool{}
	for _, l := range unique {
		uniqueSet[pyStrip(pyLower(l.Company))] = true
	}
	var dedupRejected []any
	for _, l := range valid {
		if !uniqueSet[pyStrip(pyLower(l.Company))] && !contains(dedupRemoved, l.Company) {
			dedupRejected = append(dedupRejected, l.Company)
		}
	}
	for _, n := range dedupRemoved {
		dedupRejected = append(dedupRejected, n)
	}
	if err := j.completeStage(ctx, did, StageResult{InputCount: len(valid), OutputCount: len(unique),
		RejectedCount: len(valid) - len(unique), Details: pyDumps(dict{{"cross_job_removed", crossRemoved},
			{"rejected_names", orEmpty(slice2(dedupRejected, 20))}})}); err != nil {
		return err
	}
	j.progress("dedup", fmt.Sprintf("🔄 %d → %d after dedup (%d already in DB)", len(valid), len(unique), crossRemoved))
	for _, l := range unique {
		if c := tally.find(l.Source); c != nil {
			c.SurvivedDedup++
		}
	}
	if c, err := j.cancelled(ctx); err != nil || c {
		return j.stopCancelled(err)
	}

	scored, err := j.scoreStage(ctx, unique)
	if err != nil {
		return err
	}
	if scored, err = j.enrichStage(ctx, scored); err != nil {
		return err
	}
	if scored, err = j.postValidateStage(ctx, scored); err != nil {
		return err
	}
	if err := j.decisionMakersStage(ctx, scored); err != nil {
		return err
	}
	if err := j.waterfallStage(ctx, scored); err != nil {
		return err
	}
	if err := j.personalEmailsStage(ctx, scored); err != nil {
		return err
	}
	if err := j.crosslinkedStage(ctx, scored); err != nil {
		return err
	}
	if err := j.hiringStage(ctx, scored); err != nil {
		return err
	}
	if err := j.smtpStage(ctx, scored); err != nil {
		return err
	}
	if err := j.deliverabilityStage(ctx, scored); err != nil {
		return err
	}
	for _, l := range scored {
		l.Score = max(l.Score, leadscore.Score(l, nil))
		l.ScoreTier = leadscore.Tier(l.Score)
	}
	if j.r.Knobs.SourceReliability {
		if rmap, err := j.store.Reliability(ctx, region); err == nil {
			nudged := 0
			for _, l := range scored {
				r, ok := rmap[NormalizeSource(l.Source)]
				if !ok {
					continue
				}
				if ns := leadscore.ApplyReliability(l.Score, &r); ns != l.Score {
					l.Score, l.ScoreTier = ns, leadscore.Tier(ns)
					nudged++
				}
			}
			if nudged > 0 {
				j.progress("score", fmt.Sprintf("⚖️ Source-reliability nudged %d lead scores", nudged))
			}
		}
	}

	// final quality gate
	qid, err := j.createStage(ctx, "quality_gate")
	if err != nil {
		return err
	}
	var qualified, unq []*Lead
	for _, l := range scored {
		l.ScoreTier = leadscore.Tier(l.Score)
		if l.ScoreTier == "unqualified" {
			unq = append(unq, l)
		} else {
			qualified = append(qualified, l)
		}
	}
	var tiers counter
	for _, l := range qualified {
		tiers.add(l.ScoreTier)
		if c := tally.find(l.Source); c != nil {
			c.Validated++
		}
	}
	var unqNames []any
	for _, l := range slice2(unq, 20) {
		unqNames = append(unqNames, l.Company)
	}
	if err := j.completeStage(ctx, qid, StageResult{InputCount: len(scored), OutputCount: len(qualified),
		RejectedCount: len(unq), Details: pyDumps(dict{{"tiers", tiers.dict()}, {"reason", "unqualified_score"},
			{"rejected_names", orEmpty(unqNames)}})}); err != nil {
		return err
	}
	j.progress("quality_gate", fmt.Sprintf("🛡️ Quality gate: %d qualified, %d unqualified rejected", len(qualified), len(unq)))
	scored = qualified

	// Fix: legacy never looked at the cancel flag again after dedup, so a cancel during
	// enrichment still stored the leads and flipped the job to done.
	if c, err := j.cancelled(ctx); err != nil || c {
		return j.stopCancelled(err)
	}

	sid, err := j.createStage(ctx, "store")
	if err != nil {
		return err
	}
	count := 0
	for _, l := range scored {
		l.CollectionJobID = j.id
		if l.SourceURL == "" {
			if l.Source == "linkedin" && l.LinkedinURL != "" {
				l.SourceURL = l.LinkedinURL
			} else {
				l.SourceURL = l.Website
			}
		}
		l.WorkspaceID = j.ws
		if err := j.store.Upsert(ctx, l); err != nil {
			return err
		}
		count++
		j.emit("lead_stored", "job_id", j.id, "lead", obj("company", l.Company, "email", l.Email, "phone", l.Phone,
			"city", l.City, "score", l.Score, "score_tier", l.ScoreTier))
	}
	if err := j.completeStage(ctx, sid, StageResult{InputCount: len(scored), OutputCount: count}); err != nil {
		return err
	}
	_ = j.store.RecordSourceRun(ctx, region, tally.runs()) // never fatal (legacy logged at debug)
	if err := j.ledger.CompleteJob(ctx, j.id, count); err != nil {
		return &ledgerError{err}
	}
	msg := "✅ Collection finished: no qualified leads were stored"
	if count > 0 {
		msg = fmt.Sprintf("✅ Done! %d qualified leads stored", count)
	}
	j.emit("job_completed", "job_id", j.id, "query", j.query, "leads_found", count, "raw_total", len(all),
		"rejected", len(rejected), "message", msg)
	return nil
}

func (j *job) stopCancelled(err error) error {
	if err != nil {
		return err
	}
	j.progress("cancelled", "🛑 Job cancelled by user")
	return ErrCancelled
}

func orEmpty(xs []any) []any {
	if xs == nil {
		return []any{}
	}
	return xs
}

func (j *job) runStrategy(ctx context.Context, t strategy, sid int64) ([]*Lead, error) {
	sctx, cancel := context.WithTimeout(ctx, j.r.Knobs.StrategyTimeout)
	defer cancel()
	var leads []*Lead
	done := make(chan struct{})
	var panicked any
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		leads = t.run(sctx)
	}()
	<-done
	switch {
	case panicked != nil:
		msg := fmt.Sprint(panicked)
		if err := j.completeStage(ctx, sid, StageResult{Status: "failed", Details: pyDumps(dict{{"error", msg}})}); err != nil {
			return nil, err
		}
		j.progress(t.name, fmt.Sprintf("⚠️ %s failed: %s", t.name, msg))
		return nil, nil
	case sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		secs := int(j.r.Knobs.StrategyTimeout / time.Second)
		if err := j.completeStage(ctx, sid, StageResult{Status: "failed",
			Details: pyDumps(dict{{"error", fmt.Sprintf("Timed out after %ds", secs)}})}); err != nil {
			return nil, err
		}
		j.progress(t.name, fmt.Sprintf("⚠️ %s timed out", t.name))
		return nil, nil
	}
	var samples []any
	for _, l := range slice2(leads, 10) {
		samples = append(samples, l.Company)
	}
	if err := j.completeStage(ctx, sid, StageResult{OutputCount: len(leads), Details: pyDumps(dict{{"samples", orEmpty(samples)}})}); err != nil {
		return nil, err
	}
	j.progress(t.name, fmt.Sprintf("✅ %s: %d leads", t.name, len(leads)), "leads_found", len(leads))
	return leads, nil
}

// ── score ───────────────────────────────────────────────────────────────────

func (j *job) scoreStage(ctx context.Context, unique []*Lead) ([]*Lead, error) {
	id, err := j.createStage(ctx, "score")
	if err != nil {
		return nil, err
	}
	j.progress("score", fmt.Sprintf("🤖 AI scoring %d leads...", len(unique)))
	sc := j.r.NewLLM()
	applied := AIScoreLeads(ctx, sc, unique)
	aiUsed := applied > 0
	if !aiUsed && len(unique) > 0 {
		j.progress("score", "⚠️ AI scoring unavailable; using transparent heuristic scoring")
	}
	var zero []*Lead
	for _, l := range unique {
		if l.Score == 0 {
			zero = append(zero, l)
		}
	}
	if len(zero) > 0 {
		j.progress("score", fmt.Sprintf("📊 Heuristic fallback for %d zero-scored leads...", len(zero)))
		for _, l := range zero {
			l.Score = leadscore.Score(l, nil)
			l.ScoreTier = leadscore.Tier(l.Score)
		}
	}
	var tiers counter
	for _, l := range unique {
		tiers.add(or(l.ScoreTier, "unknown"))
	}
	usage := sc.Usage()
	errs := usage.Errors
	if errs == nil {
		errs = []string{}
	}
	var tokens any = dict{}
	if aiUsed {
		tokens = dict{{"prompt_tokens", usage.PromptTokens}, {"completion_tokens", usage.CompletionTokens},
			{"total_tokens", usage.TotalTokens}, {"cache_write_tokens", usage.CacheWriteTokens},
			{"cache_read_tokens", usage.CacheReadTokens}, {"calls", usage.Calls}, {"provider", usage.Provider},
			{"errors", errs}}
	}
	if err := j.completeStage(ctx, id, StageResult{InputCount: len(unique), OutputCount: len(unique),
		Details: pyDumps(dict{{"tiers", tiers.dict()}, {"ai", aiUsed}, {"ai_applied", applied},
			{"fallback_count", len(zero)}, {"errors", errs}, {"tokens", tokens}})}); err != nil {
		return nil, err
	}
	return unique, nil
}

// ── enrich (website discovery + website scraping) ───────────────────────────

func (j *job) enrichStage(ctx context.Context, scored []*Lead) ([]*Lead, error) {
	id, err := j.createStage(ctx, "enrich")
	if err != nil {
		return nil, err
	}
	j.progress("enrich", fmt.Sprintf("🌐 Enriching %d leads...", len(scored)))
	var needs []*Lead
	for _, l := range scored {
		if l.Website == "" || strings.Contains(l.Website, "linkedin.com") {
			needs = append(needs, l)
		}
	}
	if len(needs) > 0 {
		j.progress("enrich", fmt.Sprintf("🔍 Finding websites for %d leads...", len(needs)))
		for _, l := range slice2(needs, 15) {
			if ctx.Err() != nil {
				break
			}
			q := `"` + l.Company + `" official website`
			if l.City != "" {
				q += " " + l.City
			}
			for _, r := range j.search(ctx, q, 3) {
				if r.Href == "" {
					continue
				}
				if IsSkipDomain(hostOf(r.Href)) {
					continue
				}
				l.Website = r.Href
				if page, ok := j.fetch(ctx, r.Href, 8*time.Second); ok {
					emails, phones := leadweb.EmailsFromHTML(page), leadweb.PhonesFromHTML(page)
					if len(emails) > 0 && l.Email == "" {
						l.Email = emails[0]
					}
					if len(phones) > 0 && l.Phone == "" {
						l.Phone = phones[0]
					}
				}
				break
			}
			_ = j.sleep(ctx, 500*time.Millisecond)
		}
	}
	EnrichFromWebsites(ctx, j.r.Web, scored, 5)
	if err := j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: len(scored)}); err != nil {
		return nil, err
	}
	return scored, nil
}

func (j *job) postValidateStage(ctx context.Context, scored []*Lead) ([]*Lead, error) {
	id, err := j.createStage(ctx, "post_validate")
	if err != nil {
		return nil, err
	}
	pre := len(scored)
	valid, rejected := ValidateAll(scored, false)
	var reasons counter
	if len(rejected) > 0 {
		for _, r := range rejected {
			reasons.add(r.Reason)
		}
		j.progress("post_validate", fmt.Sprintf("🔍 Post-enrichment: removed %d leads still without contact data", len(rejected)))
	}
	if err := j.completeStage(ctx, id, StageResult{InputCount: pre, OutputCount: len(valid), RejectedCount: len(rejected),
		Details: pyDumps(dict{{"reasons", reasons.dict()}})}); err != nil {
		return nil, err
	}
	if valid == nil {
		valid = []*Lead{}
	}
	return valid, nil
}

// ── decision makers ─────────────────────────────────────────────────────────

func (j *job) decisionMakersStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "decision_makers")
	if err != nil {
		return err
	}
	j.progress("decision_makers", fmt.Sprintf("👤 Finding decision makers for %d leads...", len(scored)))
	var cands []*Lead
	for _, l := range scored {
		if (l.ScoreTier == "hot" || l.ScoreTier == "warm") && l.DecisionMakers == "" {
			cands = append(cands, l)
		}
	}
	cands = slice2(cands, max(j.r.Knobs.DMBudget, 0))
	if len(cands) > 0 {
		EnrichDecisionMakers(ctx, j.r.Web, j.sleep, cands, 2, 2)
		var fallback []*Lead
		for _, l := range cands {
			if l.DecisionMakers == "" {
				fallback = append(fallback, l)
			}
		}
		for _, l := range slice2(fallback, 5) {
			people, err := j.r.Discovery.DecisionMakers(ctx, l.Company, 3)
			if err != nil || len(people) == 0 {
				continue
			}
			list := make([]any, len(people))
			for i, p := range people {
				list[i] = dict{{"name", p.Name}, {"title", p.Title}, {"linkedin_url", p.LinkedinURL}}
			}
			l.DecisionMakers = pyDumps(list)
			if l.ContactPerson == "" {
				l.ContactPerson, l.ContactTitle = people[0].Name, people[0].Title
			}
			if l.LinkedinURL == "" && people[0].LinkedinURL != "" {
				l.LinkedinURL = people[0].LinkedinURL
			}
		}
		found := 0
		for _, l := range cands {
			if l.DecisionMakers != "" {
				found++
			}
		}
		j.progress("decision_makers", fmt.Sprintf("👤 Found decision makers for %d/%d leads", found, len(cands)))
	}
	for _, l := range scored {
		if l.DecisionMakers != "" || l.HasContactPerson() {
			l.Score = leadscore.Score(l, nil)
			l.ScoreTier = leadscore.Tier(l.Score)
		}
	}
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: len(scored)})
}

// ── email waterfall ─────────────────────────────────────────────────────────

func (j *job) waterfallStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "email_waterfall")
	if err != nil {
		return err
	}
	j.progress("email_waterfall", "📨 Email-finder waterfall...")
	var cands []*Lead
	for _, l := range scored {
		if l.Company != "" && !l.HasEmail() {
			cands = append(cands, l)
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].Score > cands[b].Score })
	budget := max(j.r.Knobs.EmailWaterfallBudget, 0)
	// enrich_emails_waterfall re-filters (company, no email) and takes the first budget.
	for _, l := range slice2(cands, budget) {
		if ctx.Err() != nil {
			break
		}
		if l.Company == "" || l.HasEmail() {
			continue
		}
		_ = j.r.Enrich.Waterfall(ctx, j.ws, l)
	}
	found := 0
	for _, l := range slice2(cands, budget) {
		if l.EmailProvider != "" {
			found++
		}
	}
	j.progress("email_waterfall", fmt.Sprintf("📨 Waterfall found %d emails", found))
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: found})
}

// ── personal emails ─────────────────────────────────────────────────────────

func (j *job) personalEmailsStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "personal_emails")
	if err != nil {
		return err
	}
	j.progress("personal_emails", "📧 Finding personal emails...")
	EnrichPersonalEmails(ctx, j.r.Web, j.r.Enrich, j.sleep, scored, time.Second)
	count := func(confs ...string) int {
		n := 0
		for _, l := range scored {
			if contains(confs, l.EmailConfidence) {
				n++
			}
		}
		return n
	}
	j.progress("personal_emails", fmt.Sprintf("📧 Found %d personal emails", count("verified", "pattern")))
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: count("verified", "pattern", "smtp_verified"),
		Details: pyDumps(dict{{"verified", count("verified")}, {"pattern", count("pattern")},
			{"smtp_verified", count("smtp_verified")}})})
}

// ── provider stages (CrossLinked, JobSpy, MailScout) ────────────────────────

func (j *job) crosslinkedStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "crosslinked")
	if err != nil {
		return err
	}
	j.progress("crosslinked", "🔗 Finding LinkedIn people...")
	var cands []*Lead
	for _, l := range scored {
		if l.Score >= 40 && l.DecisionMakers == "" {
			cands = append(cands, l)
		}
	}
	cands = slice2(cands, 10)
	found := 0
	for _, l := range cands {
		if ctx.Err() != nil {
			break
		}
		res, err := j.r.Enrich.Provider(ctx, j.ws, "crosslinked", l)
		if err != nil {
			continue
		}
		if res.Success {
			if v := fieldStr(res.Fields, "decision_makers"); v != "" {
				l.DecisionMakers = v
			}
			if v := fieldStr(res.Fields, "contact_person"); v != "" && l.ContactPerson == "" {
				l.ContactPerson = v
			}
			if v := fieldStr(res.Fields, "contact_title"); v != "" && l.ContactTitle == "" {
				l.ContactTitle = v
			}
			found++
		}
		_ = j.sleep(ctx, 500*time.Millisecond)
	}
	j.progress("crosslinked", fmt.Sprintf("🔗 Found LinkedIn people for %d/%d leads", found, len(cands)))
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: len(scored)})
}

func (j *job) hiringStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "hiring_signals")
	if err != nil {
		return err
	}
	j.progress("hiring_signals", "📊 Detecting hiring signals...")
	var cands []*Lead
	for _, l := range scored {
		if l.Score >= 50 {
			cands = append(cands, l)
		}
	}
	cands = slice2(cands, 8)
	found := 0
	for _, l := range cands {
		if ctx.Err() != nil {
			break
		}
		res, err := j.r.Enrich.Provider(ctx, j.ws, "jobspy", l)
		if err != nil {
			continue
		}
		if v := fieldStr(res.Fields, "hiring_signals"); res.Success && v != "" {
			l.HiringSignals = v
			var sig map[string]any
			if json.Unmarshal([]byte(v), &sig) == nil {
				if b, ok := sig["score_boost"].(float64); ok {
					l.Score = min(100, l.Score+int(b))
				}
			}
			found++
		}
		_ = j.sleep(ctx, 500*time.Millisecond)
	}
	j.progress("hiring_signals", fmt.Sprintf("📊 Found hiring signals for %d/%d leads", found, len(cands)))
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: len(scored)})
}

func (j *job) smtpStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "smtp_verify")
	if err != nil {
		return err
	}
	j.progress("smtp_verify", "✉️ Verifying emails via SMTP...")
	var cands []*Lead
	for _, l := range scored {
		if l.Email != "" && strings.Contains(l.Email, "@") && l.EmailConfidence != "smtp_verified" && l.Score >= 40 {
			cands = append(cands, l)
		}
	}
	cands = slice2(cands, 10)
	verified := 0
	for _, l := range cands {
		if ctx.Err() != nil {
			break
		}
		res, err := j.r.Enrich.Provider(ctx, j.ws, "mailscout", l)
		if err != nil {
			continue
		}
		if res.Success {
			if v := fieldStr(res.Fields, "email"); v != "" {
				l.Email = v
			}
			if v := fieldStr(res.Fields, "email_confidence"); v != "" {
				l.EmailConfidence = v
				l.EmailProvider = "mailscout"
			}
			if l.EmailConfidence == "smtp_verified" {
				verified++
			}
		}
	}
	j.progress("smtp_verify", fmt.Sprintf("✉️ SMTP verified %d/%d emails", verified, len(cands)))
	total := 0
	for _, l := range scored {
		if l.EmailConfidence == "smtp_verified" {
			total++
		}
	}
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: total,
		Details: pyDumps(dict{{"smtp_verified", total}})})
}

func (j *job) deliverabilityStage(ctx context.Context, scored []*Lead) error {
	id, err := j.createStage(ctx, "deliverability")
	if err != nil {
		return err
	}
	j.progress("deliverability", "🏷️ Tagging email deliverability...")
	counts := dict{{"verified", 0}, {"risky", 0}, {"unknown", 0}, {"dropped", 0}}
	bump := func(k string) {
		for i := range counts {
			if counts[i].K == k {
				counts[i].V = counts[i].V.(int) + 1
			}
		}
	}
	var cands []*Lead
	for _, l := range scored {
		if l.HasEmail() && l.EmailConfidence != "smtp_verified" {
			cands = append(cands, l)
		}
	}
	for _, l := range slice2(cands, max(j.r.Knobs.DeliverabilityBudget, 0)) {
		if ctx.Err() != nil {
			break
		}
		keep, conf, ok := j.r.Enrich.Deliverability(ctx, j.ws, l)
		if !ok {
			continue
		}
		if !keep {
			bump("dropped")
		} else if conf == "verified" || conf == "risky" || conf == "unknown" {
			bump(conf)
		}
	}
	n := func(k string) int { v, _ := counts.get(k); return v.(int) }
	j.progress("deliverability", fmt.Sprintf("🏷️ verified=%d risky=%d unknown=%d dropped=%d", n("verified"), n("risky"),
		n("unknown"), n("dropped")))
	return j.completeStage(ctx, id, StageResult{InputCount: len(scored), OutputCount: n("verified") + n("risky"),
		Details: pyDumps(counts)})
}

func fieldStr(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok || !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

// ── source-reliability tally (source_stats.py) ──────────────────────────────

// NormalizeSource is source_stats.normalize_source.
func NormalizeSource(raw string) string {
	s := pyLower(pyStrip(raw))
	if s == "" {
		return "unknown"
	}
	if strings.HasPrefix(s, "registry:") {
		s = pyStrip(s[len("registry:"):])
	}
	if s == "" {
		return "unknown"
	}
	return s
}

type tally struct {
	order []string
	m     map[string]*SourceCounters
}

func newTally() *tally { return &tally{m: map[string]*SourceCounters{}} }

func (t *tally) get(source string) *SourceCounters {
	k := NormalizeSource(source)
	c, ok := t.m[k]
	if !ok {
		c = &SourceCounters{}
		t.m[k] = c
		t.order = append(t.order, k)
	}
	return c
}

func (t *tally) find(source string) *SourceCounters { return t.m[NormalizeSource(source)] }

func (t *tally) runs() []SourceRun {
	out := make([]SourceRun, 0, len(t.order))
	for _, k := range t.order {
		out = append(out, SourceRun{k, *t.m[k]})
	}
	return out
}
