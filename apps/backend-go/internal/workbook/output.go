package workbook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook/outurl"
)

// Output columns (workbook/output.py + output_attempts.py): push the row to an external
// destination and record the outcome in the cell. The destination clients are the Go ports of
// services/integrations (plans 07/08/10); this file only maps a column onto them.

// CRMPusher is crmpush.Push bound to its CRM clients.
type CRMPusher interface {
	Push(ctx context.Context, ws string, cfg, leadData map[string]any, workbookID string, leadID int64) crmpush.Result
}

// SheetsAppender is *sheets.Client.
type SheetsAppender interface {
	AppendRow(ctx context.Context, ws, spreadsheetID string, values []any, sheetRange string) (*wbcrud.Obj, error)
}

// AirtablePusher is *airtable.Client.
type AirtablePusher interface {
	PushRecord(ctx context.Context, ws string, fields *wbcrud.Obj, baseID, table string, typecast bool) (*wbcrud.Obj, error)
}

// InstantlyAdder is *instantly.Client.
type InstantlyAdder interface {
	AddLeadToCampaign(ctx context.Context, ws, campaignID string, lead *wbcrud.Obj, skipIfInCampaign bool) (*wbcrud.Obj, error)
}

// SmartleadAdder is *smartlead.Client.
type SmartleadAdder interface {
	AddLeadToCampaign(ctx context.Context, ws, campaignID string, lead, settings *wbcrud.Obj) (*wbcrud.Obj, error)
}

// OutreachEnroller is the outreach store slice the sequencer destination uses
// (PGOutreach over outreachstore in production).
type OutreachEnroller interface {
	SequenceExists(ctx context.Context, ws, seqID string) (bool, error)
	IsSuppressed(ctx context.Context, ws, email string) (bool, error)
	// Enroll reports false when the lead is already enrolled.
	Enroll(ctx context.Context, ws, seqID string, leadID int64, email, consentSource string) (bool, error)
}

// OutputPorts are the destination clients of output columns. A nil port fails that
// destination's cells with "<destination> is not configured on this worker".
type OutputPorts struct {
	CRM       CRMPusher
	Sheets    SheetsAppender
	Airtable  AirtablePusher
	Instantly InstantlyAdder
	Smartlead SmartleadAdder
	Outreach  OutreachEnroller
}

// outResult is the {"success", "value", "error"} dict a destination returns.
type outResult struct {
	Success bool
	Value   string
	HasVal  bool // value is not None
	Error   string
}

func outFail(err string) outResult { return outResult{Error: err, HasVal: true} }

// succeeded is `("success" not in out or out["success"] is True) and not out.get("error")`.
func (o outResult) succeeded() bool { return o.Success && o.Error == "" }

// errOutputCrash is a failure Python raises out of execute_output_column (anything but a
// template ValueError): the cell becomes cell_execution_failed.
var errOutputCrash = errors.New("workbook: output destination crashed")

func crash(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errOutputCrash, fmt.Sprintf(format, a...))
}

// outputCell is the output branch of enrich_cell: execute (journaled for a queued run),
// then map the receipt onto the cell.
func (rs *runState) outputCell(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, error) {
	provider := outputProvider(col)
	out, err := rs.executeOutputJournaled(ctx, lead, col, rowID, leadID)
	var refused *outputRefusal
	if errors.As(err, &refused) {
		// Another delivery owns the cell: answer the code, write nothing (the owner's cell stays).
		return cellOutcome{Err: refused.code}, nil
	}
	if err != nil {
		return cellOutcome{}, err
	}
	r := cellResult{Provider: provider, BroadcastProvider: provider}
	if out.succeeded() {
		r.Has, r.Value = out.Value != "", out.Value
		if !r.Has {
			r.Provider = ""
		}
	} else {
		r.Provider = ""
		r.Err = out.Error
		if r.Err == "" {
			r.Err = "output_failed"
		}
	}
	return rs.commitCell(ctx, col, rowID, leadID, r)
}

// outputProvider is col_config.get("destination", "output").
func outputProvider(col column) string {
	v, ok := col.cfg["destination"]
	if !ok {
		return "output"
	}
	if v == nil {
		return ""
	}
	return manifest.PyStr(v)
}

