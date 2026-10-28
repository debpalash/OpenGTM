package destdeliver

import "context"

const (
	summaryDuplicate = "Lead already in campaign"
	summaryAdded     = "Lead added to campaign"
)

// campaign adds the mapped lead to an Instantly or Smartlead campaign.
func (d *Deliverer) campaign(ctx context.Context, dest Destination, leadID int, mapped map[string]any, sec *secretSet) Result {
	port := d.ports.Instantly
	cfg := map[string]any{"campaign_id": strOr(dest.Config["campaign_id"], "")}
	if dest.Type == "smartlead" {
		port = d.ports.Smartlead
		cfg["settings"] = dest.Config["settings"]
	} else {
		skip := true // bool(cfg.get("skip_if_in_campaign", True)): present null is False
		if v, ok := dest.Config["skip_if_in_campaign"]; ok {
			skip = pyTruthy(v)
		}
		cfg["skip_if_in_campaign"] = skip
	}
	if port == nil {
		return notWired(dest.Type)
	}
	res, err := port.AddLead(ctx, CampaignRequest{WorkspaceID: dest.WorkspaceID, Config: cfg, Payload: mapped, LeadID: int64(leadID)})
	out := finish(res, err, sec)
	if err != nil {
		return out
	}
	switch {
	case res.Duplicate:
		out.Summary = summaryDuplicate
	case res.Success:
		out.Summary = summaryAdded
	default:
		out.Summary = ""
	}
	if dest.Type == "smartlead" {
		out.ExternalID = ""
	}
	return out
}

// opName is str(result.get("operation")): Python prints None when the port named none.
func opName(op string) string {
	if op == "" {
		return "None"
	}
	return op
}

// sheets upserts one spreadsheet row keyed dest:<id>:lead:<lead>.
func (d *Deliverer) sheets(ctx context.Context, dest Destination, leadID int, mapped map[string]any, sec *secretSet) Result {
	if d.ports.Sheets == nil {
		return notWired(dest.Type)
	}
	columns, _ := dest.Config["columns"].([]any)
	row := make([]any, 0, len(columns))
	for _, c := range columns {
		var cell any = ""
		if name, ok := c.(string); ok {
			if v, found := mapped[name]; found {
				cell = v
			}
		}
		row = append(row, cell)
	}
	res, err := d.ports.Sheets.Upsert(ctx, RowUpsert{
		WorkspaceID: dest.WorkspaceID, LeadID: int64(leadID), Payload: mapped,
		Config: map[string]any{
			"spreadsheet_id": strOr(dest.Config["spreadsheet_id"], ""),
			"row":            row,
			"key":            destinationRowKey(dest, leadID),
			"range":          strOr(dest.Config["range"], "Sheet1!A:ZZ"),
		},
	})
	out := finish(res, err, sec)
	if err != nil {
		return out
	}
	out.Summary = ""
	if res.Success {
		out.Summary = "Sheet row " + opName(res.Operation)
	}
	return out
}

// airtable upserts one record keyed by the idempotency field (default "OpenGTM ID").
func (d *Deliverer) airtable(ctx context.Context, dest Destination, leadID int, mapped map[string]any, sec *secretSet) Result {
	if d.ports.Airtable == nil {
		return notWired(dest.Type)
	}
	typecast := true // bool(cfg.get("typecast", True))
	if v, ok := dest.Config["typecast"]; ok {
		typecast = pyTruthy(v)
	}
	res, err := d.ports.Airtable.Upsert(ctx, RowUpsert{
		WorkspaceID: dest.WorkspaceID, LeadID: int64(leadID), Payload: mapped,
		Config: map[string]any{
			"base_id":           strOr(dest.Config["base_id"], ""),
			"table":             strOr(dest.Config["table"], ""),
			"key":               destinationRowKey(dest, leadID),
			"idempotency_field": strOr(dest.Config["idempotency_field"], "OpenGTM ID"),
			"typecast":          typecast,
		},
	})
	out := finish(res, err, sec)
	if err != nil {
		return out
	}
	out.Summary = ""
	if res.Success {
		out.Summary = "Airtable record " + opName(res.Operation)
	}
	return out
}
