package crmpush

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// NoLeadID is the lead_id=None of the Python signature. Any other value
// (including 0) is "present": row ids are positive serials, so a negative
// value never matches a row.
const NoLeadID int64 = -1

// Config is destination_config as Push reads it.
type Config struct {
	Type     string
	FieldMap crm.FieldMap
}

// ParseConfig reads type (default hubspot, lower-cased, not stripped) and the
// ordered field_map.
func ParseConfig(cfg map[string]any) Config {
	typ := "hubspot"
	if v := cfg["type"]; providers.Truthy(v) {
		typ = strings.ToLower(rowtmpl.PyStr(v))
	}
	return Config{Type: typ, FieldMap: FieldMapOf(cfg["field_map"])}
}

// Result is the {success, value, error} dict _push_crm returns. error is null
// when Error is empty.
type Result struct {
	Success bool
	Value   string
	Error   string
}

// MarshalJSON writes the keys in Python order: success, value, error.
func (r Result) MarshalJSON() ([]byte, error) {
	val, _ := json.Marshal(r.Value)
	out := `{"success":` + fmt.Sprint(r.Success) + `,"value":` + string(val) + `,"error":`
	if r.Error == "" {
		return []byte(out + `null}`), nil
	}
	e, _ := json.Marshal(r.Error)
	return []byte(out + string(e) + `}`), nil
}

func fail(msg string) Result { return Result{Error: msg} }

// HubSpotAPI is the part of *crm.HubSpot the push uses.
type HubSpotAPI interface {
	IsConnected(ctx context.Context, ws string) bool
	UpdateContactByID(ctx context.Context, ws, id string, properties crm.FieldMap) crm.UpdateResult
	PushLeadAsContact(ctx context.Context, ws string, lead crm.Lead, fieldMap crm.FieldMap) crm.PushResult
}

// SalesforceAPI is the part of *crm.Salesforce the push uses.
type SalesforceAPI interface {
	IsConnected(ctx context.Context, ws string) bool
	UpdateRecord(ctx context.Context, ws, sobject, id string, fields crm.FieldMap) crm.SalesforceUpdateResult
	PushLeadAsContact(ctx context.Context, ws string, lead crm.Lead, fieldMap crm.FieldMap) crm.SalesforcePushResult
}

var (
	_ HubSpotAPI    = (*crm.HubSpot)(nil)
	_ SalesforceAPI = (*crm.Salesforce)(nil)
)

