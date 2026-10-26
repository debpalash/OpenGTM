package destdeliver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
)

// webhookHeaders is dict(cfg.get("headers") or {}) with Python's failure for a value that is not
// a mapping (the text mirrors CPython's dict() errors for the shapes JSON can produce).
func webhookHeaders(v any) (map[string]any, error) {
	if !pyTruthy(v) {
		return map[string]any{}, nil
	}
	switch x := v.(type) {
	case map[string]any:
		return copyMap(x), nil
	case string:
		return nil, errors.New("dictionary update sequence element #0 has length 1; 2 is required")
	case bool:
		return nil, errors.New("'bool' object is not iterable")
	case int, int64, json.Number:
		if n, ok := x.(json.Number); ok && containsAny(string(n), ".eE") {
			return nil, errors.New("'float' object is not iterable")
		}
		return nil, errors.New("'int' object is not iterable")
	case float64:
		return nil, errors.New("'float' object is not iterable")
	}
	return nil, errors.New("cannot convert dictionary update sequence element #0 to a sequence")
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, r := range s {
			if r == c {
				return true
			}
		}
	}
	return false
}

// webhook posts the audience.member.upsert envelope. The port owns URL guarding, header secret
// resolution, signing and the HTTP exchange.
func (d *Deliverer) webhook(ctx context.Context, dest Destination, leadID int, snapshot, mapped map[string]any, idem string, sec *secretSet) Result {
	cfg := copyMap(dest.Config)
	headers, err := webhookHeaders(cfg["headers"])
	if err != nil {
		return failed(err.Error(), sec)
	}
	headers["Idempotency-Key"] = idem
	cfg["headers"] = headers
	cfg["body"] = map[string]any{
		"event":          "audience.member.upsert",
		"audience_id":    dest.AudienceID,
		"destination_id": dest.ID,
		"lead_id":        int64(leadID),
		"data":           mapped,
	}
	res, err := d.ports.Webhook.Send(ctx, WebhookRequest{
		WorkspaceID: dest.WorkspaceID, DestinationType: "webhook", Config: cfg,
		Payload: snapshot, LeadID: int64(leadID), IdempotencyKey: idem,
	})
	return finish(res, err, sec)
}

// crm pushes through the CRM port with the destination type and field map merged into the config.
func (d *Deliverer) crm(ctx context.Context, dest Destination, leadID int, snapshot map[string]any) Result {
	cfg := copyMap(dest.Config)
	cfg["type"] = dest.Type
	fm := dest.FieldMap
	if fm == nil {
		fm = map[string]any{}
	}
	cfg["field_map"] = fm
	var res crmpush.Result = d.ports.CRM.Push(ctx, dest.WorkspaceID, cfg, snapshot, "", int64(leadID))
	if res.Success {
		return Result{Success: true, Summary: res.Value}
	}
	msg := res.Error
	if msg == "" {
		msg = "crm push failed"
	}
	return failed(msg, nil)
}
