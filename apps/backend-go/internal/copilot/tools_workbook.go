package copilot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	collectq "github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports the workbook-authoring and collection tools of services/copilot/tools.py execute().
// Workbook rows are filtered by workspace explicitly (Python's ORM had no RLS backstop) and run inside
// tenancy.InWorkspace, so RLS also applies.

func hex8() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (e *Engine) refreshExecutor(ws string) string {
	if e.RefreshExecutor != nil {
		return e.RefreshExecutor(ws)
	}
	return "python" // go_executor.refresh_executor without the cohort flags
}

// workbookInWS reports whether the workbook exists in ws (and locks it for the update that follows).
func workbookInWS(ctx context.Context, tx pgx.Tx, ws, id string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM workbooks WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, id, ws).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// pyJSONText renders v as json.dumps (SQLAlchemy JSON columns store exactly that).
func pyJSONText(v any) (string, error) {
	var b []byte
	var err error
	if o, ok := v.(*wbcrud.Obj); ok {
		b, err = o.MarshalJSON()
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return "", err
	}
	r, err := ssestream.Reformat(b)
	return string(r), err
}

// policyResult renders {"message": ..., **set_refresh_policy(...)}.
func policyResult(message string, policy *wbcrud.Obj, res map[string]any) (string, error) {
	out := wbcrud.NewObj()
	out.Set("message", message)
	if e, ok := res["error"]; ok {
		out.Set("error", e)
	} else {
		out.Set("refresh_policy", policy)
		out.Set("next_in_minutes", res["next_in_minutes"])
	}
	return objJSON(out)
}

// objWithout is a copy of o without key (dict.pop).
func objWithout(o *wbcrud.Obj, key string) *wbcrud.Obj {
	out := wbcrud.NewObj()
	for _, k := range o.Keys() {
		if k != key {
			v, _ := o.Get(k)
			out.Set(k, v)
		}
	}
	return out
}

func objToMap(o *wbcrud.Obj) map[string]any {
	m := make(map[string]any, o.Len())
	for _, k := range o.Keys() {
		m[k], _ = o.Get(k)
	}
	return m
}

func (t *Turn) setWorkbookRefresh(ctx context.Context, a map[string]any) (string, bool, error) {
	raw, present := a["workbook_id"]
	if !present {
		return keyErr("set_workbook_refresh", "workbook_id"), true, nil
	}
	wbID, ok := raw.(string)
	if !ok {
		return "", false, nil
	}
	var out string
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		found, err := workbookInWS(ctx, tx, t.WS, wbID)
		if err != nil {
			return err
		}
		if !found {
			out = errObj("Workbook not found")
			return nil
		}
		interval, present := a["interval"]
		if !present {
			out = keyErr("set_workbook_refresh", "interval")
			return nil
		}
		policy := wbcrud.NewObj()
		policy.Set("enabled", true)
		policy.Set("interval", interval)
		res, err := refresh.SetPolicyExecutor(ctx, tx, wbID, objToMap(policy), t.Engine.refreshExecutor(t.WS))
		if err != nil {
			return err
		}
		if _, isErr := res["error"]; isErr {
			out = errObj(pyStr(res["error"]))
			return nil
		}
		out, err = policyResult("Workbook will refresh "+pyStr(interval)+".", policy, res)
		return err
	})
	return out, err == nil, err
}

// pyFloatArg is float(v) for the argument shapes mirrored here.
func pyFloatArg(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimFunc(x, isPySpaceRune), 64)
		return f, err == nil
	}
	return 0, false
}

