// Package airtable ports services/integrations/airtable.py: append or upsert a
// workbook row as an Airtable record with a Personal Access Token. Plain REST
// to the fixed host api.airtable.com; redirects are refused, calls time out at
// 10 s and bodies are bounded to 1 MiB. Results are Python's dicts (key order
// kept); the token never appears in a result, error or log.
package airtable

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// DefaultIdempotencyField is Python's idempotency_field default.
const DefaultIdempotencyField = "OpenGTM ID"

const (
	secretName = "AIRTABLE_TOKEN"
	baseURL    = "https://api.airtable.com/v0/"
)

// Obj is an insertion-ordered JSON object.
type Obj = intkit.Obj

// Client holds the egress client and the credential source.
type Client struct {
	http    *http.Client
	secrets intkit.Secrets
}

// New returns a Client; httpc must be the egress (SSRF-safe) client.
func New(httpc *http.Client, s intkit.Secrets) *Client { return &Client{http: httpc, secrets: s} }

// IsConnected reports whether a token resolves for the workspace.
func (c *Client) IsConnected(ctx context.Context, ws string) (bool, error) {
	k, err := intkit.Key(ctx, c.secrets, ws, secretName)
	return k != "", err
}

func recordURL(baseID, table string) string {
	return baseURL + intkit.PyQuote(baseID) + "/" + intkit.PyQuote(table)
}

// PushRecord creates one record. The returned error is only a lookup failure
// or a cancelled context; every vendor failure is {"success": false, "error"}.
func (c *Client) PushRecord(ctx context.Context, ws string, fields *Obj, baseID, table string, typecast bool) (*Obj, error) {
	token, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return intkit.Fail("Airtable not connected (set AIRTABLE_TOKEN)"), nil
	}
	if baseID == "" || table == "" {
		return intkit.Fail("Airtable base_id and table are required"), nil
	}
	body := intkit.NewObj()
	body.Set("fields", intkit.Clean(fields))
	body.Set("typecast", typecast)
	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPost, URL: recordURL(baseID, table), Header: intkit.JSONHeaders(token), Body: intkit.Marshal(body),
	}, token)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(intkit.ErrText(err, token)), nil
	}
	if resp.Status != 200 && resp.Status != 201 {
		return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, token)), nil
	}
	data, err := intkit.ParseJSON(resp.Body)
	var id any
	if err == nil {
		id, err = providers.PyGet(data, "id", nil)
	}
	if err != nil {
		return intkit.Fail(intkit.ErrText(err, token)), nil
	}
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("record_id", id)
	return res, nil
}

// UpsertRecord creates or updates a record with Airtable performUpsert, merging
// on idempotencyField (the delivery key).
func (c *Client) UpsertRecord(ctx context.Context, ws string, fields *Obj, baseID, table, idempotencyKey, idempotencyField string, typecast bool) (*Obj, error) {
	token, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return intkit.Fail("Airtable not connected"), nil
	}
	if baseID == "" || table == "" || idempotencyKey == "" || idempotencyField == "" {
		return intkit.Fail("Airtable upsert configuration is incomplete"), nil
	}
	clean := intkit.Clean(fields)
	clean.Set(idempotencyField, idempotencyKey)
	merge := intkit.NewObj()
	merge.Set("fieldsToMergeOn", []any{idempotencyField})
	rec := intkit.NewObj()
	rec.Set("fields", clean)
	body := intkit.NewObj()
	body.Set("performUpsert", merge)
	body.Set("records", []any{rec})
	body.Set("typecast", typecast)
	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPatch, URL: recordURL(baseID, table), Header: intkit.JSONHeaders(token), Body: intkit.Marshal(body),
	}, token)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, token))), nil
	}
	if resp.Status != 200 && resp.Status != 201 {
		return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, token)), nil
	}
	res, err := upsertResult(resp.Body)
	if err != nil {
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, token))), nil
	}
	return res, nil
}

func upsertResult(body []byte) (*Obj, error) {
	data, err := intkit.ParseJSON(body)
	if err != nil {
		return nil, err
	}
	records, err := providers.PyGet(data, "records", nil)
	if err != nil {
		return nil, err
	}
	var recordID any
	if providers.Truthy(records) {
		first, err := intkit.Index0(records)
		if err != nil {
			return nil, err
		}
		if recordID, err = providers.PyGet(first, "id", nil); err != nil {
			return nil, err
		}
	}
	created, _ := providers.PyGet(data, "createdRecords", nil)
	op := "updated"
	if providers.Truthy(created) {
		op = "created"
	}
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("record_id", recordID)
	res.Set("operation", op)
	return res, nil
}
