package destinbound

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ErrEventIDRequired is Python's ValueError("external_event_id is required").
var ErrEventIDRequired = errors.New("external_event_id is required")

// ErrInvalidBody marks a body shape Python could not have produced via the route.
var ErrInvalidBody = errors.New("destinbound: invalid inbound body")

// UnmatchedError is the receipt error text of an unmatched callback.
const UnmatchedError = "No workspace lead matched the callback"

// AllowedFields is the exact ALLOWED_FIELDS allow-list of lead columns a CRM may write.
var AllowedFields = map[string]bool{
	"company": true, "website": true, "email": true, "phone": true, "contact_person": true, "contact_title": true,
	"city": true, "state": true, "address": true, "specialization": true, "company_size": true,
	"employee_count_exact": true, "description": true, "revenue_range": true, "founded_year": true,
	"industry_tags": true, "technologies": true, "funding_stage": true, "linkedin_url": true,
	"twitter_url": true, "facebook_url": true, "secondary_emails": true, "secondary_phones": true,
	"decision_makers": true, "status": true, "notes": true,
}

// Destination is the slice of an audience destination reconcile needs.
// Config and FieldMap are the raw JSON column texts (FieldMap order matters:
// on a target collision the last entry wins, as in Python's dict).
type Destination struct {
	ID              string
	WorkspaceID     string
	DestinationType string
	Config          json.RawMessage
	FieldMap        json.RawMessage
}

// Field is one key of an ordered JSON object.
type Field struct {
	Key   string
	Value any
}

// OrderedFields is a JSON object that keeps its key order (later duplicates
// replace the value in the first position, like a Python dict).
type OrderedFields []Field

// InboundBody is the callback body as a dict of the keys that were present
// (external_event_id, external_record_id, lead_id, fields). Values use the
// canonjson shapes (string, int64, float64, bool, nil, []any, map[string]any);
// a "fields" object is an OrderedFields.
type InboundBody struct{ m map[string]any }

// NewInboundBody wraps an already decoded body. A "fields" map[string]any is
// treated as an object with sorted keys.
func NewInboundBody(m map[string]any) InboundBody { return InboundBody{m: m} }

// FillModelDefaults adds the keys pydantic's model_dump() always emits.
func (b *InboundBody) FillModelDefaults() {
	if b.m == nil {
		b.m = map[string]any{}
	}
	for _, k := range []string{"external_event_id", "external_record_id", "lead_id"} {
		if _, ok := b.m[k]; !ok {
			b.m[k] = nil
		}
	}
	if _, ok := b.m["fields"]; !ok {
		b.m["fields"] = OrderedFields{}
	}
}

// ParseInboundBody decodes a JSON object body. Unknown keys are dropped.
func ParseInboundBody(raw []byte) (InboundBody, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	obj, err := decodeObject(dec)
	if err != nil {
		return InboundBody{}, fmt.Errorf("%w: %v", ErrInvalidBody, err)
	}
	m := map[string]any{}
	for _, f := range obj {
		switch f.Key {
		case "external_event_id", "external_record_id", "company_id", "person_id", "fields":
			m[f.Key] = f.Value
		case "lead_id":
			switch v := f.Value.(type) {
			case nil:
				m[f.Key] = nil
			case int64:
				m[f.Key] = v
			default:
				return InboundBody{}, fmt.Errorf("%w: lead_id must be an integer", ErrInvalidBody)
			}
		}
	}
	return InboundBody{m: m}, nil
}