// outputCellKey is the journal key: row:<row_id>/<col> (lead:<lead_id> for a lead-store row).
func outputCellKey(lead map[string]any, col column, leadID int64) string {
	subject := fmt.Sprintf("lead:%d", leadID)
	if v, ok := lead["__row_id"]; ok && v != nil {
		subject = "row:" + manifest.PyStr(v)
	}
	return subject + "/" + manifest.PyStr(col.cfg["id"])
}

// outputContract is the request dict execute_output_with_journal hashes.
func (rs *runState) outputContract(lead map[string]any, col column, leadID int64) map[string]any {
	return map[string]any{"col_config": col.cfg, "lead_data": rowForColumn(lead), "columns_config": rs.columnConfigs(),
		"workbook_id": rs.p.WorkbookID, "lead_id": leadID, "workspace_id": rs.p.WorkspaceID}
}

// contractHash is sha256(json.dumps(contract, sort_keys=True, separators=(",", ":"))).
func contractHash(contract map[string]any) (string, error) {
	b, err := canonjson.Marshal(canonValue(contract), true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonValue maps decoded row values onto the shapes canonjson accepts.
func canonValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil && !strings.ContainsAny(string(x), ".eE") {
			return i
		}
		f, _ := x.Float64()
		return f
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case colgraph.Col:
		m := make(map[string]any, len(x.Vals))
		for k, e := range x.Vals {
			m[k] = canonValue(e)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = canonValue(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonValue(e)
		}
		return out
	}
	return v
}

// executeOutputJournaled is execute_output_with_journal: a queued run claims the cell in the
// job payload (output_attempts) before contacting the destination and records the receipt
// after; a recorded receipt is reused and a claim without one is never resent. An in-request
// cell run (no lease) sends directly, like Python's direct callers.
func (rs *runState) executeOutputJournaled(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (outResult, error) {
	if rs.claim == nil {
		return rs.executeOutputSingle(ctx, lead, col, rowID, leadID)
	}
	key := outputCellKey(lead, col, leadID)
	digest, err := contractHash(rs.outputContract(lead, col, leadID))
	if err != nil {
		return outResult{}, crash("output contract: %v", err)
	}
	var reuse *outResult
	reconcile := false
	err = rs.withJobPayload(ctx, func(attempts map[string]any) (bool, error) {
		if prior, ok := attempts[key]; ok {
			pm, isMap := prior.(map[string]any)
			if !isMap || pm["contract_hash"] != digest {
				return false, crash("Output contract changed; review required")
			}
			if pm["state"] == "recorded" {
				r := receiptOf(pm["result"])
				reuse = &r
			} else {
				reconcile = true
			}
			return false, nil
		}
		attempts[key] = map[string]any{"contract_hash": digest, "state": "dispatch_claimed"}
		return true, nil
	})
	if err != nil {
		return outResult{}, err
	}
	if reuse != nil {
		return *reuse, nil
	}
	if reconcile {
		return outResult{Error: "output_delivery_requires_review"}, nil
	}
	// No database transaction is held while contacting the destination.
	exec := rs.executeOutput
	if rs.outputExec != nil {
		exec = rs.outputExec
	}
	res, err := exec(ctx, lead, col, leadID)
	if err != nil {
		return outResult{}, err
	}
	receipt := outResult{Success: res.succeeded()}
	if receipt.Success {
		receipt.Value, receipt.HasVal = res.Value, res.HasVal
	}
	receipt.Error = res.Error
	if receipt.Error == "" && !receipt.Success {
		receipt.Error = "output_failed"
	}
	err = rs.withJobPayload(ctx, func(attempts map[string]any) (bool, error) {
		pm, ok := attempts[key].(map[string]any)
		if !ok || pm["contract_hash"] != digest {
			return false, crash("Output claim does not match result")
		}
		rec := receiptJSON(receipt)
		if pm["state"] == "recorded" {
			if prev, _ := json.Marshal(pm["result"]); string(prev) != string(mustJSON(rec)) {
				return false, crash("Output result conflicts with committed receipt")
			}
		}
		attempts[key] = map[string]any{"contract_hash": digest, "state": "recorded", "result": rec}
		return true, nil
	})
	if err != nil {
		return outResult{}, err
	}
	return receipt, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func receiptJSON(r outResult) map[string]any {
	m := map[string]any{"success": r.Success, "value": nil, "error": nil}
	if r.HasVal && r.Success {
		m["value"] = r.Value
	}
	if r.Error != "" {
		m["error"] = r.Error
	}
	return m
}

// receiptOf reads a stored receipt; a malformed one is an unsuccessful output.
func receiptOf(v any) outResult {
	m, ok := v.(map[string]any)
	if !ok {
		return outResult{}
	}
	var r outResult
	r.Success = m["success"] == true
	if s, ok := m["value"].(string); ok {
		r.Value, r.HasVal = s, true
	}
	if e, ok := m["error"].(string); ok {
		r.Error = e
	}
	return r
}

// withJobPayload locks the owned run_workbook job (the lease fence), lets fn edit its
// output_attempts and writes the payload back when fn reports a change.
func (rs *runState) withJobPayload(ctx context.Context, fn func(attempts map[string]any) (bool, error)) error {
	c := rs.claim
	return tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		raw, err := q.LockOwnedRunWorkbookJob(ctx, gen.LockOwnedRunWorkbookJobParams{
			ID: int32(c.ID), WorkspaceID: c.WorkspaceID, WorkerID: c.WorkerID,
			LockedAt: pgtype.Timestamp{Time: c.LockedAt, Valid: true}, LeaseToken: c.LeaseToken,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		payload, err := jsonObject(raw)
		if err != nil {
			return err
		}
		if wb, _ := payload["workbook_id"].(string); wb != rs.p.WorkbookID {
			return ErrLeaseLost
		}
		attempts, _ := payload["output_attempts"].(map[string]any)
		if attempts == nil {
			attempts = map[string]any{}
		}
		changed, err := fn(attempts)
		if err != nil || !changed {
			return err
		}
		payload["output_attempts"] = attempts
		b, err := canonjson.Marshal(canonValue(payload), false)
		if err != nil {
			return err
		}
		return q.SetRunWorkbookJobPayload(ctx, gen.SetRunWorkbookJobPayloadParams{Payload: b, ID: int32(c.ID), WorkspaceID: c.WorkspaceID})
	})
}

// executeOutput is execute_output_column: dispatch by destination; a template error
// (unknown or ambiguous reference) is an explicit failed receipt, nothing was sent.
func (rs *runState) executeOutput(ctx context.Context, lead map[string]any, col column, leadID int64) (outResult, error) {
	dest := strings.ToLower(manifest.PyStr(orDefault(col.cfg["destination"], "")))
	cfg, cfgOrdered := rs.destinationConfig(col)
	row := rowForColumn(lead)
	res, err := rs.dispatchOutput(ctx, dest, cfg, cfgOrdered, row, leadID)
	var te *templateError
	if errors.As(err, &te) {
		return outResult{Error: te.msg}, nil
	}
	return res, err
}

// templateError is a ValueError raised by template resolution.
type templateError struct{ msg string }

func (e *templateError) Error() string { return e.msg }

func orDefault(v, def any) any {
	if !manifest.PyTruthy(v) {
		return def
	}
	return v
}

// destinationConfig is col_config.get("destination_config") or {} as a plain map and with
// its key order (field maps are ordered dicts in Python).
func (rs *runState) destinationConfig(col column) (map[string]any, colgraph.Col) {
	cfg, _ := col.cfg["destination_config"].(map[string]any)
	if !manifest.PyTruthy(col.cfg["destination_config"]) || cfg == nil {
		return map[string]any{}, colgraph.Col{Vals: map[string]any{}}
	}
	ordered := colgraph.Col{Vals: map[string]any{}}
	if raw, ok := col.raw["destination_config"]; ok {
		if v, err := rowtmpl.DecodeOrdered(raw); err == nil {
			if c, ok := v.(colgraph.Col); ok {
				ordered = c
			}
		}
	}
	return cfg, ordered
}

func (rs *runState) colGraph() []colgraph.Col { return rs.allCols }

// resolve is output._resolve: strict template resolution against the row.
func (rs *runState) resolve(tmpl string, row map[string]any) (string, error) {
	s, err := rowtmpl.Resolve(tmpl, row, rs.colGraph())
	if err != nil {
		return "", &templateError{msg: err.Error()}
	}
	return s, nil
}

func (rs *runState) dispatchOutput(ctx context.Context, dest string, cfg map[string]any, ordered colgraph.Col, row map[string]any, leadID int64) (outResult, error) {
	ports := rs.deps.Output
	ws := rs.p.WorkspaceID
	notWired := func() (outResult, error) {
		return outFail(dest + " is not configured on this worker"), nil
	}
	switch dest {
	case "webhook":
		return rs.sendWebhook(ctx, cfg, ordered, row)
	case "crm":
		if ports == nil || ports.CRM == nil {
			return notWired()
		}
		r := ports.CRM.Push(ctx, ws, cfg, row, rs.p.WorkbookID, leadID)
		return outResult{Success: r.Success, Value: r.Value, HasVal: true, Error: r.Error}, nil
	case "sequencer":
		if ports == nil || ports.Outreach == nil {
			return notWired()
		}
		return enrollSequence(ctx, ports.Outreach, cfg, leadID, row, ws), nil
	case "airtable":
		if ports == nil || ports.Airtable == nil {
			return notWired()
		}
		return pushAirtable(ctx, ports.Airtable, cfg, ordered, row, ws)
	case "sheets":
		if ports == nil || ports.Sheets == nil {
			return notWired()
		}
		return pushSheets(ctx, ports.Sheets, cfg, row, ws)
	case "instantly":
		if ports == nil || ports.Instantly == nil {
			return notWired()
		}
		return rs.pushInstantly(ctx, ports.Instantly, cfg, ordered, row, ws)
	case "smartlead":
		if ports == nil || ports.Smartlead == nil {
			return notWired()
		}
		return rs.pushSmartlead(ctx, ports.Smartlead, cfg, ordered, row, ws)
	}
	return outFail(fmt.Sprintf("unknown destination %s", pyRepr(dest))), nil
}

// pyRepr is repr() of a str (single quotes unless the text holds one and no double quote).
func pyRepr(s string) string {
	return colgraph.PyStrRepr(s)
}

// ── webhook ──────────────────────────────────────────────────────────────

const webhookTimeout = 15 * time.Second

func (rs *runState) sendWebhook(ctx context.Context, cfg map[string]any, ordered colgraph.Col, row map[string]any) (outResult, error) {
	rawURL, ok := orDefault(cfg["url"], "").(string)
	if !ok {
		return outResult{}, crash("webhook url is not a string")
	}
	rawURL = manifest.PyStrip(rawURL)
	if rawURL == "" {
		return outFail("webhook url not configured"), nil
	}
	// The scheme and authority are literal in the template and substituted path/query values are
	// percent-encoded: a row value can never pick the destination (plan 15 review M3). Fail closed.
	target, err := outurl.Resolve(rawURL, row, rs.resolve)
	if errors.Is(err, outurl.ErrNotLiteral) || errors.Is(err, outurl.ErrHostChanged) {
		return outFail(err.Error()), nil
	}
	if err != nil {
		return outResult{}, err
	}
	if reason := rs.webhookURLProblem(target); reason != "" {
		return outFail("blocked url: " + reason), nil
	}
	method := strings.ToUpper(manifest.PyStr(orDefault(cfg["method"], "POST")))
	var hkeys []string
	hvals := map[string]string{}
	if h, ok := ordered.Get("headers").(colgraph.Col); ok && manifest.PyTruthy(cfg["headers"]) {
		for _, k := range h.Keys {
			v, err := rs.resolve(rowtmpl.PyStr(h.Vals[k]), row)
			if err != nil {
				return outResult{}, err
			}
			if _, seen := hvals[k]; !seen {
				hkeys = append(hkeys, k)
			}
			hvals[k] = v
		}
	}
	var body []byte
	contentType := ""
	if method == "POST" || method == "PUT" || method == "PATCH" {
		body, contentType, err = rs.webhookBody(cfg, ordered, row)
		if err != nil {
			return outResult{}, err
		}
		if contentType == "text/plain" {
			if _, set := hvals["Content-Type"]; set {
				contentType = ""
			}
		}
	}
	rctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, target, bytes.NewReader(body))
	if err != nil {
		return outFail(truncRunes(err.Error(), maxErrorRunes)), nil
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for _, k := range hkeys {
		req.Header.Set(k, hvals[k])
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := rs.webhookClient()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return outResult{}, context.Cause(ctx)
		}
		return outFail(truncRunes(webhookErrText(err), maxErrorRunes)), nil
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	value := fmt.Sprintf("%s %d", method, resp.StatusCode)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return outResult{Success: true, Value: value, HasVal: true}, nil
	}
	return outResult{Value: value, HasVal: true,
		Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncRunes(validUTF8(text), 160))}, nil
}

func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// webhookErrText strips the request URL Go prepends to transport errors (it may carry a
// resolved row value or a credential in a query string).
func webhookErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// webhookURLProblem is _is_safe_public_url: http(s) only, no credentials, a host, and a
// public address (the egress client re-checks at dial time).
func (rs *runState) webhookURLProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "unparseable url: " + err.Error()
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "only http/https allowed"
	}
	if u.User != nil {
		return "credentials in url not allowed"
	}
	if u.Hostname() == "" {
		return "missing host"
	}
	check := rs.deps.URLCheck
	if check == nil {
		check = func(s string) error { return egress.CheckURL(s, true) }
	}
	if err := check(raw); err != nil {
		return err.Error()
	}
	return ""
}