// Deps are the collaborators of Push. Credentials are resolved per workspace
// inside the CRM clients; Push never reads a token itself.
type Deps struct {
	HubSpot    HubSpotAPI
	Salesforce SalesforceAPI
	Pool       *pgxpool.Pool
	Log        *slog.Logger
	// StoreRowID overrides the row stamp (tests). nil: StoreRowCRMID on Pool.
	StoreRowID func(ctx context.Context, ws, workbookID string, leadID int64, crmType, oldExtID, newID, newObject string) (bool, error)
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// stamp is the best-effort _store_row_crm_id: it never fails the push, never
// panics out of it, and logs a failure (the error text carries no secret).
func (d Deps) stamp(ctx context.Context, ws, workbookID string, leadID int64, crmType, oldExt, newID, newObject string) {
	defer func() {
		if r := recover(); r != nil {
			d.log().Warn("failed to store new crm id onto row", "crm", crmType, "lead_id", leadID, "error", fmt.Sprint(r))
		}
	}()
	fn := d.StoreRowID
	if fn == nil {
		fn = func(ctx context.Context, ws, wb string, id int64, ct, o, n, obj string) (bool, error) {
			return StoreRowCRMID(ctx, d.Pool, ws, wb, id, ct, o, n, obj)
		}
	}
	ok, err := fn(ctx, ws, workbookID, leadID, crmType, oldExt, newID, newObject)
	if err != nil {
		d.log().Warn("failed to store new crm id onto row", "crm", crmType, "lead_id", leadID, "error", err.Error())
		return
	}
	if !ok {
		d.log().Warn("row not identified for crm id stamp", "crm", crmType, "lead_id", leadID)
	}
}

// pyOr is str(x or default).
func pyOr(v any, def string) string {
	if !providers.Truthy(v) {
		return def
	}
	return rowtmpl.PyStr(v)
}

// idString renders a CRM id the way an f-string would; absent is "".
func idString(v any) string {
	if v == nil {
		return ""
	}
	return rowtmpl.PyStr(v)
}

// Push is output._push_crm. leadID is NoLeadID when the Python lead_id is None;
// workbookID "" is None. The CRM clients use workspace ws's own credentials
// (then the global fallback exactly as Python); a row stamp runs in
// tenancy.InWorkspace and is best effort.
func Push(ctx context.Context, d Deps, ws string, cfg map[string]any, leadData map[string]any, workbookID string, leadID int64) Result {
	c := ParseConfig(cfg)
	var label string
	switch c.Type {
	case "hubspot":
		label = "HubSpot"
		if d.HubSpot == nil || !d.HubSpot.IsConnected(ctx, ws) {
			return fail(label + " not connected")
		}
	case "salesforce":
		label = "Salesforce"
		if d.Salesforce == nil || !d.Salesforce.IsConnected(ctx, ws) {
			return fail(label + " not connected")
		}
	default:
		return fail("unsupported crm '" + c.Type + "'")
	}

	// Write-back by external id (CRM sync): update only the mapped fields; a
	// 404 falls through to the create path below.
	extID := wbcrud.TrimPy(pyOr(leadData["crm_external_id"], ""))
	rowCRM := strings.ToLower(wbcrud.TrimPy(pyOr(leadData["crm_type"], "")))
	crmDeleted := false
	if extID != "" && rowCRM == c.Type {
		fields := UpdateFieldMap(c.Type, cfg, leadData)
		var ok, notFound bool
		var errMsg string
		if c.Type == "hubspot" {
			res := d.HubSpot.UpdateContactByID(ctx, ws, extID, fields)
			ok, notFound, errMsg = res.Success, res.NotFound, res.Error
		} else {
			sobject := "Contact"
			if strings.ToLower(pyOr(leadData["crm_object"], "contact")) == "lead" {
				sobject = "Lead"
			}
			res := d.Salesforce.UpdateRecord(ctx, ws, sobject, extID, fields)
			ok, notFound, errMsg = res.Success, res.NotFound, res.Error
		}
		if ok {
			return Result{Success: true, Value: label + ": updated " + extID}
		}
		if !notFound {
			if errMsg == "" {
				errMsg = label + " update failed"
			}
			return fail(errMsg)
		}
		crmDeleted = true // deleted in the CRM: recreate below
	}

	// Create path (no external id, or the CRM record is gone).
	lead := dictLead(leadData)
	var success bool
	var action, errMsg string
	var newID any
	var newObject string
	if c.Type == "hubspot" {
		res := d.HubSpot.PushLeadAsContact(ctx, ws, lead, c.FieldMap)
		success, action, errMsg, newID, newObject = res.Success, res.Action, res.Error, res.HubspotID, "contact"
	} else {
		res := d.Salesforce.PushLeadAsContact(ctx, ws, lead, c.FieldMap)
		// Salesforce's create path makes a Lead sObject, not a Contact.
		success, action, errMsg, newID, newObject = res.Success, res.Action, res.Error, res.SalesforceID, "lead"
	}
	if !success {
		if errMsg == "" {
			errMsg = label + " push failed"
		}
		return fail(errMsg)
	}
	if crmDeleted && providers.Truthy(newID) && workbookID != "" && leadID != NoLeadID {
		d.stamp(ctx, ws, workbookID, leadID, c.Type, extID, idString(newID), newObject)
	}
	if action == "" {
		action = "synced"
	}
	return Result{Success: true, Value: strings.TrimSpace(label + ": " + action + " " + idString(newID))}
}