func (t *Turn) addAgentColumn(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "add_agent_column"
	raw, present := a["workbook_id"]
	if !present {
		return keyErr(name, "workbook_id"), true, nil
	}
	wbID, ok := raw.(string)
	if !ok {
		return "", false, nil
	}
	cost := 0.10
	if v, present := a["max_cost_usd"]; present {
		if cost, ok = pyFloatArg(v); !ok {
			return "", false, nil
		}
	}
	var out string
	handled := true
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		found, err := workbookInWS(ctx, tx, t.WS, wbID)
		if err != nil || !found {
			out = errObj("Workbook not found")
			return err
		}
		for _, k := range []string{"column_name", "goal", "target_field"} {
			if _, present := a[k]; !present {
				out = keyErr(name, k)
				return nil
			}
		}
		pol := wbcrud.NewObj()
		pol.Set("max_steps", int64(6))
		pol.Set("max_cost_usd", cost)
		col := wbcrud.NewObj()
		col.Set("id", "agent_"+hex8())
		col.Set("name", a["column_name"])
		col.Set("type", "agent")
		col.Set("goal", a["goal"])
		col.Set("target_field", a["target_field"])
		col.Set("policy", pol)
		var stored *string
		if err := tx.QueryRow(ctx, `SELECT columns_config::text FROM workbooks WHERE id = $1 AND workspace_id = $2`, wbID, t.WS).Scan(&stored); err != nil {
			return err
		}
		var cols []json.RawMessage
		if stored != nil && *stored != "null" {
			if err := json.Unmarshal([]byte(*stored), &cols); err != nil {
				handled = false // not a list: Python's list() would treat it differently
				return nil
			}
		}
		parts := make([]string, 0, len(cols)+1)
		for _, c := range cols {
			r, err := ssestream.Reformat(c)
			if err != nil {
				return err
			}
			parts = append(parts, string(r))
		}
		cj, err := pyJSONText(col)
		if err != nil {
			return err
		}
		parts = append(parts, cj)
		if _, err := tx.Exec(ctx, `UPDATE workbooks SET columns_config = $1::json, updated_at = (now() AT TIME ZONE 'utc')
			WHERE id = $2 AND workspace_id = $3`, "["+strings.Join(parts, ", ")+"]", wbID, t.WS); err != nil {
			return err
		}
		res := wbcrud.NewObj()
		res.Set("added", a["column_name"])
		res.Set("type", "agent")
		res.Set("message", "Added agent column '"+pyStr(a["column_name"])+"' (goal: "+pyStr(a["goal"])+").")
		out, err = objJSON(res)
		return err
	})
	if !handled {
		return "", false, nil
	}
	return out, err == nil, err
}

// signalsJoin is ', '.join(args['signals']); ok=false where Python raises.
func signalsJoin(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		parts := make([]string, 0, len(x))
		for _, r := range x {
			parts = append(parts, string(r))
		}
		return strings.Join(parts, ", "), true
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ", "), true
	}
	return "", false
}

func (t *Turn) addSignalTrigger(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "add_signal_trigger"
	raw, present := a["workbook_id"]
	if !present {
		return keyErr(name, "workbook_id"), true, nil
	}
	wbID, ok := raw.(string)
	if !ok {
		return "", false, nil
	}
	var out string
	handled := true
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		found, err := workbookInWS(ctx, tx, t.WS, wbID)
		if err != nil || !found {
			out = errObj("Workbook not found")
			return err
		}
		var stored *string
		if err := tx.QueryRow(ctx, `SELECT refresh_policy::text FROM workbooks WHERE id = $1 AND workspace_id = $2`, wbID, t.WS).Scan(&stored); err != nil {
			return err
		}
		policy := wbcrud.NewObj()
		if stored != nil && *stored != "null" {
			p, err := wbcrud.ParseObject([]byte(*stored))
			if err != nil {
				handled = false
				return nil
			}
			if p.Len() > 0 {
				policy = p
			}
		}
		signals, present := a["signals"]
		if !present {
			out = keyErr(name, "signals")
			return nil
		}
		joined, ok := signalsJoin(signals)
		if !ok {
			handled = false
			return nil
		}
		policy.Set("enabled", true)
		policy.Set("on_signal", signals)
		res, err := refresh.SetPolicyExecutor(ctx, tx, wbID, objToMap(policy), t.Engine.refreshExecutor(t.WS))
		if err != nil {
			return err
		}
		out, err = policyResult("Workbook will refresh on signals: "+joined+".", policy, res)
		return err
	})
	if !handled {
		return "", false, nil
	}
	return out, err == nil, err
}

// ---- start_collection ----

const collectStartedMsg = "Collection started with 6 strategies: Maps, Web, Directories, LinkedIn, Job Boards, Review Sites"

