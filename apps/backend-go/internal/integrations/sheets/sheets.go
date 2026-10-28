// Package sheets ports services/integrations/sheets.py: append or upsert a
// workbook row in a Google Sheet through the Sheets REST API with an OAuth2
// bearer token (GOOGLE_SHEETS_TOKEN). Fixed host, redirects refused, 10 s
// timeout, 1 MiB body bound. Path segments (spreadsheet id, range) are
// percent-encoded like Python's quote(safe="") (Python concatenated the id and
// the append range raw; encoding them is a documented hardening, D4).
package sheets

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	// DefaultAppendRange and DefaultUpsertRange are Python's sheet_range
	// defaults; an empty range argument selects them.
	DefaultAppendRange = "Sheet1"
	DefaultUpsertRange = "Sheet1!A:ZZ"

	secretName = "GOOGLE_SHEETS_TOKEN"
	baseURL    = "https://sheets.googleapis.com/v4/spreadsheets/"
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

// plainNumber is _PLAIN_NUMBER; Python's `$` also matches before one final
// newline.
var plainNumber = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?\n?\z`)

// cell renders one value for a USER_ENTERED write without letting it run
// (_cell): a leading apostrophe keeps text that starts with = + - @ TAB CR
// verbatim, except plain numbers.
func cell(v any) string {
	text := ""
	if v != nil {
		text = intkit.Str(v)
	}
	if text != "" && strings.ContainsRune("=+-@\t\r", rune(text[0])) && !plainNumber.MatchString(text) {
		return "'" + text
	}
	return text
}

// columnName is the A1 column letters of a 1-based index (_column_name).
// rangeStart returns the 1-based column and row of the range's top-left cell;
// a missing column or row component (A:ZZ, 5:9) defaults to 1.
func rangeStart(sheetRange string) (col, row int) {
	a1 := sheetRange
	if i := strings.LastIndex(a1, "!"); i >= 0 {
		a1 = a1[i+1:]
	}
	a1, _, _ = strings.Cut(a1, ":")
	a1 = strings.ReplaceAll(a1, "$", "")
	col, row = 1, 1
	i := 0
	n := 0
	for ; i < len(a1) && (a1[i]|0x20) >= 'a' && (a1[i]|0x20) <= 'z'; i++ {
		n = n*26 + int(a1[i]|0x20-'a') + 1
	}
	if n > 0 {
		col = n
	}
	if r, err := strconv.Atoi(a1[i:]); err == nil && r > 0 {
		row = r
	}
	return col, row
}

func columnName(n int) string {
	out := ""
	for n > 0 {
		n--
		out = string(rune('A'+n%26)) + out
		n /= 26
	}
	return out
}

func cells(values []any) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = cell(v)
	}
	return out
}

func rowBody(row []any) []byte {
	b := intkit.NewObj()
	b.Set("values", []any{row})
	return intkit.Marshal(b)
}

// AppendRow appends one row. An empty sheetRange selects DefaultAppendRange.
func (c *Client) AppendRow(ctx context.Context, ws, spreadsheetID string, values []any, sheetRange string) (*Obj, error) {
	token, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return intkit.Fail("Google Sheets not connected (set GOOGLE_SHEETS_TOKEN — an OAuth2 access token)"), nil
	}
	if spreadsheetID == "" {
		return intkit.Fail("spreadsheet_id is required"), nil
	}
	if sheetRange == "" {
		sheetRange = DefaultAppendRange
	}
	resp, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodPost,
		URL:    baseURL + intkit.PyQuote(spreadsheetID) + "/values/" + intkit.PyQuote(sheetRange) + ":append",
		Query:  [][2]string{{"valueInputOption", "USER_ENTERED"}, {"insertDataOption", "INSERT_ROWS"}},
		Header: intkit.JSONHeaders(token),
		Body:   rowBody(cells(values)),
	}, token)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(intkit.ErrText(err, token)), nil
	}
	if resp.Status != 200 {
		return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, token)), nil
	}
	rng, err := updatedRange(resp.Body, false)
	if err != nil {
		return intkit.Fail(intkit.ErrText(err, token)), nil
	}
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("range", rng)
	return res, nil
}

// updatedRange reads updates.updatedRange (or, for an upsert, the top-level
// updatedRange first), with Python's chained .get semantics.
func updatedRange(body []byte, topLevelFirst bool) (any, error) {
	data, err := intkit.ParseJSON(body)
	if err != nil {
		return nil, err
	}
	if topLevelFirst {
		top, err := providers.PyGet(data, "updatedRange", nil)
		if err != nil {
			return nil, err
		}
		if providers.Truthy(top) {
			return top, nil
		}
	}
	updates, err := providers.PyGet(data, "updates", map[string]any{})
	if err != nil {
		return nil, err
	}
	return providers.PyGet(updates, "updatedRange", "")
}

// UpsertRow upserts a row whose first cell is the stable delivery key. An empty
// sheetRange selects DefaultUpsertRange.
func (c *Client) UpsertRow(ctx context.Context, ws, spreadsheetID string, values []any, idempotencyKey, sheetRange string) (*Obj, error) {
	token, err := intkit.Key(ctx, c.secrets, ws, secretName)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return intkit.Fail("Google Sheets not connected"), nil
	}
	if spreadsheetID == "" || idempotencyKey == "" {
		return intkit.Fail("spreadsheet_id and idempotency_key are required"), nil
	}
	if sheetRange == "" {
		sheetRange = DefaultUpsertRange
	}
	row := append([]any{idempotencyKey}, cells(values)...)
	base := baseURL + intkit.PyQuote(spreadsheetID) + "/values"
	fail := func(err error) (*Obj, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return intkit.Fail(providers.Trunc200(intkit.ErrText(err, token))), nil
	}
	lookup, err := intkit.Send(ctx, c.http, providers.Req{
		Method: http.MethodGet, URL: base + "/" + intkit.PyQuote(sheetRange),
		Query: [][2]string{{"majorDimension", "ROWS"}}, Header: intkit.JSONHeaders(token),
	}, token)
	if err != nil {
		return fail(err)
	}
	if lookup.Status != 200 {
		return intkit.Fail(intkit.HTTPError(lookup.Status, lookup.Body, token)), nil
	}
	existing, err := findRow(lookup.Body, idempotencyKey)
	if err != nil {
		return fail(err)
	}
	var (
		resp      providers.Response
		operation string
	)
	if existing > 0 {
		sheet := ""
		if i := strings.LastIndex(sheetRange, "!"); i >= 0 {
			sheet = sheetRange[:i]
		}
		startCol, startRow := rangeStart(sheetRange)
		abs := startRow + existing - 1 // lookup values begin at the range's first row
		target := sheet + "!" + columnName(startCol) + strconv.Itoa(abs) + ":" +
			columnName(startCol+len(row)-1) + strconv.Itoa(abs)
		resp, err = intkit.Send(ctx, c.http, providers.Req{
			Method: http.MethodPut, URL: base + "/" + intkit.PyQuote(target),
			Query: [][2]string{{"valueInputOption", "USER_ENTERED"}}, Header: intkit.JSONHeaders(token), Body: rowBody(row),
		}, token)
		operation = "updated"
	} else {
		resp, err = intkit.Send(ctx, c.http, providers.Req{
			Method: http.MethodPost, URL: base + "/" + intkit.PyQuote(sheetRange) + ":append",
			Query:  [][2]string{{"valueInputOption", "USER_ENTERED"}, {"insertDataOption", "INSERT_ROWS"}},
			Header: intkit.JSONHeaders(token), Body: rowBody(row),
		}, token)
		operation = "appended"
	}
	if err != nil {
		return fail(err)
	}
	if resp.Status != 200 {
		return intkit.Fail(intkit.HTTPError(resp.Status, resp.Body, token)), nil
	}
	rng, err := updatedRange(resp.Body, true)
	if err != nil {
		return fail(err)
	}
	res := intkit.NewObj()
	res.Set("success", true)
	res.Set("range", rng)
	res.Set("operation", operation)
	return res, nil
}

// findRow is the 1-based index of the first row whose first cell equals key
// (0 when none): `next((i for i, item in enumerate(rows, 1) if item and
// item[0] == key), None)`.
func findRow(body []byte, key string) (int, error) {
	data, err := intkit.ParseJSON(body)
	if err != nil {
		return 0, err
	}
	v, err := providers.PyGet(data, "values", nil)
	if err != nil {
		return 0, err
	}
	if !providers.Truthy(v) {
		return 0, nil
	}
	rows, ok := v.([]any)
	if !ok {
		switch v.(type) {
		case string, map[string]any:
			return 0, nil // iterates characters or keys, which never carry the delivery key
		}
		return 0, &providers.PyException{Msg: "'" + providers.TypeName(v) + "' object is not iterable"}
	}
	for i, item := range rows {
		if !providers.Truthy(item) {
			continue
		}
		first, err := intkit.Index0(item)
		if err != nil {
			return 0, err
		}
		if s, ok := first.(string); ok && s == key {
			return i + 1, nil
		}
	}
	return 0, nil
}