// webhookClient is the egress-guarded client with redirects disabled (follow_redirects=False).
func (rs *runState) webhookClient() *http.Client {
	base := rs.deps.Client
	if base == nil {
		base = http.DefaultClient
	}
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// webhookBody ports the body rules of _send_webhook: none -> the row as JSON; a dict/list
// (or a string that parses as one) resolves structurally; any other string is resolved and
// sent as JSON when it parses, else as text/plain.
func (rs *runState) webhookBody(cfg map[string]any, ordered colgraph.Col, row map[string]any) ([]byte, string, error) {
	const jsonCT = "application/json"
	raw, present := ordered.Vals["body"]
	if !present {
		raw = nil
	}
	if s, ok := raw.(string); ok {
		if v, err := rowtmpl.DecodeOrdered([]byte(s)); err == nil {
			switch v.(type) {
			case colgraph.Col, []any:
				raw = v
			}
		}
	}
	switch t := raw.(type) {
	case nil:
		b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(row))
		return b, jsonCT, err
	case colgraph.Col, []any:
		v, err := rowtmpl.ResolveDeep(t, row, rs.colGraph())
		if err != nil {
			return nil, "", &templateError{msg: err.Error()}
		}
		b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(v))
		return b, jsonCT, err
	}
	resolved, err := rs.resolve(rowtmpl.PyStr(raw), row)
	if err != nil {
		return nil, "", err
	}
	if v, derr := rowtmpl.DecodeOrdered([]byte(resolved)); derr == nil {
		if v == nil {
			return nil, "", nil // json=None: no body
		}
		b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(v))
		return b, jsonCT, err
	}
	return []byte(resolved), "text/plain", nil
}

