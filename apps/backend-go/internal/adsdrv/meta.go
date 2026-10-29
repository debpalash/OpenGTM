package adsdrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

func (c Client) syncMeta(ctx context.Context, wsID string, cfg map[string]any, rows []map[string]string, op, past string) (Result, error) {
	token, err := c.secret(ctx, wsID, "META_ACCESS_TOKEN")
	if err != nil {
		return Result{}, err
	}
	audienceID, err := cfgStr(cfg, "custom_audience_id")
	if err != nil {
		return Result{}, err
	}
	var keys []string // schema column order is email then phone
	for _, k := range []string{"email", "phone"} {
		for _, r := range rows {
			if _, ok := r[k]; ok {
				keys = append(keys, k)
				break
			}
		}
	}
	schema := make([]string, len(keys))
	for i, k := range keys {
		schema[i] = strings.ToUpper(k)
	}
	data := make([][]string, len(rows))
	for i, r := range rows {
		line := make([]string, len(keys))
		for j, k := range keys {
			line[j] = r[k]
		}
		data[i] = line
	}
	payload, err := json.Marshal(map[string]any{"schema": schema, "data": data})
	if err != nil {
		return Result{}, err
	}
	method := "POST"
	if op == "remove" {
		method = "DELETE"
	}
	target := c.Endpoints.Meta + "/" + seg(cfgGet(cfg, "api_version", "v23.0")) + "/" + seg(audienceID) + "/users"
	form := url.Values{"access_token": {token}, "payload": {string(payload)}}
	raw, err := c.do(ctx, method, target, nil, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return Result{}, err
	}
	body, ok := v.(map[string]any)
	if !ok {
		return Result{}, errBadJSON
	}
	ext := audienceID
	if s := body["session_id"]; pyTruthy(s) {
		ext = rowtmpl.PyStr(s)
	}
	return Result{Success: true, Summary: fmt.Sprintf("%s %d hashed users", past, len(rows)), ExternalID: ext}, nil
}
