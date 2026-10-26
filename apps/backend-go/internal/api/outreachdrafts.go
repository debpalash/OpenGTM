package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const draftNotFound = "Draft not found"

type draftRoutes struct{ seqRoutes }

// registerOutreachDrafts registers the 2 grounded draft read routes (Task 24). Any workspace member may read
// (current_workspace); there is no write route.
func registerOutreachDrafts(r *httpx.Router, d Deps) {
	h := draftRoutes{seqRoutes{d: d}}
	read := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	r.Handle("GET /api/outreach/drafts", http.HandlerFunc(h.list), read...)
	r.Handle("GET /api/outreach/drafts/{draft_id}", http.HandlerFunc(h.get), read...)
}

// draftCore is _draft_to_api without the evidence; field order is the Python dict's insertion order.
type draftCore struct {
	ID            string  `json:"id"`
	PersonID      string  `json:"person_id"`
	PersonName    string  `json:"person_name"`
	Company       string  `json:"company"`
	Title         string  `json:"title"`
	ToEmail       string  `json:"to_email"`
	ContactStatus string  `json:"contact_status"`
	RiskyApproved bool    `json:"risky_approved"`
	GenericInbox  bool    `json:"generic_inbox"`
	IsRoleAddress bool    `json:"is_role_address"`
	Subject       string  `json:"subject"`
	BodyText      string  `json:"body_text"`
	State         string  `json:"state"`
	SentAt        *string `json:"sent_at"`
	SendPerformed bool    `json:"send_performed"`
	CreatedAt     *string `json:"created_at"`
	UpdatedAt     *string `json:"updated_at"`
}

type draftFull struct {
	draftCore
	SentenceEvidence json.RawMessage `json:"sentence_evidence"`
	SourceSnapshot   json.RawMessage `json:"source_snapshot"`
}

func newDraftCore(id, personID, personName, company string, title pgtype.Text, toEmail, contactStatus string, risky, generic, role bool,
	subject, body, state string, sent, created, updated pgtype.Timestamptz) draftCore {
	return draftCore{ID: id, PersonID: personID, PersonName: personName, Company: company, Title: title.String, ToEmail: toEmail,
		ContactStatus: contactStatus, RiskyApproved: risky, GenericInbox: generic, IsRoleAddress: role, Subject: subject, BodyText: body,
		State: state, SentAt: pyISOTz(sent), SendPerformed: sent.Valid || state == "sent", CreatedAt: pyISOTz(created), UpdatedAt: pyISOTz(updated)}
}

// pyISOTz is datetime.isoformat() of an aware UTC datetime: microseconds only when non-zero, then "+00:00".
func pyISOTz(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	layout := "2006-01-02T15:04:05"
	if u.Nanosecond()/1000 != 0 {
		layout += ".000000"
	}
	s := u.Truncate(time.Microsecond).Format(layout) + "+00:00"
	return &s
}

func (h draftRoutes) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	limit, offset, errs := draftPaging(r.URL.Query())
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	var rows []gen.OutreachDraftListRow
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		rows, e = gen.New(tx).OutreachDraftList(r.Context(), gen.OutreachDraftListParams{WorkspaceID: ws, RowLimit: limit, RowOffset: offset})
		return e
	})
	if err != nil {
		h.fail(w, "drafts list", err)
		return
	}
	out := make([]draftCore, 0, len(rows))
	for _, x := range rows {
		out = append(out, newDraftCore(x.ID, x.PersonID, x.PersonName, x.Company, x.Title, x.ToEmail, x.ContactStatus, x.RiskyApproved,
			x.GenericInbox, x.IsRoleAddress, x.Subject, x.BodyText, x.State, x.SentAt, x.CreatedAt, x.UpdatedAt))
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Drafts []draftCore `json:"drafts"`
		Limit  int64       `json:"limit"`
		Offset int64       `json:"offset"`
	}{out, limit, offset})
}