// ── row destinations ─────────────────────────────────────────────────────

func objStr(o *wbcrud.Obj, key, def string) string {
	if o == nil {
		return def
	}
	v, ok := o.Get(key)
	if !ok {
		return def
	}
	return rowtmpl.PyStr(v)
}

func objTrue(o *wbcrud.Obj, key string) bool {
	if o == nil {
		return false
	}
	v, _ := o.Get(key)
	return manifest.PyTruthy(v)
}

func clientResult(res *wbcrud.Obj, err error, okValue func(*wbcrud.Obj) string, failDefault string) (outResult, error) {
	if err != nil {
		return outResult{}, crash("%v", err)
	}
	if objTrue(res, "success") {
		return outResult{Success: true, Value: okValue(res), HasVal: true}, nil
	}
	return outFail(objStr(res, "error", failDefault)), nil
}

// configStr is cfg.get(key, def) rendered as the str the client receives.
func configStr(cfg map[string]any, key, def string) string {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	return rowtmpl.PyStr(v)
}

func pushSheets(ctx context.Context, c SheetsAppender, cfg map[string]any, row map[string]any, ws string) (outResult, error) {
	columns := []any{"company", "email", "phone", "website", "city"}
	if l, ok := cfg["columns"].([]any); ok && len(l) > 0 {
		columns = l
	}
	values := make([]any, len(columns))
	for i, c := range columns {
		v, ok := row[rowtmpl.PyStr(c)]
		if !ok {
			v = ""
		}
		values[i] = v
	}
	res, err := c.AppendRow(ctx, ws, configStr(cfg, "spreadsheet_id", ""), values, configStr(cfg, "range", "Sheet1"))
	return clientResult(res, err, func(o *wbcrud.Obj) string { return "Sheets: " + objStr(o, "range", "appended") }, "sheets append failed")
}

