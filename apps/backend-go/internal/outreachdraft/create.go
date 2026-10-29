package outreachdraft

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Request is create_grounded_draft's input. Source is the saved enrich_people_contacts result (ordered).
type Request struct {
	WorkspaceID, ConversationID, SourceActionID string
	RequestedPersonID                           string
	AllowRisky                                  bool
	IdempotencyKey                              string
	Source                                      *wbcrud.Obj
	// Suppressed reports addresses on the workspace suppression list (nil: none).
	Suppressed func(email string) bool
	// NewID overrides uuid4 for the draft id (tests).
	NewID func() string
}

// outreach_drafts column limits (characters).
var columnLimits = []struct {
	name string
	max  int
}{{"conversation_id", 64}, {"source_action_id", 255}, {"person_id", 80}, {"person_name", 200}, {"company", 200},
	{"title", 240}, {"to_email", 320}, {"subject", 300}}

// headerSafe collapses control characters (CR/LF included) to single spaces, so a scraped company name can
// never split the subject header of a later send.
func headerSafe(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) }), " ")
}

type row struct {
	id, ws, key, personID, personName, company, title, toEmail, status, subject, body, state string
	risky, generic, role                                                                     bool
	sentenceEvidence, snapshot                                                               string
	sentAt                                                                                   *time.Time
}

const selectDraft = `SELECT id, workspace_id, action_idempotency_key, person_id, person_name, company, COALESCE(title, ''),
	to_email, contact_status, risky_approved, generic_inbox, is_role_address, subject, body_text, state,
	sentence_evidence::text, source_snapshot::text, sent_at
	FROM outreach_drafts WHERE workspace_id = $1 AND `

