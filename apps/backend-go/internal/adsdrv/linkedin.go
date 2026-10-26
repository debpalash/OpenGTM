package adsdrv

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

func (c Client) syncLinkedIn(ctx context.Context, wsID string, cfg map[string]any, rows []map[string]string, op, past string) (Result, error) {
	token, err := c.secret(ctx, wsID, "LINKEDIN_ACCESS_TOKEN")
	if err != nil {
		return Result{}, err
	}
	segmentID, err := cfgStr(cfg, "segment_id")
	if err != nil {
		return Result{}, err
	}
	action := "ADD"
	if op == "remove" {
		action = "REMOVE"
	}
	var elements []any
	for _, r := range rows {
		if r["email"] == "" {
			continue
		}
		elements = append(elements, map[string]any{"action": action, "userIds": []any{
			map[string]any{"idType": "SHA256_EMAIL", "idValue": r["email"]},
		}})
	}
	if len(elements) == 0 {
		return Result{Error: "LinkedIn requires a valid email identifier"}, nil
	}
	body, err := json.Marshal(map[string]any{"elements": elements})
	if err != nil {
		return Result{}, err
	}
	raw, err := c.do(ctx, "POST", c.Endpoints.LinkedIn+"/rest/dmpSegments/"+seg(segmentID)+"/users", map[string]string{
		"Authorization":             "Bearer " + token,
		"Linkedin-Version":          cfgGet(cfg, "api_version", "202607"),
		"X-Restli-Protocol-Version": "2.0.0",
		"X-RestLi-Method":           "BATCH_CREATE",
	}, "application/json", body)
	if err != nil {
		return Result{}, err
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return Result{}, err
	}
	if msg, bad := linkedinBatchFailure(v, len(elements)); bad {
		return Result{Error: msg, ExternalID: segmentID}, nil
	}
	return Result{Success: true, Summary: fmt.Sprintf("%s %d hashed users", past, len(elements)), ExternalID: segmentID}, nil
}

// pyInt reports whether a decoded JSON value is a Python int (bool counts, as in Python) and
// returns its value.
func pyInt(v any) (*big.Int, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return nil, false
		}
		n, ok := new(big.Int).SetString(string(t), 10)
		return n, ok
	}
	return nil, false
}

// statusIs201 is item.get("status") == 201 (201.0 equals 201 in Python).
func statusIs201(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	if i, isInt := pyInt(n); isInt {
		return i.Cmp(big.NewInt(201)) == 0
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return err == nil && f == 201
}

// linkedinBatchFailure validates every batch element without retaining LinkedIn's messages.
func linkedinBatchFailure(body any, expected int) (string, bool) {
	m, ok := body.(map[string]any)
	if !ok {
		return "LinkedIn returned an invalid batch response", true
	}
	elements, ok := m["elements"].([]any)
	if !ok {
		return "LinkedIn returned an invalid batch response", true
	}
	if len(elements) != expected {
		return fmt.Sprintf("LinkedIn returned an incomplete batch response (expected=%d, received=%d)", expected, len(elements)), true
	}
	rejected := 0
	type st struct {
		n   *big.Int
		txt string
	}
	seen := map[string]st{}
	for _, e := range elements {
		item, isMap := e.(map[string]any)
		if isMap && statusIs201(item["status"]) {
			continue
		}
		rejected++
		if !isMap {
			continue
		}
		if n, isInt := pyInt(item["status"]); isInt {
			txt := n.String()
			if b, isBool := item["status"].(bool); isBool {
				txt = map[bool]string{true: "True", false: "False"}[b]
			}
			seen[txt+"|"+n.String()] = st{n, txt}
		}
	}
	if rejected == 0 {
		return "", false
	}
	list := make([]st, 0, len(seen))
	for _, s := range seen {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n.Cmp(list[j].n) < 0 })
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = s.txt
	}
	text := strings.Join(parts, ",")
	if text == "" {
		text = "unknown"
	}
	return fmt.Sprintf("LinkedIn rejected batch elements (count=%d, statuses=%s)", rejected, text), true
}
