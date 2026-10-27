package copilot

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachdraft"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// The people and grounded-draft branches of services/copilot/preroute.py (plan 15 WP-COPILOT-TOOLS).

var riskyApprovalRE = regexp.MustCompile(`(?i)\b(?:approve|approved|use|allow)\b.{0,24}\brisky\b|\brisky\b.{0,24}\b(?:approve|approved|use|allow)\b`)

// runPeople runs fn while keepalive comments flow (await_with_keepalive).
func (t *Turn) runPeople(p *preWriter, fn func(ctx context.Context) (map[string]any, error)) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(p.ctx, t.toolTimeout())
	defer cancel()
	type out struct {
		m   map[string]any
		err error
	}
	ch := make(chan out, 1)
	go func() {
		m, err := fn(ctx)
		ch <- out{m, err}
	}()
	tick, stop := t.ticker()
	defer stop()
	for {
		select {
		case o := <-ch:
			return o.m, o.err
		case <-tick:
			if err := p.w.Keepalive(); err != nil {
				cancel()
				<-ch
				return nil, err
			}
			p.wrote = true
		case <-p.ctx.Done():
			cancel()
			<-ch
			return nil, p.ctx.Err()
		}
	}
}

// peopleResultTree renders a research / verification result for the frames and decodes it back for the
// renderers (one value shape for both).
func peopleResultTree(res map[string]any) (string, map[string]any, error) {
	s, err := pyDumps(res)
	if err != nil {
		return "", nil, err
	}
	m, _ := decodeObject([]byte(s))
	return s, m, nil
}

// runPeopleTool emits tool_call, runs fn, then tool_result, the rendered content and the two history rows.
func (t *Turn) runPeopleTool(p *preWriter, name string, displayArgs *wbcrud.Obj, fn func(ctx context.Context) (map[string]any, error),
	unavailable func(err error) map[string]any, render func(map[string]any) string) error {
	if err := p.obj("conversation_id", t.ConvID); err != nil {
		return err
	}
	call := wbcrud.NewObj()
	call.Set("name", name)
	call.Set("args", displayArgs)
	if err := p.obj("tool_call", call); err != nil {
		return err
	}
	res, err := t.runPeople(p, fn)
	if err != nil {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		res = unavailable(err)
	}
	raw, m, err := peopleResultTree(res)
	if err != nil {
		return &turnErr{errTextInterrupted, err}
	}
	if err := p.sse([]byte(`{"tool_result": {"name": ` + jstr(name) + `, "result": ` + raw + `}}`)); err != nil {
		return err
	}
	text := render(m)
	if err := p.obj("content", text); err != nil {
		return err
	}
	toolData := `{"name": ` + jstr(name) + `, "result": ` + raw + `}`
	p.persist("tool", name+" result", &toolData)
	p.persist("assistant", text, nil)
	return p.w.Done()
}

func detail(err error) string { return runePrefix(err.Error(), 240) }

func (t *Turn) prePeopleResearch(p *preWriter, req *collectintent.CompanyTeamRequest) error {
	args := wbcrud.NewObj()
	args.Set("company", req.Entity)
	args.Set("function", req.Function)
	args.Set("limit", int64(8))
	return t.runPeopleTool(p, "find_people_at_company", args,
		func(ctx context.Context) (map[string]any, error) {
			return t.Engine.People.Research(ctx, req.Entity, req.Function, leadpeople.ResearchOptions{Limit: 8, Workspace: t.WS})
		},
		func(err error) map[string]any {
			return map[string]any{"ok": false, "error": "people_research_unavailable", "detail": detail(err),
				"company": req.Entity, "function": req.Function, "people": []any{}, "count": int64(0)}
		}, FormatPeopleResearch)
}

func (t *Turn) preVerifyPeople(p *preWriter, source *wbcrud.Obj) error {
	company := or2(oget(source, "company"), "")
	function := or2(oget(source, "function"), "")
	people := or2(oget(source, "people"), []any{})
	n, _ := pyLen(people)
	display := wbcrud.NewObj()
	display.Set("company", company)
	display.Set("function", function)
	display.Set("people_count", int64(n))
	return t.runPeopleTool(p, "verify_people_at_company", display,
		func(ctx context.Context) (map[string]any, error) {
			return t.Engine.People.Verify(ctx, pyStr(company), pyStr(function), plainPeople(people))
		},
		func(err error) map[string]any {
			return map[string]any{"ok": false, "error": "people_verification_unavailable", "detail": detail(err),
				"company": toPlain(company), "function": toPlain(function), "people": []any{}, "count": int64(0)}
		}, FormatPeopleVerification)
}