func scanRow(r pgx.Row) (*row, error) {
	var d row
	err := r.Scan(&d.id, &d.ws, &d.key, &d.personID, &d.personName, &d.company, &d.title, &d.toEmail, &d.status,
		&d.risky, &d.generic, &d.role, &d.subject, &d.body, &d.state, &d.sentenceEvidence, &d.snapshot, &d.sentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Receipt is draft_receipt.
func receipt(d *row, readback, reused bool) (*wbcrud.Obj, error) {
	evidence := []any{}
	if d.sentenceEvidence != "" && d.sentenceEvidence != "null" {
		v, err := parseArray(d.sentenceEvidence)
		if err != nil {
			return nil, err
		}
		evidence = v
	}
	personalized := int64(0)
	for _, e := range evidence {
		if o, ok := e.(*wbcrud.Obj); ok {
			if p, _ := o.Get("personalized"); p == true {
				personalized++
			}
		}
	}
	o := wbcrud.NewObj()
	o.Set("ok", true)
	o.Set("persisted", true)
	o.Set("reused", reused)
	o.Set("readback_confirmed", readback)
	o.Set("action_id", d.key)
	o.Set("draft_id", d.id)
	o.Set("workspace_id", d.ws)
	o.Set("state", d.state)
	o.Set("person_id", d.personID)
	o.Set("person_name", d.personName)
	o.Set("company", d.company)
	o.Set("title", d.title)
	o.Set("to_email", d.toEmail)
	o.Set("contact_status", d.status)
	o.Set("risky_approved", d.risky)
	o.Set("generic_inbox", d.generic)
	o.Set("is_role_address", d.role)
	o.Set("subject", d.subject)
	o.Set("body_text", d.body)
	o.Set("sentence_evidence", evidence)
	o.Set("personalized_sentence_count", personalized)
	o.Set("send_performed", d.sentAt != nil || d.state == "sent")
	if d.sentAt != nil {
		o.Set("sent_at", d.sentAt.UTC().Format("2006-01-02T15:04:05.999999-07:00"))
	} else {
		o.Set("sent_at", nil)
	}
	o.Set("url", "/outreach?draft="+d.id)
	return o, nil
}

func parseArray(s string) ([]any, error) {
	o, err := wbcrud.ParseObjectCanon([]byte(`{"v": ` + s + `}`))
	if err != nil {
		return nil, err
	}
	v, _ := o.Get("v")
	l, _ := v.([]any)
	return l, nil
}

func snapshotHash(s string) string {
	o, err := wbcrud.ParseObjectCanon([]byte(s))
	if err != nil {
		return ""
	}
	v, _ := o.Get("contract_hash")
	h, _ := v.(string)
	return h
}

// dumps is json.dumps of an ordered value (what the SQLAlchemy JSON columns store).
func dumps(v any) (string, error) {
	w := wbcrud.NewObj()
	w.Set("v", v)
	b, err := w.MarshalJSON()
	if err != nil {
		return "", err
	}
	r, err := ssestream.Reformat(b[len(`{"v":`) : len(b)-1])
	return string(r), err
}

// Create is create_grounded_draft inside the caller's workspace transaction (tenancy.InWorkspace). A
// refusal is an *Error; any other error is a database failure.
func Create(ctx context.Context, tx pgx.Tx, r Request) (*wbcrud.Obj, error) {
	key := text(r.IdempotencyKey)
	if key == "" || utf8.RuneCountInString(key) > 255 {
		return nil, fail("A valid idempotency key is required", nil)
	}
	sel, derr := SelectBestContact(r.Source, r.RequestedPersonID, r.AllowRisky, r.Suppressed)
	if derr != nil {
		return nil, derr
	}
	c := sel.Selected
	company := text(or(at(r.Source, "company"), at(c.Person, "company")))
	if company == "" {
		return nil, fail("Saved company identity is missing", nil)
	}
	hash := ContractHash(r.SourceActionID, c, r.AllowRisky)
	existing, err := scanRow(tx.QueryRow(ctx, selectDraft+`action_idempotency_key = $2`, r.WorkspaceID, key))
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if snapshotHash(existing.snapshot) != hash {
			return nil, fail("Idempotency key already belongs to a different draft request", nil)
		}
		return receipt(existing, true, true)
	}

	firstName := strings.FieldsFunc(c.Name, isPySpace)[0]
	rolePhrase := c.Title
	if rolePhrase == "" {
		rolePhrase = c.Function
	}
	subject := "Partnership idea for " + headerSafe(company)
	sentences := []string{
		"Hi " + firstName + ",",
		"I came across your work in " + rolePhrase + " at " + company + " while researching partnership teams.",
		"I would like to compare notes on whether OpenGTM's evidence-backed account research and workflow automation could support your team.",
		"Would you be open to a brief conversation next week?",
		"Best,",
	}
	for _, l := range columnLimits {
		v := map[string]string{"conversation_id": r.ConversationID, "source_action_id": r.SourceActionID,
			"person_id": c.PersonID, "person_name": c.Name, "company": company, "title": c.Title, "to_email": c.Email,
			"subject": subject}[l.name]
		if utf8.RuneCountInString(v) > l.max {
			return nil, fail("Saved contact data exceeds the draft limits ("+l.name+")", nil)
		}
	}
	identity, employment, function := claimID(c.PersonID, "person_identity"), claimID(c.PersonID, "current_employment"),
		claimID(c.PersonID, "partnership_function")
	sentence := func(id, txt string, personalized bool, claims []any, evidence []any) *wbcrud.Obj {
		o := wbcrud.NewObj()
		o.Set("sentence_id", id)
		o.Set("text", txt)
		o.Set("personalized", personalized)
		o.Set("claim_ids", claims)
		o.Set("evidence", evidence)
		return o
	}
	se := []any{
		sentence("subject", subject, true, []any{employment}, c.Evidence),
		sentence("s1", sentences[0], true, []any{identity}, c.Evidence),
		sentence("s2", sentences[1], true, []any{identity, employment, function}, c.Evidence),
	}
	for i, s := range sentences[2:] {
		se = append(se, sentence("s"+itoa(i+3), s, false, []any{}, []any{}))
	}
	contact := dict(at(c.Person, "contactability"))
	snap := wbcrud.NewObj()
	snap.Set("contract_hash", hash)
	snap.Set("company", company)
	snap.Set("function", text(at(r.Source, "function")))
	snap.Set("person_id", c.PersonID)
	snap.Set("contact_status", c.Status)
	snap.Set("contact_observed_at", text(at(contact, "observed_at")))
	attempts := list(at(contact, "attempts"))
	if attempts == nil {
		attempts = []any{}
	}
	snap.Set("contact_attempts", attempts)
	snap.Set("public_evidence", c.Evidence)
	snap.Set("contact_flags", sel.Flags)
	seJSON, err := dumps(se)
	if err != nil {
		return nil, err
	}
	snapJSON, err := dumps(snap)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	if r.NewID != nil {
		id = r.NewID()
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	_, err = sp.Exec(ctx, `INSERT INTO outreach_drafts (id, workspace_id, action_idempotency_key, conversation_id,
		source_action_id, person_id, person_name, company, title, to_email, contact_status, risky_approved,
		generic_inbox, is_role_address, subject, body_text, sentence_evidence, source_snapshot, state, sent_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17::json, $18::json, 'draft', NULL)`,
		id, r.WorkspaceID, key, r.ConversationID, r.SourceActionID, c.PersonID, c.Name, company, c.Title, c.Email,
		c.Status, c.Status == "risky" && r.AllowRisky, c.GenericInbox, c.RoleAddress, subject,
		strings.Join(sentences, "\n\n"), seJSON, snapJSON)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// A concurrent retry raced past the first lookup: the unique workspace/action key is authoritative.
		if rerr := sp.Rollback(ctx); rerr != nil {
			return nil, rerr
		}
		raced, err := scanRow(tx.QueryRow(ctx, selectDraft+`action_idempotency_key = $2`, r.WorkspaceID, key))
		if err != nil {
			return nil, err
		}
		if raced == nil {
			return nil, fail("Draft write conflicted and could not be recovered", nil)
		}
		if snapshotHash(raced.snapshot) != hash {
			return nil, fail("Idempotency key already belongs to a different draft request", nil)
		}
		return receipt(raced, true, true)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return nil, err
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, err
	}
	saved, err := scanRow(tx.QueryRow(ctx, selectDraft+`id = $2`, r.WorkspaceID, id))
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, fail("Draft write could not be confirmed", nil)
	}
	return receipt(saved, true, false)
}

func itoa(i int) string { return pyStr(int64(i)) }