// decodeObject reads one JSON value from dec; an object becomes OrderedFields
// (nested values via canonjson), anything else its canonjson value.
func decodeObject(dec *json.Decoder) (OrderedFields, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("body must be a JSON object")
	}
	var out OrderedFields
	idx := map[string]int{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("bad object key")
		}
		var rm json.RawMessage
		if err := dec.Decode(&rm); err != nil {
			return nil, err
		}
		var val any
		if key == "fields" && len(rm) > 0 && rm[0] == '{' {
			val, err = decodeObject(json.NewDecoder(bytes.NewReader(rm)))
		} else {
			val, err = canonjson.Decode(rm)
		}
		if err != nil {
			return nil, err
		}
		if i, dup := idx[key]; dup {
			out[i].Value = val
			continue
		}
		idx[key] = len(out)
		out = append(out, Field{Key: key, Value: val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// Receipt is the API shape of a destination_inbound_receipts row.
type Receipt struct {
	CompanyID        *string   `json:"company_id,omitempty"`
	PersonID         *string   `json:"person_id,omitempty"`
	ID               string    `json:"id"`
	DestinationID    string    `json:"destination_id"`
	Provider         string    `json:"provider"`
	ExternalEventID  string    `json:"external_event_id"`
	ExternalRecordID *string   `json:"external_record_id"`
	LeadID           *int32    `json:"lead_id"`
	Status           string    `json:"status"`
	ConflictPolicy   string    `json:"conflict_policy"`
	AppliedFields    []string  `json:"applied_fields"`
	IgnoredFields    []string  `json:"ignored_fields"`
	Error            *string   `json:"error"`
	CreatedAt        time.Time `json:"created_at"`
}

// pyStr is str(x) for the scalar shapes a JSON body yields; falsy is true for
// the values `x or ""` replaces (None, "", 0, 0.0, False).
func pyStr(v any) (s string, falsy bool, err error) {
	switch x := v.(type) {
	case nil:
		return "", true, nil
	case string:
		return x, x == "", nil
	case int64:
		return fmt.Sprint(x), x == 0, nil
	case float64:
		r, e := canonjson.FloatRepr(x)
		return r, x == 0, e
	case bool:
		if x {
			return "True", false, nil
		}
		return "False", true, nil
	}
	return "", false, ErrInvalidBody
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// idText is str(v or "").strip()[:255].
func idText(v any) (string, error) {
	s, falsy, err := pyStr(v)
	if err != nil {
		return "", err
	}
	if falsy {
		return "", nil
	}
	return trunc(strings.TrimSpace(s), 255), nil
}

// leadWriter is the fixed per-column statement table: each allowed column has
// its own sqlc statement; no SQL string is ever built from a field name.
type leadWriter struct {
	isInt bool
	set   func(ctx context.Context, q *gen.Queries, ws string, id int64, s string, n int32) error
}

func textCol[P any](
	run func(*gen.Queries, context.Context, P) error, mk func(v, ws string, id int64) P,
) leadWriter {
	return leadWriter{set: func(ctx context.Context, q *gen.Queries, ws string, id int64, s string, _ int32) error {
		return run(q, ctx, mk(s, ws, id))
	}}
}

var leadWriters = map[string]leadWriter{
	"company": textCol((*gen.Queries).DestInboundSetLead_company, func(v, ws string, id int64) gen.DestInboundSetLead_companyParams {
		return gen.DestInboundSetLead_companyParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"website": textCol((*gen.Queries).DestInboundSetLead_website, func(v, ws string, id int64) gen.DestInboundSetLead_websiteParams {
		return gen.DestInboundSetLead_websiteParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"email": textCol((*gen.Queries).DestInboundSetLead_email, func(v, ws string, id int64) gen.DestInboundSetLead_emailParams {
		return gen.DestInboundSetLead_emailParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"phone": textCol((*gen.Queries).DestInboundSetLead_phone, func(v, ws string, id int64) gen.DestInboundSetLead_phoneParams {
		return gen.DestInboundSetLead_phoneParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"contact_person": textCol((*gen.Queries).DestInboundSetLead_contact_person, func(v, ws string, id int64) gen.DestInboundSetLead_contact_personParams {
		return gen.DestInboundSetLead_contact_personParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"contact_title": textCol((*gen.Queries).DestInboundSetLead_contact_title, func(v, ws string, id int64) gen.DestInboundSetLead_contact_titleParams {
		return gen.DestInboundSetLead_contact_titleParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"city": textCol((*gen.Queries).DestInboundSetLead_city, func(v, ws string, id int64) gen.DestInboundSetLead_cityParams {
		return gen.DestInboundSetLead_cityParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"state": textCol((*gen.Queries).DestInboundSetLead_state, func(v, ws string, id int64) gen.DestInboundSetLead_stateParams {
		return gen.DestInboundSetLead_stateParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"address": textCol((*gen.Queries).DestInboundSetLead_address, func(v, ws string, id int64) gen.DestInboundSetLead_addressParams {
		return gen.DestInboundSetLead_addressParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"specialization": textCol((*gen.Queries).DestInboundSetLead_specialization, func(v, ws string, id int64) gen.DestInboundSetLead_specializationParams {
		return gen.DestInboundSetLead_specializationParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"company_size": textCol((*gen.Queries).DestInboundSetLead_company_size, func(v, ws string, id int64) gen.DestInboundSetLead_company_sizeParams {
		return gen.DestInboundSetLead_company_sizeParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"description": textCol((*gen.Queries).DestInboundSetLead_description, func(v, ws string, id int64) gen.DestInboundSetLead_descriptionParams {
		return gen.DestInboundSetLead_descriptionParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"revenue_range": textCol((*gen.Queries).DestInboundSetLead_revenue_range, func(v, ws string, id int64) gen.DestInboundSetLead_revenue_rangeParams {
		return gen.DestInboundSetLead_revenue_rangeParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"founded_year": textCol((*gen.Queries).DestInboundSetLead_founded_year, func(v, ws string, id int64) gen.DestInboundSetLead_founded_yearParams {
		return gen.DestInboundSetLead_founded_yearParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"industry_tags": textCol((*gen.Queries).DestInboundSetLead_industry_tags, func(v, ws string, id int64) gen.DestInboundSetLead_industry_tagsParams {
		return gen.DestInboundSetLead_industry_tagsParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"technologies": textCol((*gen.Queries).DestInboundSetLead_technologies, func(v, ws string, id int64) gen.DestInboundSetLead_technologiesParams {
		return gen.DestInboundSetLead_technologiesParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"funding_stage": textCol((*gen.Queries).DestInboundSetLead_funding_stage, func(v, ws string, id int64) gen.DestInboundSetLead_funding_stageParams {
		return gen.DestInboundSetLead_funding_stageParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"linkedin_url": textCol((*gen.Queries).DestInboundSetLead_linkedin_url, func(v, ws string, id int64) gen.DestInboundSetLead_linkedin_urlParams {
		return gen.DestInboundSetLead_linkedin_urlParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"twitter_url": textCol((*gen.Queries).DestInboundSetLead_twitter_url, func(v, ws string, id int64) gen.DestInboundSetLead_twitter_urlParams {
		return gen.DestInboundSetLead_twitter_urlParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"facebook_url": textCol((*gen.Queries).DestInboundSetLead_facebook_url, func(v, ws string, id int64) gen.DestInboundSetLead_facebook_urlParams {
		return gen.DestInboundSetLead_facebook_urlParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"secondary_emails": textCol((*gen.Queries).DestInboundSetLead_secondary_emails, func(v, ws string, id int64) gen.DestInboundSetLead_secondary_emailsParams {
		return gen.DestInboundSetLead_secondary_emailsParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"secondary_phones": textCol((*gen.Queries).DestInboundSetLead_secondary_phones, func(v, ws string, id int64) gen.DestInboundSetLead_secondary_phonesParams {
		return gen.DestInboundSetLead_secondary_phonesParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"decision_makers": textCol((*gen.Queries).DestInboundSetLead_decision_makers, func(v, ws string, id int64) gen.DestInboundSetLead_decision_makersParams {
		return gen.DestInboundSetLead_decision_makersParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"status": textCol((*gen.Queries).DestInboundSetLead_status, func(v, ws string, id int64) gen.DestInboundSetLead_statusParams {
		return gen.DestInboundSetLead_statusParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"notes": textCol((*gen.Queries).DestInboundSetLead_notes, func(v, ws string, id int64) gen.DestInboundSetLead_notesParams {
		return gen.DestInboundSetLead_notesParams{Value: v, WorkspaceID: ws, ID: id}
	}),
	"employee_count_exact": {isInt: true, set: func(ctx context.Context, q *gen.Queries, ws string, id int64, _ string, n int32) error {
		return q.DestInboundSetLead_employee_count_exact(ctx, gen.DestInboundSetLead_employee_count_exactParams{Value: n, WorkspaceID: ws, ID: id})
	}},
}

// current returns the lead's present value of an allowed column: text
// (isNull when SQL NULL) or, for employee_count_exact, the integer.
func current(l gen.DestInboundLeadLockRow, col string) (text string, isNull bool, n int32) {
	t := func(v pgtype.Text) (string, bool) { return v.String, !v.Valid }
	switch col {
	case "company":
		return l.Company, false, 0
	case "website":
		text, isNull = t(l.Website)
	case "email":
		text, isNull = t(l.Email)
	case "phone":
		text, isNull = t(l.Phone)
	case "contact_person":
		text, isNull = t(l.ContactPerson)
	case "contact_title":
		text, isNull = t(l.ContactTitle)
	case "city":
		text, isNull = t(l.City)
	case "state":
		text, isNull = t(l.State)
	case "address":
		text, isNull = t(l.Address)
	case "specialization":
		text, isNull = t(l.Specialization)
	case "company_size":
		text, isNull = t(l.CompanySize)
	case "employee_count_exact":
		return "", !l.EmployeeCountExact.Valid, l.EmployeeCountExact.Int32
	case "description":
		text, isNull = t(l.Description)
	case "revenue_range":
		text, isNull = t(l.RevenueRange)
	case "founded_year":
		text, isNull = t(l.FoundedYear)
	case "industry_tags":
		text, isNull = t(l.IndustryTags)
	case "technologies":
		text, isNull = t(l.Technologies)
	case "funding_stage":
		text, isNull = t(l.FundingStage)
	case "linkedin_url":
		text, isNull = t(l.LinkedinUrl)
	case "twitter_url":
		text, isNull = t(l.TwitterUrl)
	case "facebook_url":
		text, isNull = t(l.FacebookUrl)
	case "secondary_emails":
		text, isNull = t(l.SecondaryEmails)
	case "secondary_phones":
		text, isNull = t(l.SecondaryPhones)
	case "decision_makers":
		text, isNull = t(l.DecisionMakers)
	case "status":
		text, isNull = t(l.Status)
	case "notes":
		text, isNull = t(l.Notes)
	}
	return text, isNull, 0
}

// coerceInt is Python's int(value) for the JSON scalar shapes, limited to what
// the INTEGER column holds (an out-of-range value is a failed coercion; Python
// would 500 at the database).
func coerceInt(v any) (int32, bool) {
	var b *big.Int
	switch x := v.(type) {
	case int64:
		b = big.NewInt(x)
	case bool:
		b = big.NewInt(0)
		if x {
			b = big.NewInt(1)
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		b, _ = new(big.Float).SetFloat64(math.Trunc(x)).Int(nil)
	case string:
		var ok bool
		if b, ok = parsePyInt(x); !ok {
			return 0, false
		}
	default:
		return 0, false
	}
	if !b.IsInt64() || b.Int64() > math.MaxInt32 || b.Int64() < math.MinInt32 {
		return 0, false
	}
	return int32(b.Int64()), true
}

// textValue is the stored form of a non-empty allowed value for a text column.
// Python passed the raw JSON value to the driver: strings as is, numbers and
// booleans through the database's text cast. Containers are not storable.
func textValue(v any) (s string, nonString, ok bool) {
	switch x := v.(type) {
	case string:
		return x, false, true
	case int64:
		return fmt.Sprint(x), true, true
	case float64:
		r, err := canonjson.FloatRepr(x)
		return r, true, err == nil
	case bool:
		if x {
			return "true", true, true
		}
		return "false", true, true
	}
	return "", false, false
}

// fieldsOf returns the body's "fields" as ordered pairs; ok is false when it
// is not an object (Python: treated as {}).
func fieldsOf(v any) (OrderedFields, bool) {
	switch x := v.(type) {
	case OrderedFields:
		return x, true
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(OrderedFields, 0, len(keys))
		for _, k := range keys {
			out = append(out, Field{Key: k, Value: x[k]})
		}
		return out, true
	}
	return nil, false
}

// reverseMap maps CRM field name -> OpenGTM column: {target: source for
// source, target in field_map.items()}, the last entry winning a collision.
func reverseMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out
	}
	obj, err := decodeObject(json.NewDecoder(bytes.NewReader(raw)))
	if err != nil {
		return out
	}
	for _, f := range obj {
		if target, ok := f.Value.(string); ok {
			out[target] = f.Key
		}
	}
	return out
}

func conflictPolicy(raw json.RawMessage) string {
	var cfg map[string]any
	if json.Unmarshal(raw, &cfg) == nil {
		if s, ok := cfg["inbound_conflict_policy"].(string); ok && s == "crm_wins" {
			return "crm_wins"
		}
	}
	return "fill_missing"
}

func fingerprint(b InboundBody) (string, error) {
	m := map[string]any{}
	for _, k := range []string{"external_event_id", "external_record_id", "lead_id", "fields"} {
		v, ok := b.m[k]
		if !ok {
			continue
		}
		if of, isObj := v.(OrderedFields); isObj {
			mm := make(map[string]any, len(of))
			for _, f := range of {
				mm[f.Key] = f.Value
			}
			v = mm
		}
		m[k] = v
	}
	out, err := canonjson.Marshal(m, true)
	if err != nil {
		return "", fmt.Errorf("destinbound: fingerprint: %w", err)
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:]), nil
}

func toReceipt(id, destID, provider, eventID string, recID pgtype.Text, leadID pgtype.Int4, status, policy string,
	applied, ignored []byte, errText pgtype.Text, created pgtype.Timestamp, refs ...pgtype.Text) (Receipt, error) {
	r := Receipt{ID: id, DestinationID: destID, Provider: provider, ExternalEventID: eventID, Status: status,
		ConflictPolicy: policy, CreatedAt: created.Time}
	if len(refs) > 0 && refs[0].Valid {
		r.CompanyID = &refs[0].String
	}
	if len(refs) > 1 && refs[1].Valid {
		r.PersonID = &refs[1].String
	}
	if recID.Valid {
		r.ExternalRecordID = &recID.String
	}
	if leadID.Valid {
		r.LeadID = &leadID.Int32
	}
	if errText.Valid {
		r.Error = &errText.String
	}
	r.AppliedFields, r.IgnoredFields = []string{}, []string{}
	if err := json.Unmarshal(applied, &r.AppliedFields); err != nil {
		return Receipt{}, fmt.Errorf("destinbound: receipt applied_fields: %w", err)
	}
	if err := json.Unmarshal(ignored, &r.IgnoredFields); err != nil {
		return Receipt{}, fmt.Errorf("destinbound: receipt ignored_fields: %w", err)
	}
	return r, nil
}

func priorReceipt(ctx context.Context, q *gen.Queries, ws, destID, eventID string) (*Receipt, error) {
	p, err := q.DestInboundReceiptByEvent(ctx, gen.DestInboundReceiptByEventParams{
		WorkspaceID: ws, DestinationID: destID, ExternalEventID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("destinbound: load receipt: %w", err)
	}
	r, err := toReceipt(p.ID, p.DestinationID, p.Provider, p.ExternalEventID, p.ExternalRecordID, p.LeadID,
		p.Status, p.ConflictPolicy, p.AppliedFields, p.IgnoredFields, p.Error, p.CreatedAt, p.CompanyEntityID, p.PersonEntityID)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func isEventConflict(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "uq_destination_inbound_event"
}

// Reconcile applies a CRM callback to a lead and records an idempotent receipt
// (Python reconcile). Run inside tenancy.InWorkspace(dest.WorkspaceID) at READ
// COMMITTED; the receipt insert and lead writes share a savepoint so that a
// concurrent winner of the same event turns this call into a replay of the
// winner's receipt with none of this call's lead writes kept.
func Reconcile(ctx context.Context, tx pgx.Tx, dest Destination, body InboundBody) (Receipt, bool, error) {
	ws := dest.WorkspaceID
	if err := requireTenant(ctx, tx, ws); err != nil {
		return Receipt{}, false, err
	}
	q := gen.New(tx)

	eventID, err := idText(body.m["external_event_id"])
	if err != nil {
		return Receipt{}, false, err
	}
	if eventID == "" {
		return Receipt{}, false, ErrEventIDRequired
	}
	if p, err := priorReceipt(ctx, q, ws, dest.ID, eventID); err != nil || p != nil {
		if err != nil {
			return Receipt{}, false, err
		}
		return *p, true, nil
	}
	extText, err := idText(body.m["external_record_id"])
	if err != nil {
		return Receipt{}, false, err
	}
	if out, replayed, handled, e := reconcileRecords(ctx, tx, dest, body, eventID, extText); handled {
		return out, replayed, e
	}
	externalID := pgtype.Text{String: extText, Valid: extText != ""}

	fields, fieldsIsObj := fieldsOf(body.m["fields"])
	if !fieldsIsObj {
		fields = nil
	}

	// Lead resolution: explicit id, then prior successful delivery, then unique email.
	var leadID *int64
	switch v := body.m["lead_id"].(type) {
	case nil:
	case int64:
		leadID = &v
	default:
		return Receipt{}, false, fmt.Errorf("%w: lead_id must be an integer", ErrInvalidBody)
	}
	if leadID == nil && externalID.Valid {
		id, err := q.DestInboundDeliveryLead(ctx, gen.DestInboundDeliveryLeadParams{
			WorkspaceID: ws, DestinationID: dest.ID, ExternalID: externalID.String})
		if err == nil {
			v := int64(id.Int32)
			if id.Valid {
				leadID = &v
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Receipt{}, false, fmt.Errorf("destinbound: delivery lookup: %w", err)
		}
	}
	if leadID == nil && fieldsIsObj {
		var emailVal any
		for _, f := range fields {
			if f.Key == "email" {
				emailVal = f.Value
			}
		}
		if s, falsy, err := pyStr(emailVal); err == nil && !falsy {
			ids, err := q.DestInboundLeadsByEmail(ctx, gen.DestInboundLeadsByEmailParams{
				WorkspaceID: ws, Email: strings.TrimSpace(s)})
			if err != nil {
				return Receipt{}, false, fmt.Errorf("destinbound: email lookup: %w", err)
			}
			if len(ids) == 1 {
				v := int64(ids[0])
				leadID = &v
			}
		}
	}
	if leadID != nil && (*leadID > math.MaxInt32 || *leadID < math.MinInt32) {
		return Receipt{}, false, fmt.Errorf("%w: lead_id out of range", ErrInvalidBody)
	}
	var lead *gen.DestInboundLeadLockRow
	if leadID != nil {
		row, err := q.DestInboundLeadLock(ctx, gen.DestInboundLeadLockParams{WorkspaceID: ws, ID: *leadID})
		if err == nil {
			lead = &row
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Receipt{}, false, fmt.Errorf("destinbound: lock lead: %w", err)
		}
	}

	// Reverse the field map, keep the allow-list.
	rev := reverseMap(dest.FieldMap)
	type kv struct {
		key string
		val any
	}
	var mapped []kv
	pos := map[string]int{}
	for _, f := range fields {
		k := f.Key
		if r, ok := rev[k]; ok {
			k = r
		}
		if i, ok := pos[k]; ok {
			mapped[i].val = f.Value
			continue
		}
		pos[k] = len(mapped)
		mapped = append(mapped, kv{k, f.Value})
	}
	allowed := map[string]any{}
	var allowedOrder []string
	for _, m := range mapped {
		if AllowedFields[m.key] {
			allowed[m.key] = m.val
			allowedOrder = append(allowedOrder, m.key)
		}
	}
	ints := map[string]int32{}
	if v, ok := allowed["employee_count_exact"]; ok {
		if n, ok := coerceInt(v); ok && n >= 0 && n <= 1000000000 {
			ints["employee_count_exact"] = n
		} else {
			delete(allowed, "employee_count_exact")
		}
	}
	policy := conflictPolicy(dest.Config)

	ignoredSet := map[string]bool{}
	for _, m := range mapped {
		if _, ok := allowed[m.key]; !ok {
			ignoredSet[m.key] = true
		}
	}
	type write struct {
		col string
		s   string
		n   int32
	}
	var writes []write
	applied := []string{}
	status := "applied"
	var errText pgtype.Text
	if lead == nil {
		status = "unmatched"
		errText = pgtype.Text{String: UnmatchedError, Valid: true}
	} else {
		for _, col := range allowedOrder {
			if _, ok := allowed[col]; !ok {
				continue
			}
			val := allowed[col]
			curText, curNull, curInt := current(*lead, col)
			if col == "employee_count_exact" {
				n := ints[col]
				populated := !curNull // 0 is populated, as in Python
				if policy == "fill_missing" && populated {
					ignoredSet[col] = true
					continue
				}
				if curNull || curInt != n {
					writes = append(writes, write{col: col, n: n})
					applied = append(applied, col)
				}
				continue
			}
			if val == nil {
				ignoredSet[col] = true
				continue
			}
			s, nonString, ok := textValue(val)
			if !ok || (!nonString && s == "") {
				ignoredSet[col] = true
				continue
			}
			populated := !curNull && curText != "" && curText != "N/A"
			if policy == "fill_missing" && populated {
				ignoredSet[col] = true
				continue
			}
			if nonString || curNull || curText != s {
				writes = append(writes, write{col: col, s: s})
				applied = append(applied, col)
			}
		}
		if len(applied) == 0 {
			status = "no_change"
		}
	}
	ignored := make([]string, 0, len(ignoredSet))
	for k := range ignoredSet {
		ignored = append(ignored, k)
	}
	sort.Strings(ignored)
	sort.Strings(applied)

	fp, err := fingerprint(body)
	if err != nil {
		return Receipt{}, false, err
	}
	appliedJSON, _ := json.Marshal(applied)
	ignoredJSON, _ := json.Marshal(ignored)
	var leadCol pgtype.Int4
	if leadID != nil {
		leadCol = pgtype.Int4{Int32: int32(*leadID), Valid: true}
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return Receipt{}, false, fmt.Errorf("destinbound: savepoint: %w", err)
	}
	sq := gen.New(sp)
	fail := func(err error) (Receipt, bool, error) {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		return Receipt{}, false, err
	}
	var priorSource string
	if err := sp.QueryRow(ctx, `SELECT coalesce(current_setting('opengtm.record_source',true),'')`).Scan(&priorSource); err != nil {
		return fail(err)
	}
	source := "connector:" + dest.ID
	if policy == "crm_wins" {
		source = "connector:crm-wins:" + dest.ID
	}
	if _, err := sp.Exec(ctx, `SELECT set_config('opengtm.record_source',$1,true)`, source); err != nil {
		return fail(err)
	}
	for _, w := range writes {
		lw, ok := leadWriters[w.col]
		if !ok || !AllowedFields[w.col] {
			return fail(fmt.Errorf("destinbound: column %q is not writable", w.col))
		}
		if err := lw.set(ctx, sq, ws, *leadID, w.s, w.n); err != nil {
			return fail(fmt.Errorf("destinbound: update lead: %w", err))
		}
	}
	if _, err := sp.Exec(ctx, `SELECT set_config('opengtm.record_source',$1,true)`, priorSource); err != nil {
		return fail(err)
	}
	ins, err := sq.DestInboundReceiptInsert(ctx, gen.DestInboundReceiptInsertParams{
		ID: newID(), WorkspaceID: ws, DestinationID: dest.ID, Provider: dest.DestinationType,
		ExternalEventID: eventID, ExternalRecordID: externalID, LeadID: leadCol, Status: status,
		ConflictPolicy: policy, AppliedFields: appliedJSON, IgnoredFields: ignoredJSON,
		PayloadFingerprint: fp, Error: errText})
	if err != nil {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		if isEventConflict(err) {
			if p, perr := priorReceipt(ctx, q, ws, dest.ID, eventID); perr != nil {
				return Receipt{}, false, perr
			} else if p != nil {
				return *p, true, nil
			}
		}
		return Receipt{}, false, fmt.Errorf("destinbound: insert receipt: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return Receipt{}, false, fmt.Errorf("destinbound: release savepoint: %w", err)
	}
	r, err := toReceipt(ins.ID, ins.DestinationID, ins.Provider, ins.ExternalEventID, ins.ExternalRecordID,
		ins.LeadID, ins.Status, ins.ConflictPolicy, ins.AppliedFields, ins.IgnoredFields, ins.Error, ins.CreatedAt, ins.CompanyEntityID, ins.PersonEntityID)
	if err != nil {
		return Receipt{}, false, err
	}
	return r, false, nil
}

// MarshalJSON renders created_at like FastAPI's naive datetime.
func (r Receipt) MarshalJSON() ([]byte, error) {
	if r.LeadID != nil && *r.LeadID > 0 {
		r.CompanyID, r.PersonID = nil, nil
	}
	type plain Receipt
	return json.Marshal(struct {
		plain
		CreatedAt string `json:"created_at"`
	}{plain(r), pyISO(r.CreatedAt, false)})
}
