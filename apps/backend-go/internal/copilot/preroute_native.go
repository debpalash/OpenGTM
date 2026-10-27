package copilot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports services/copilot/preroute.py under GO_COPILOT_NATIVE. The branches are evaluated in
// Python's order. The company-team clarification, signal tracking and account discovery answer in Go; the
// people research / verification / contacts / workbook and grounded-draft branches belong to tools that are
// still Python (WP-COPILOT-TOOLS), so when one of them is the first match the whole pre-route is delegated to
// the worker, which re-evaluates the same order and answers it.

// preDecision names the first matching branch.
type preDecision int

const (
	preNone preDecision = iota
	preClarify
	preDelegate
	preTrack
	preAccounts
)

// Python's follow-up regexes (fullmatch / search, IGNORECASE; "." does not cross newlines).
var (
	verifyFollowupRE = regexp.MustCompile(`(?i)\A(?:please\s+)?(?:ver[i\x{130}\x{131}]fy|rever[i\x{130}\x{131}]fy|re-ver[i\x{130}\x{131}]fy|val[i\x{130}\x{131}]date|check|conf[i\x{130}\x{131}]rm)` +
		`(?:\s+(?:them|these|those|the\s+(?:people|results|cand[i\x{130}\x{131}]dates)))?[?.!]*\z`)
	contactFollowupRE = regexp.MustCompile(`(?i)\A(?:please\s+)?(?:f[i\x{130}\x{131}]nd|get|d[i\x{130}\x{131}]scover|enr[i\x{130}\x{131}]ch|look\s+up|ver[i\x{130}\x{131}]fy)\b.*` +
		`\b(?:work\s+)?(?:e-?ma[i\x{130}\x{131}]ls?|ema[i\x{130}\x{131}]l\s+addresses?|contact\s+[i\x{130}\x{131}]nfo(?:rmat[i\x{130}\x{131}]on)?)\b.*\z`)
	workbookFollowupRE = regexp.MustCompile(`(?i)\A(?:please\s+)?(?:make|create|bu[i\x{130}\x{131}]ld|save|add|[i\x{130}\x{131}]mport|put)\b.*\bworkbook\b.*\z|` +
		`\A(?:please\s+)?(?:make|create|bu[i\x{130}\x{131}]ld)\s+(?:a\s+)?workbook\s+w[i\x{130}\x{131}]th\s+` +
		`(?:them|these|those|the\s+(?:people|results|cand[i\x{130}\x{131}]dates))[?.!]*\z`)
	groundedDraftRE = regexp.MustCompile(`(?i)\bdraft\b.*\b(?:partnersh[i\x{130}\x{131}]p\s+)?(?:e-?ma[i\x{130}\x{131}]l|message)\b.*\b(?:ver[i\x{130}\x{131}]f[i\x{130}\x{131}]ed\s+)?contact\b`)
	draftNoSendRE   = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|dont|never)\s+send\b`)
)

// newCallHex is uuid4().hex.
func newCallHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// preState is what the decision needs from the conversation.
type preState struct {
	decision collectintent.Decision
	tracking *TrackingRequest
	brief    *wbcrud.Obj
	history  []chathistory.Message
}

// decidePreRoute evaluates the branches in Python's order.
func (t *Turn) decidePreRoute(ctx context.Context, msg string, approved bool) (preDecision, preState, error) {
	var st preState
	if approved {
		return preNone, st, nil // every branch requires "not approved"
	}
	if msg != "" {
		st.decision = collectintent.Decide(msg, nil)
		if st.decision.ClarificationKind == "company_team" {
			return preClarify, st, nil
		}
	}
	if collectintent.ExtractExplicitPeopleRequest(msg) != nil {
		return preDelegate, st, nil
	}
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		st.history, err = chathistory.Messages(ctx, tx, t.ConvID, t.WS, t.UID)
		return err
	})
	if err != nil {
		return preNone, st, err
	}
	stripped := strings.TrimFunc(msg, isPySpaceRune)
	if _, _, prior := LatestToolResult(st.history, "verify_people_at_company", "find_people_at_company"); prior {
		if contactFollowupRE.MatchString(stripped) || verifyFollowupRE.MatchString(stripped) || workbookFollowupRE.MatchString(stripped) {
			return preDelegate, st, nil
		}
	}
	if groundedDraftRE.MatchString(stripped) && draftNoSendRE.MatchString(stripped) {
		return preDelegate, st, nil
	}
	if st.tracking = ExtractTrackingRequest(msg); st.tracking != nil {
		return preTrack, st, nil
	}
	if st.brief = ExtractAccountDiscovery(msg); st.brief != nil {
		return preAccounts, st, nil
	}
	return preNone, st, nil
}

// preWriter emits one branch's frames: data events through t.emit (Python json.dumps bytes), history rows
// through storeMessage, and [DONE].
type preWriter struct {
	t     *Turn
	ctx   context.Context
	w     *ssestream.Writer
	wrote bool
}

func (p *preWriter) sse(raw []byte) error {
	if err := p.t.emit(p.w, raw); err != nil {
		return err
	}
	p.wrote = true
	return nil
}

func (p *preWriter) obj(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return &turnErr{errTextInterrupted, err}
	}
	return p.sse([]byte(`{` + jstr(key) + `: ` + string(b) + `}`))
}

func (p *preWriter) persist(role, content string, toolData *string) {
	p.t.storeMessage(p.ctx, role, content, toolData)
	p.wrote = true
}

// proposal emits the conversation_id, the proposal text, the confirmation card and awaiting_confirmation, then
// persists the proposal and ends the stream: the shape shared by every approval branch.
func (p *preWriter) proposal(text, name string, fnArgs *wbcrud.Obj, argsMap map[string]any) error {
	t := p.t
	argsJSON, err := json.Marshal(fnArgs)
	if err != nil {
		return &turnErr{errTextInterrupted, err}
	}
	argStr, err := ssestream.Reformat(argsJSON) // json.dumps(fn_args)
	if err != nil {
		return &turnErr{errTextInterrupted, err}
	}
	callID := "call_" + t.newID()
	toolCall := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": string(argStr)}}
	var approvalID string
	err = t.inTx(p.ctx, func(tx pgx.Tx) error {
		var err error
		approvalID, err = chathistory.CreateApproval(p.ctx, tx, t.WS, t.UID, toolCall)
		return err
	})
	if err != nil {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		return &turnErr{errTextApproval, err}
	}
	desc, ok := DescribeAction(name, argsMap)
	if !ok {
		desc = t.describeFallback(name)
	}
	level, label := Meta(name)
	display := wbcrud.NewObj()
	display.Set("id", approvalID)
	display.Set("type", "function")
	fn := wbcrud.NewObj()
	fn.Set("name", name)
	fn.Set("arguments", string(argStr))
	display.Set("function", fn)
	card := wbcrud.NewObj()
	card.Set("confirmation_id", approvalID)
	card.Set("tool_call", display)
	card.Set("name", name)
	card.Set("args", fnArgs)
	card.Set("description", desc)
	card.Set("level", level)
	card.Set("label", label)
	if err := p.obj("conversation_id", t.ConvID); err != nil {
		return err
	}
	if err := p.obj("content", text); err != nil {
		return err
	}
	if err := p.obj("confirmation_required", card); err != nil {
		return err
	}
	if err := p.sse([]byte(`{"awaiting_confirmation": true}`)); err != nil {
		return err
	}
	p.persist("assistant", text, nil)
	return p.w.Done()
}

// notice emits conversation_id and one content message, persists it and ends the stream.
func (p *preWriter) notice(text string) error {
	if err := p.obj("conversation_id", p.t.ConvID); err != nil {
		return err
	}
	if err := p.obj("content", text); err != nil {
		return err
	}
	p.persist("assistant", text, nil)
	return p.w.Done()
}

func (t *Turn) newID() string {
	if t.Engine != nil && t.Engine.NewID != nil {
		return t.Engine.NewID()
	}
	return newCallHex()
}

func sha16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// nativePreRoute answers the Go branches. delegate=true means "ask the worker" (nothing was written).
func (t *Turn) nativePreRoute(ctx context.Context, w *ssestream.Writer, msg string, approved bool) (handled, delegate bool, err error) {
	dec, st, err := t.decidePreRoute(ctx, msg, approved)
	if err != nil {
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		if t.OpenFault != nil && t.OpenFault() {
			return false, false, errors.Join(ErrPreRouteOpen, err)
		}
		return true, false, t.preFail(w, &turnErr{errTextInterrupted, err})
	}
	p := &preWriter{t: t, ctx: ctx, w: w}
	switch dec {
	case preNone:
		return false, false, nil
	case preDelegate:
		return false, true, nil
	case preClarify:
		err = t.preClarify(p, st.decision)
	case preTrack:
		err = t.preTrack(p, st)
	case preAccounts:
		err = t.preAccounts(p, st.brief)
	}
	if err != nil {
		var te *turnErr
		if errors.As(err, &te) {
			if !p.wrote && t.OpenFault != nil && t.OpenFault() {
				return false, false, errors.Join(ErrPreRouteOpen, te.err)
			}
			return true, false, t.preFail(w, te)
		}
		return true, false, err
	}
	return true, false, nil
}

func (t *Turn) preClarify(p *preWriter, d collectintent.Decision) error {
	// Python renders f"**{entity} {query[len(entity):].strip()}**", which assumes the query starts with the
	// entity ("find stripe.com sales leaders" became "**stripe.com e.com sales leaders**"); Go shows the query.
	head := "**" + d.Query + "**"
	if strings.HasPrefix(d.Query, d.Entity) {
		head = "**" + d.Entity + " " + strings.TrimFunc(d.Query[len(d.Entity):], isPySpaceRune) + "**"
	}
	text := head + " can mean three different GTM jobs. Choose the result you want below. I haven't queued a collection task."
	payload := wbcrud.NewObj()
	payload.Set("ok", false)
	dict := d.ToDict()
	for _, k := range dict.Keys() {
		v, _ := dict.Get(k)
		payload.Set(k, v)
	}
	payload.Set("message", d.Reason)
	if err := p.obj("conversation_id", t.ConvID); err != nil {
		return err
	}
	if err := p.obj("content", text); err != nil {
		return err
	}
	if err := p.obj("intent_clarification", payload); err != nil {
		return err
	}
	p.persist("assistant", text, nil)
	return p.w.Done()
}

// workbookAccountIDs is tracking.workbook_account_ids: the stable account ids persisted in the workbook (row
// data account_id, else canonical_entity_id), in row order, de-duplicated; empty for a foreign workbook.
func workbookAccountIDs(ctx context.Context, tx pgx.Tx, ws, wbID string) ([]string, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM workbooks WHERE id = $1 AND workspace_id = $2`, wbID, ws).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT data::text, canonical_entity_id FROM workbook_rows
		WHERE workbook_id = $1 AND workspace_id = $2 ORDER BY position ASC, id ASC`, wbID, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	seen := map[string]bool{}
	for rows.Next() {
		var data, canon *string
		if err := rows.Scan(&data, &canon); err != nil {
			return nil, err
		}
		var v any
		if data != nil {
			if d, err := canonjson.Decode([]byte(*data)); err == nil {
				if m, ok := d.(map[string]any); ok {
					v = m["account_id"]
				}
			}
		}
		if !pyTruthy(v) && canon != nil {
			v = *canon
		}
		id := ""
		if v != nil {
			id = strings.TrimFunc(pyStr(v), isPySpaceRune)
		}
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

func (t *Turn) preTrack(p *preWriter, st preState) error {
	e := t.Engine
	switch {
	case !e.IntentPollerEnabled:
		return p.notice("Intent signal tracking is disabled in this deployment. I have not created a schedule. Enable Intent Watches and its Postgres dependency, then retry.")
	case !e.PGLeadStore:
		return p.notice("Intent signal tracking requires the Postgres lead store. I have not created a schedule. Enable Intent Watches and its Postgres dependency, then retry.")
	}
	wbID := ""
	if _, res, ok := LatestToolResult(st.history, "create_source_workbook"); ok && pyTruthy(res["workbook_id"]) {
		wbID = pyStr(res["workbook_id"])
	}
	var accounts []string
	if wbID != "" {
		err := t.inTx(p.ctx, func(tx pgx.Tx) error {
			var err error
			accounts, err = workbookAccountIDs(p.ctx, tx, t.WS, wbID)
			return err
		})
		if err != nil {
			if p.ctx.Err() != nil {
				return p.ctx.Err()
			}
			return &turnErr{errTextInterrupted, err}
		}
	}
	if wbID == "" || len(accounts) == 0 {
		return p.notice("I cannot bind **these accounts** to a saved account selection yet. Create or finish sourcing an account workbook in this conversation, then ask me to track it. I have not created a schedule.")
	}
	sorted := append([]string(nil), accounts...)
	sort.Strings(sorted)
	tr := st.tracking
	ids := make([]any, len(accounts))
	for i, a := range accounts {
		ids[i] = a
	}
	sigs := make([]any, len(tr.SignalTypes))
	for i, s := range tr.SignalTypes {
		sigs[i] = s
	}
	key := "chat-signals:" + t.ConvID + ":" + sha16(strings.Join(sorted, "|"))
	args := wbcrud.NewObj()
	args.Set("workbook_id", wbID)
	args.Set("account_ids", ids)
	args.Set("cadence", tr.Cadence)
	args.Set("signal_types", sigs)
	args.Set("idempotency_key", key)
	argsMap := map[string]any{"workbook_id": wbID, "account_ids": ids, "cadence": tr.Cadence, "signal_types": sigs, "idempotency_key": key}
	text := "I can create one **" + tr.Cadence + "** schedule for the exact **" + strconv.Itoa(len(accounts)) +
		" accounts** saved in the workbook, covering " + strings.Join(tr.SignalTypes, ", ") +
		". Repeating this request will update or return the same schedule."
	return p.proposal(text, "track_account_signals", args, argsMap)
}

func joinAny(v any) string {
	list, _ := v.([]any)
	parts := make([]string, 0, len(list))
	for _, x := range list {
		parts = append(parts, pyStr(x))
	}
	return strings.Join(parts, ", ")
}

func (t *Turn) preAccounts(p *preWriter, brief *wbcrud.Obj) error {
	get := func(k string) any { v, _ := brief.Get(k); return v }
	count := get("requested_count").(int64)
	companyType, geography := joinAny(get("company_types")), joinAny(get("geographies"))
	parts := []string{strconv.FormatInt(count, 10), companyType, "Accounts"}
	if companyType == "" {
		parts[1] = "Target"
	}
	if geography != "" {
		parts = append(parts, "in", geography)
	}
	name := runePrefix(strings.Join(parts, " "), 255)
	key := "chat-accounts:" + t.ConvID + ":" + pyStr(get("brief_id"))
	icp := pyStr(get("original_query"))
	args := wbcrud.NewObj()
	args.Set("icp_description", icp)
	args.Set("name", name)
	args.Set("target_rows", count)
	args.Set("auto_run", true)
	args.Set("auto_enrich", false)
	args.Set("account_discovery", true)
	args.Set("idempotency_key", key)
	argsMap := map[string]any{"icp_description": icp, "name": name, "target_rows": count, "auto_run": true,
		"auto_enrich": false, "account_discovery": true, "idempotency_key": key}
	filters := []string{"company type " + companyType}
	if geography != "" {
		filters = append(filters, "geography "+geography)
	}
	if v := get("technologies"); pyTruthy(v) {
		filters = append(filters, "technology "+joinAny(v))
	}
	if v := get("hiring_roles"); pyTruthy(v) {
		filters = append(filters, "hiring "+joinAny(v))
	}
	text := "I parsed this as " + strconv.FormatInt(count, 10) + " accounts with " + strings.Join(filters, ", ") +
		". I can create and run an evidence-gated sourcing workbook. Rows must prove every requested filter; any shortfall will be reported as partial."
	return p.proposal(text, "create_source_workbook", args, argsMap)
}