var airtableDefaultMap = [][2]string{{"company", "Company"}, {"email", "Email"}, {"phone", "Phone"}, {"website", "Website"}, {"city", "City"}}

func pushAirtable(ctx context.Context, c AirtablePusher, cfg map[string]any, ordered colgraph.Col, row map[string]any, ws string) (outResult, error) {
	fields := wbcrud.NewObj()
	if fm, ok := ordered.Get("field_map").(colgraph.Col); ok && manifest.PyTruthy(cfg["field_map"]) {
		for _, lf := range fm.Keys {
			fields.Set(rowtmpl.PyStr(fm.Vals[lf]), row[lf])
		}
	} else {
		for _, p := range airtableDefaultMap {
			fields.Set(p[1], row[p[0]])
		}
	}
	res, err := c.PushRecord(ctx, ws, fields, configStr(cfg, "base_id", ""), configStr(cfg, "table", ""), true)
	return clientResult(res, err, func(o *wbcrud.Obj) string { return "Airtable: " + objStr(o, "record_id", "created") }, "airtable push failed")
}

var sequencerDefaultMap = [][2]string{{"email", "email"}, {"first_name", "first_name"}, {"last_name", "last_name"}, {"company", "company_name"}}

// mapLeadFields is _map_lead_fields: each workbook-column key resolves through the webhook
// resolver (a key may itself be a template); empty values and unknown columns are dropped.
func (rs *runState) mapLeadFields(cfg map[string]any, ordered colgraph.Col, row map[string]any) (*wbcrud.Obj, error) {
	pairs := sequencerDefaultMap
	if fm, ok := ordered.Get("field_map").(colgraph.Col); ok && manifest.PyTruthy(cfg["field_map"]) {
		pairs = nil
		for _, k := range fm.Keys {
			pairs = append(pairs, [2]string{k, rowtmpl.PyStr(fm.Vals[k])})
		}
	}
	out := wbcrud.NewObj()
	for _, p := range pairs {
		tmpl := p[0]
		if !strings.Contains(tmpl, "{") {
			tmpl = "{" + tmpl + "}"
		}
		val, err := rs.resolve(tmpl, row)
		if err != nil {
			if strings.HasPrefix(err.Error(), "Unknown column reference") {
				continue
			}
			return nil, err
		}
		if val == "" {
			continue
		}
		out.Set(p[1], val)
	}
	return out, nil
}