// prePeopleProposal is the contacts (workbook=false) and people-workbook approval branches.
func (t *Turn) prePeopleProposal(p *preWriter, source *wbcrud.Obj, workbook bool) error {
	company := stripOr(oget(source, "company"), "People")
	function := stripOr(oget(source, "function"), "Research")
	people := dictPeople(oget(source, "people"))
	var ids []string
	seen := map[string]bool{}
	for _, person := range people {
		id := personID(company, person)
		person.Set("person_id", id)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	setID := stripOr(oget(source, "result_set_id"), "")
	if setID == "" {
		setID = resultSetID(company, function, people)
	}
	digest := digest16(ids)
	args := wbcrud.NewObj()
	args.Set("conversation_id", t.ConvID)
	argsMap := map[string]any{"conversation_id": t.ConvID, "person_ids": anyStrings(ids)}
	var name, text string
	if workbook {
		wbName := runePrefix(peopleWorkbookName(company, function), 255)
		args.Set("name", wbName)
		argsMap["name"] = wbName
		name = "create_people_workbook"
		text = "I can create **" + wbName + "** with the exact " + strconv.Itoa(len(ids)) +
			" people already stored in this conversation. This will not run a new lead search."
	} else {
		name = "enrich_people_contacts"
		text = "I can look for work emails for the exact " + strconv.Itoa(len(ids)) + " people saved " +
			"in this conversation, then check each found address with a separate deliverability verifier. " +
			"This may use configured provider credits."
	}
	args.Set("person_ids", anyStrings(ids))
	prefix := "chat-contacts:"
	if workbook {
		prefix = "chat-people:"
	}
	key := prefix + t.ConvID + ":" + setID + ":" + digest
	args.Set("idempotency_key", key)
	argsMap["idempotency_key"] = key
	return p.proposal(text, name, args, argsMap)
}

func (t *Turn) preDraft(p *preWriter, st preState) error {
	_, source, ok := latestOrdered(st.history, "enrich_people_contacts")
	if !ok {
		source = wbcrud.NewObj()
	}
	sourceActionID := stripOr(oget(source, "action_id"), "")
	allowRisky := riskyApprovalRE.MatchString(st.text)
	suppressed := map[string]bool{}
	err := t.inTx(p.ctx, func(tx pgx.Tx) error {
		for _, e := range outreachdraft.Emails(source) {
			s, err := outreachstore.IsSuppressed(p.ctx, tx, t.WS, e)
			if err != nil {
				return err
			}
			suppressed[e] = s
		}
		return nil
	})
	if err != nil {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		return &turnErr{errTextInterrupted, err}
	}
	sel, derr := outreachdraft.SelectBestContact(source, "", allowRisky, func(e string) bool { return suppressed[e] })
	if derr != nil {
		reasonSet := map[string]bool{}
		for _, f := range derr.Flags {
			if o, ok := f.(*wbcrud.Obj); ok {
				if r := pyStr(oget(o, "reason")); pyTruthy(oget(o, "reason")) {
					reasonSet[r] = true
				}
			}
		}
		var reasons []string
		for r := range reasonSet {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		text := "I could not choose an eligible saved contact: " + derr.Msg + ". "
		if len(reasons) > 0 {
			text += "Blocked by: " + strings.Join(reasons, ", ") + ". "
		}
		text += "No draft was created and no message was sent."
		blocked := wbcrud.NewObj()
		blocked.Set("error", derr.Msg)
		blocked.Set("contact_flags", derr.Flags)
		blocked.Set("send_performed", false)
		if err := p.obj("conversation_id", t.ConvID); err != nil {
			return err
		}
		if err := p.obj("content", text); err != nil {
			return err
		}
		if err := p.obj("draft_blocked", blocked); err != nil {
			return err
		}
		p.persist("assistant", text, nil)
		return p.w.Done()
	}
	c := sel.Selected
	key := "chat-draft:" + t.ConvID + ":" + sha16(sourceActionID+"|"+c.PersonID+"|"+c.Email+"|"+pyStr(allowRisky))
	args := wbcrud.NewObj()
	args.Set("conversation_id", t.ConvID)
	args.Set("source_action_id", sourceActionID)
	args.Set("person_id", c.PersonID)
	args.Set("allow_risky", allowRisky)
	args.Set("idempotency_key", key)
	argsMap := map[string]any{"conversation_id": t.ConvID, "source_action_id": sourceActionID, "person_id": c.PersonID,
		"allow_risky": allowRisky, "idempotency_key": key}
	company := "the target company"
	if v, has := source.Get("company"); has {
		company = pyStr(v)
	}
	text := "I can save a short draft for **" + c.Name + "** at **" + company + "** using the saved **" + c.Status +
		"** address and public role evidence. The draft action has no send capability."
	return p.proposal(text, "draft_grounded_outreach", args, argsMap)
}