// draftPaging validates limit (default 50, 1..200) then offset (default 0, >= 0), like FastAPI Query(ge, le).
func draftPaging(q url.Values) (limit, offset int64, errs []httpx.FieldError) {
	def50, def0, one, max, zero := int64(50), int64(0), int64(1), int64(200), int64(0)
	var fe *httpx.FieldError
	if limit, fe = wbcrud.QueryInt(q, "limit", &def50, &one, &max); fe != nil {
		errs = append(errs, *fe)
	}
	if offset, fe = wbcrud.QueryInt(q, "offset", &def0, &zero, nil); fe != nil {
		errs = append(errs, *fe)
	}
	return limit, offset, errs
}

func (h draftRoutes) get(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	id := r.PathValue("draft_id")
	var row gen.OutreachDraftGetRow
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) (e error) {
		row, e = gen.New(tx).OutreachDraftGet(r.Context(), gen.OutreachDraftGetParams{WorkspaceID: ws, ID: id})
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, draftNotFound)
		return
	}
	if err != nil {
		h.fail(w, "draft get", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, draftFull{
		draftCore: newDraftCore(row.ID, row.PersonID, row.PersonName, row.Company, row.Title, row.ToEmail, row.ContactStatus, row.RiskyApproved,
			row.GenericInbox, row.IsRoleAddress, row.Subject, row.BodyText, row.State, row.SentAt, row.CreatedAt, row.UpdatedAt),
		SentenceEvidence: pyJSONText(row.SentenceEvidence, "[]"),
		SourceSnapshot:   pyJSONText(row.SourceSnapshot, "{}"),
	})
}

// pyJSONText re-serializes a stored json column the way Python's read-then-FastAPI round trip does: key order
// kept, strings unescaped (ensure_ascii off), floats in repr form, SQL/JSON null replaced by empty.
// Unparseable text is passed through unchanged (a json column cannot hold it).
func pyJSONText(raw, empty string) json.RawMessage {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := pyJSONValue(dec, &buf); err != nil {
		return json.RawMessage(raw)
	}
	switch buf.String() { // Python `x or empty`: every falsy value maps to the empty container
	case "null", "[]", "{}", "0", "0.0", `""`, "false":
		return json.RawMessage(empty)
	}
	return buf.Bytes()
}

func pyJSONValue(dec *json.Decoder, buf *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		closer := byte(']')
		if t == '{' {
			closer = '}'
		}
		buf.WriteByte(byte(t))
		for first := true; dec.More(); first = false {
			if !first {
				buf.WriteByte(',')
			}
			if t == '{' {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				pyJSONString(buf, k.(string))
				buf.WriteByte(':')
			}
			if err := pyJSONValue(dec, buf); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		buf.WriteByte(closer)
	case string:
		pyJSONString(buf, t)
	case json.Number:
		buf.WriteString(pyNumber(t.String()))
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case nil:
		buf.WriteString("null")
	default:
		return io.ErrUnexpectedEOF
	}
	return nil
}

func pyJSONString(buf *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	// Go escapes U+2028/U+2029; Python (ensure_ascii off) emits them raw. Walk escape pairs so a literal
	// backslash followed by "u2028" (encoded with a doubled backslash) is left alone.
	for i := 0; i < len(out); i++ {
		if out[i] != '\\' || i+1 >= len(out) {
			buf.WriteByte(out[i])
			continue
		}
		switch {
		case bytes.HasPrefix(out[i:], []byte("\\u2028")):
			buf.WriteRune(0x2028)
			i += 5
		case bytes.HasPrefix(out[i:], []byte("\\u2029")):
			buf.WriteRune(0x2029)
			i += 5
		default:
			buf.WriteByte(out[i])
			buf.WriteByte(out[i+1])
			i++
		}
	}
}

// pyNumber keeps integers verbatim and renders floats like Python repr (what json.dumps prints).
func pyNumber(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) {
		return s
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	if a := math.Abs(f); a >= 1e16 || a < 1e-4 {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	out := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(out, ".") {
		out += ".0"
	}
	return out
}