func campaignNote(o *wbcrud.Obj) string {
	if objTrue(o, "duplicate") {
		return "already in campaign"
	}
	return "added"
}

func (rs *runState) pushInstantly(ctx context.Context, c InstantlyAdder, cfg map[string]any, ordered colgraph.Col, row map[string]any, ws string) (outResult, error) {
	campaign := manifest.PyStrip(rowtmpl.PyStr(orDefault(cfg["campaign_id"], orDefault(cfg["campaign"], ""))))
	if campaign == "" {
		return outFail("Instantly campaign_id not configured"), nil
	}
	lead, err := rs.mapLeadFields(cfg, ordered, row)
	if err != nil {
		return outResult{}, err
	}
	skip := true
	if v, ok := cfg["skip_if_in_campaign"]; ok {
		skip = manifest.PyTruthy(v)
	}
	res, cerr := c.AddLeadToCampaign(ctx, ws, campaign, lead, skip)
	return clientResult(res, cerr, func(o *wbcrud.Obj) string { return "Instantly: " + campaignNote(o) }, "instantly push failed")
}

func (rs *runState) pushSmartlead(ctx context.Context, c SmartleadAdder, cfg map[string]any, ordered colgraph.Col, row map[string]any, ws string) (outResult, error) {
	campaign := manifest.PyStrip(rowtmpl.PyStr(orDefault(cfg["campaign_id"], "")))
	if campaign == "" {
		return outFail("Smartlead campaign_id not configured"), nil
	}
	lead, err := rs.mapLeadFields(cfg, ordered, row)
	if err != nil {
		return outResult{}, err
	}
	var settings *wbcrud.Obj
	if s, ok := ordered.Get("settings").(colgraph.Col); ok {
		settings = wbcrud.NewObj()
		for _, k := range s.Keys {
			settings.Set(k, s.Vals[k])
		}
	}
	res, cerr := c.AddLeadToCampaign(ctx, ws, campaign, lead, settings)
	return clientResult(res, cerr, func(o *wbcrud.Obj) string { return "Smartlead: " + campaignNote(o) }, "smartlead push failed")
}