type collectPayload struct {
	JobID       string `json:"job_id"`
	Query       string `json:"query"`
	Intent      string `json:"intent"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
}

func (t *Turn) startCollection(ctx context.Context, a map[string]any) (string, bool, error) {
	e := t.Engine
	if !e.CollectionLedgerPG {
		return "", false, nil // the SQLite collection ledger is Python's
	}
	raw, present := a["query"]
	if !present {
		return keyErr("start_collection", "query"), true, nil
	}
	query, ok := raw.(string)
	if !ok {
		return "", false, nil
	}
	var intent *string
	switch v := a["intent"].(type) {
	case nil:
	case string:
		intent = &v
	default:
		return "", false, nil
	}
	if t.WS == "" || t.Slug == "" {
		return errObj("Workspace context is required"), true, nil
	}
	d := collectintent.Decide(query, intent)
	if d.ClarificationRequired {
		out := wbcrud.NewObj()
		out.Set("ok", false)
		dict := d.ToDict()
		for _, k := range dict.Keys() {
			v, _ := dict.Get(k)
			out.Set(k, v)
		}
		out.Set("message", d.Reason)
		s, err := objJSON(out)
		return s, err == nil, err
	}
	jobID := t.newID()
	details, err := pyJSONText(d.ToDict())
	if err != nil {
		return "", false, err
	}
	var queueID int64
	enqueueFailed := false
	err = t.inTx(ctx, func(tx pgx.Tx) error {
		if err := collectledger.Create(ctx, tx, t.WS, jobID, query, d.Intent, details, time.Now().UTC()); err != nil {
			return err
		}
		executor := "python"
		if e.CollectExecutor != nil {
			executor = e.CollectExecutor(t.WS)
		}
		id, err := collectq.Enqueue(ctx, tx, "collect", t.WS, executor, collectPayload{jobID, query, d.Intent, t.WS, t.Slug}, "collect:"+t.WS+":"+jobID)
		if err != nil {
			enqueueFailed = true
			return err
		}
		queueID = id
		return nil
	})
	if enqueueFailed && ctx.Err() == nil {
		// The ledger row and the queue row commit together, so nothing is left behind (Python marked a failed row).
		t.logger().Warn("copilot start_collection enqueue failed", "reason", "database error")
		return errObj("Collection queue unavailable"), true, nil
	}
	if err != nil {
		return "", false, err
	}
	if e.Progress != nil {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		e.Progress.EmitOrdered(ectx, t.WS, "job_created", leadprogress.Obj{
			{Key: "job_id", Val: jobID}, {Key: "query", Val: query}, {Key: "intent", Val: d.Intent},
			{Key: "workspace_id", Val: t.WS}, {Key: "message", Val: "Collection queued: " + query},
		})
		cancel()
	}
	out := wbcrud.NewObj()
	out.Set("ok", true)
	out.Set("job_id", jobID)
	out.Set("query", query)
	out.Set("intent", d.Intent)
	out.Set("queue_job_id", queueID)
	out.Set("message", collectStartedMsg)
	s, err := objJSON(out)
	return s, err == nil, err
}

// ---- create_source_workbook ----

var sourceBaseColumns = []string{
	`{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"}`,
	`{"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website"}`,
	`{"id": "city", "name": "City", "type": "lead_field", "lead_field": "city"}`,
	`{"id": "fit_reasons", "name": "Fit Reasons", "type": "lead_field", "lead_field": "fit_reasons"}`,
	`{"id": "evidence_urls", "name": "Evidence", "type": "lead_field", "lead_field": "evidence_urls"}`,
	`{"id": "field_confidence", "name": "Confidence", "type": "lead_field", "lead_field": "field_confidence"}`,
}

type sourcePayload struct {
	WorkbookID  string `json:"workbook_id"`
	ColumnID    string `json:"column_id"`
	EnrichAfter bool   `json:"enrich_after"`
	WorkspaceID string `json:"workspace_id"`
}

// pyIntArg is int(v) for the argument shapes mirrored here.
func pyIntArg(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strings.TrimFunc(x, isPySpaceRune), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (t *Turn) createSourceWorkbook(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "create_source_workbook"
	raw, present := a["icp_description"]
	if !present {
		return keyErr(name, "icp_description"), true, nil
	}
	icpRaw := ""
	if pyTruthy(raw) {
		icpRaw = pyStr(raw)
	}
	icp := runePrefix(pyClean(icpRaw), 500)
	if icp == "" {
		return errObj("ICP description is required"), true, nil
	}
	accountDiscovery := pyTruthy(a["account_discovery"])
	brief := BuildAccountBrief(icp)
	bget := func(k string) any { v, _ := brief.Get(k); return v }
	if accountDiscovery && bget("complete") != true {
		out := wbcrud.NewObj()
		out.Set("error", "Account discovery brief is incomplete")
		out.Set("missing_fields", bget("missing_fields"))
		out.Set("brief", brief)
		s, err := objJSON(out)
		return s, err == nil, err
	}
	wbName := "Source — " + runePrefix(icp, 40)
	if v := a["name"]; pyTruthy(v) {
		s, ok := v.(string)
		if !ok {
			return "", false, nil
		}
		wbName = s
	}
	targetRows := int64(0)
	if v := a["target_rows"]; pyTruthy(v) {
		n, ok := pyIntArg(v)
		if !ok {
			return "", false, nil
		}
		targetRows = n
	}
	if accountDiscovery {
		targetRows = bget("requested_count").(int64)
	}
	targetRows = min(max(targetRows, 0), 500)
	autoRunV, hasAutoRun := a["auto_run"]
	autoRun := !hasAutoRun || pyTruthy(autoRunV)
	autoEnrich := pyTruthy(a["auto_enrich"])
	key := ""
	if v := a["idempotency_key"]; pyTruthy(v) {
		key = strings.TrimFunc(pyStr(v), isPySpaceRune)
	}
	if runeLen(key) > 255 {
		return errObj("Idempotency key exceeds 255 characters"), true, nil
	}
	if key == "" {
		if accountDiscovery {
			key = "chat-accounts:" + pyStr(bget("brief_id"))
		} else {
			sum := sha256.Sum256([]byte(pyLowerText(icp)))
			key = "chat-source:" + hex.EncodeToString(sum[:])[:24] + ":" + strconv.FormatInt(targetRows, 10)
		}
	}
	for attempt := 0; ; attempt++ {
		out, retry, err := t.sourceWorkbookOnce(ctx, sourceArgs{icp, wbName, targetRows, autoRun, autoEnrich, accountDiscovery, key, brief})
		if retry && attempt == 0 {
			continue // a concurrent create of the same action won the unique index: reuse it
		}
		return out, err == nil, err
	}
}

type sourceArgs struct {
	icp, name                        string
	targetRows                       int64
	autoRun, autoEnrich, accountDisc bool
	key                              string
	brief                            *wbcrud.Obj
}

func runeLen(s string) int { return len([]rune(s)) }

// enqueueSource is queue_service.add_job("source_workbook", ..., fire_key=...): a plain insert on the Python
// queue (executor python, which runs source_workbook until WP-SOURCEWB). It runs in a savepoint so a
// duplicate active fire key (Python's IntegrityError) leaves the caller's transaction usable.
func (t *Turn) enqueueSource(ctx context.Context, tx pgx.Tx, wbID, colID string, enrich bool, key string) (int64, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	id, err := queue.Enqueue(ctx, sp, "source_workbook", t.WS, sourcePayload{wbID, colID, enrich, t.WS}, "source-workbook:"+t.WS+":"+key)
	if err != nil {
		_ = sp.Rollback(ctx)
		return 0, errSourceQueue
	}
	return id, sp.Commit(ctx)
}

var errSourceQueue = errors.New("copilot: source queue unavailable")

type storedWB struct {
	id, name, description string
	totalRows             int64
	config                *wbcrud.Obj
	columns               []json.RawMessage
}

// sourceWorkbookOnce is one pass of the create-or-reuse logic. retry=true means the insert lost a race on the
// (workspace, action key) unique index.
func (t *Turn) sourceWorkbookOnce(ctx context.Context, s sourceArgs) (out string, retry bool, err error) {
	var existing *storedWB
	err = t.inTx(ctx, func(tx pgx.Tx) error {
		var w storedWB
		var desc, cfg, cols *string
		var total *int64
		err := tx.QueryRow(ctx, `SELECT id, name, description, source_config::text, columns_config::text, total_rows
			FROM workbooks WHERE workspace_id = $1 AND action_idempotency_key = $2 LIMIT 1 FOR UPDATE`, t.WS, s.key).
			Scan(&w.id, &w.name, &desc, &cfg, &cols, &total)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		w.description = sval(desc)
		if total != nil {
			w.totalRows = *total
		}
		w.config = wbcrud.NewObj()
		if cfg != nil && *cfg != "null" {
			if p, err := wbcrud.ParseObject([]byte(*cfg)); err == nil {
				w.config = p
			}
		}
		if cols != nil && *cols != "null" {
			_ = json.Unmarshal([]byte(*cols), &w.columns)
		}
		existing = &w
		return t.reuseSourceWorkbook(ctx, tx, existing, s, &out)
	})
	if err != nil || existing != nil {
		return out, false, err
	}
	return t.createNewSourceWorkbook(ctx, s)
}

func (t *Turn) reuseSourceWorkbook(ctx context.Context, tx pgx.Tx, w *storedWB, s sourceArgs, out *string) error {
	cfgGet := func(k string) any { v, _ := w.config.Get(k); return v }
	existingBriefID := any(nil)
	if b, ok := cfgGet("account_discovery_brief").(*wbcrud.Obj); ok {
		existingBriefID, _ = b.Get("brief_id")
	}
	briefID, _ := s.brief.Get("brief_id")
	if (s.accountDisc && existingBriefID != briefID) || (!s.accountDisc && w.description != s.icp) {
		o := wbcrud.NewObj()
		o.Set("error", "Idempotency key conflicts with a different sourcing brief")
		o.Set("action_id", s.key)
		var err error
		*out, err = objJSON(o)
		return err
	}
	if s.autoRun && !pyTruthy(cfgGet("source_job_id")) {
		colID := ""
		for _, c := range w.columns {
			var col struct {
				ID   any `json:"id"`
				Type any `json:"type"`
			}
			if json.Unmarshal(c, &col) == nil && col.Type == "source" {
				colID = pyStr(col.ID)
				break
			}
		}
		if colID == "" {
			o := wbcrud.NewObj()
			o.Set("error", "Persisted workbook has no source column")
			o.Set("workbook_id", w.id)
			var err error
			*out, err = objJSON(o)
			return err
		}
		qid, err := t.enqueueSource(ctx, tx, w.id, colID, s.autoEnrich, s.key)
		if errors.Is(err, errSourceQueue) {
			o := wbcrud.NewObj()
			o.Set("ok", false)
			o.Set("persisted", true)
			o.Set("reused", true)
			o.Set("action_id", s.key)
			o.Set("workbook_id", w.id)
			o.Set("sourcing", false)
			o.Set("error", "Source queue unavailable")
			o.Set("url", "/workbooks/"+w.id)
			*out, err = objJSON(o)
			return err
		}
		if err != nil {
			return err
		}
		w.config.Set("source_job_id", qid)
		w.config.Set("source_queue_status", "queued")
		w.config = objWithout(w.config, "source_queue_error_class")
		cj, err := pyJSONText(w.config)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workbooks SET source_config = $1::json, updated_at = (now() AT TIME ZONE 'utc')
			WHERE id = $2 AND workspace_id = $3`, cj, w.id, t.WS); err != nil {
			return err
		}
	}
	o := wbcrud.NewObj()
	o.Set("ok", true)
	o.Set("persisted", true)
	o.Set("reused", true)
	o.Set("action_id", s.key)
	o.Set("workbook_id", w.id)
	o.Set("name", w.name)
	o.Set("sourcing", pyTruthy(cfgGet("source_job_id")))
	o.Set("source_job_id", cfgGet("source_job_id"))
	o.Set("row_count", w.totalRows)
	if b := cfgGet("account_discovery_brief"); pyTruthy(b) {
		o.Set("brief", b)
	} else {
		o.Set("brief", s.brief)
	}
	o.Set("url", "/workbooks/"+w.id)
	var err error
	*out, err = objJSON(o)
	return err
}

func (t *Turn) createNewSourceWorkbook(ctx context.Context, s sourceArgs) (string, bool, error) {
	geo, _ := s.brief.Get("geographies")
	icpObj := wbcrud.NewObj()
	icpObj.Set("description", s.icp)
	for _, k := range []string{"company_types", "geo", "technologies", "hiring_roles"} {
		src := k
		if k == "geo" {
			src = "geographies"
		}
		v, _ := s.brief.Get(src)
		if k == "geo" {
			v = geo
		}
		icpObj.Set(k, v)
	}
	srcID := "src_" + hex8()
	src := wbcrud.NewObj()
	src.Set("id", srcID)
	src.Set("name", "Source")
	src.Set("type", "source")
	src.Set("icp", icpObj)
	src.Set("channels", wbcrud.NewObj())
	src.Set("target_rows", s.targetRows)
	if s.accountDisc {
		src.Set("account_discovery_brief", s.brief)
	}
	sj, err := pyJSONText(src)
	if err != nil {
		return "", false, err
	}
	columns := "[" + strings.Join(append(append([]string(nil), sourceBaseColumns...), sj), ", ") + "]"
	cfg := wbcrud.NewObj()
	cfg.Set("workspace_id", t.WS)
	briefOut := any(wbcrud.NewObj())
	if s.accountDisc {
		briefOut = s.brief
	}
	cfg.Set("account_discovery_brief", briefOut)
	sourceType := "empty"
	if s.accountDisc {
		sourceType = "account_discovery"
	}
	wbID := uuid.NewString()
	cj, err := pyJSONText(cfg)
	if err != nil {
		return "", false, err
	}
	err = t.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
			filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd,
			refresh_policy, action_idempotency_key, created_at, updated_at)
			VALUES ($1, $2, $3, 'draft', $4, $5, $6::json, '{}'::json, $7::json, 0, 0, true, 0.0, 0.0, '{}'::json, $8,
			        (now() AT TIME ZONE 'utc'), (now() AT TIME ZONE 'utc'))`,
			wbID, s.name, s.icp, t.WS, sourceType, cj, columns, s.key)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	var sourceJob any
	queueFailed := false
	if s.autoRun {
		err = t.inTx(ctx, func(tx pgx.Tx) error {
			qid, err := t.enqueueSource(ctx, tx, wbID, srcID, s.autoEnrich, s.key)
			if err != nil {
				return err
			}
			cfg.Set("source_job_id", qid)
			cfg.Set("source_queue_status", "queued")
			cj, err := pyJSONText(cfg)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE workbooks SET source_config = $1::json WHERE id = $2 AND workspace_id = $3`, cj, wbID, t.WS)
			sourceJob = qid
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			queueFailed = true
			cfg = objWithout(cfg, "source_job_id")
			cfg.Set("source_queue_status", "failed")
			cfg.Set("source_queue_error_class", "queue_unavailable")
			if cj, jerr := pyJSONText(cfg); jerr == nil {
				_ = t.inTx(ctx, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE workbooks SET source_config = $1::json WHERE id = $2 AND workspace_id = $3`, cj, wbID, t.WS)
					return err
				})
			}
		}
	}
	o := wbcrud.NewObj()
	o.Set("ok", !queueFailed)
	o.Set("persisted", true)
	o.Set("reused", false)
	o.Set("action_id", s.key)
	o.Set("workbook_id", wbID)
	o.Set("name", s.name)
	o.Set("sourcing", s.autoRun && !queueFailed)
	o.Set("source_job_id", sourceJob)
	o.Set("row_count", int64(0))
	o.Set("brief", briefOut)
	if queueFailed {
		o.Set("error", "Source queue unavailable")
	}
	o.Set("url", "/workbooks/"+wbID)
	if !queueFailed {
		msg := "Created live-sourcing workbook '" + s.name + "'."
		if s.autoRun {
			msg += " Sourcing started — rows will stream in."
		}
		o.Set("message", msg+" Open at /workbooks/"+wbID)
	}
	out, err := objJSON(o)
	return out, false, err
}