// enrollSequence is _enroll_sequence. Any store failure is the cell's error text.
func enrollSequence(ctx context.Context, store OutreachEnroller, cfg map[string]any, leadID int64, row map[string]any, ws string) outResult {
	seq := cfg["sequence_id"]
	if !manifest.PyTruthy(seq) {
		return outFail("sequence_id not configured")
	}
	if ws == "" {
		return outFail("workspace_id required for sequencer")
	}
	email := emailnorm.Normalize(rowtmpl.PyStr(orDefault(row["email"], "")))
	if v, ok := row["email"]; !ok || v == nil {
		email = ""
	}
	if email == "" {
		return outFail("no_email")
	}
	seqID := rowtmpl.PyStr(seq)
	fail := func(err error) outResult { return outFail(truncRunes(err.Error(), maxErrorRunes)) }
	exists, err := store.SequenceExists(ctx, ws, seqID)
	if err != nil {
		return fail(err)
	}
	if !exists {
		return outFail("sequence not found in workspace")
	}
	suppressed, err := store.IsSuppressed(ctx, ws, email)
	if err != nil {
		return fail(err)
	}
	if suppressed {
		return outResult{Value: "suppressed", HasVal: true}
	}
	ok, err := store.Enroll(ctx, ws, seqID, leadID, email, "workbook_output")
	if err != nil {
		return fail(err)
	}
	if ok {
		return outResult{Success: true, Value: "enrolled", HasVal: true}
	}
	return outResult{Value: "already enrolled", HasVal: true}
}

// PGOutreach is the OutreachEnroller over the outreach store tables.
type PGOutreach struct{ Pool *pgxpool.Pool }

func (p PGOutreach) in(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
	return tenancy.InWorkspace(ctx, p.Pool, ws, fn)
}

func (p PGOutreach) SequenceExists(ctx context.Context, ws, seqID string) (ok bool, err error) {
	err = p.in(ctx, ws, func(tx pgx.Tx) error {
		ok, err = outreachstore.SequenceExists(ctx, tx, ws, seqID)
		return err
	})
	return ok, err
}

func (p PGOutreach) IsSuppressed(ctx context.Context, ws, email string) (ok bool, err error) {
	err = p.in(ctx, ws, func(tx pgx.Tx) error {
		ok, err = outreachstore.IsSuppressed(ctx, tx, ws, email)
		return err
	})
	return ok, err
}

func (p PGOutreach) Enroll(ctx context.Context, ws, seqID string, leadID int64, email, consentSource string) (created bool, err error) {
	err = p.in(ctx, ws, func(tx pgx.Tx) error {
		_, created, err = outreachstore.Enroll(ctx, tx, ws, seqID, int(leadID), email, consentSource, time.Time{})
		return err
	})
	return created, err
}
