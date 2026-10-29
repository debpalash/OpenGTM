package adsdrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

var jobNameRE = regexp.MustCompile(`^customers/[0-9]+/offlineUserDataJobs/[0-9]+$`)

func (c Client) syncGoogle(ctx context.Context, wsID string, cfg map[string]any, rows []map[string]string, op string) (Result, error) {
	token, err := c.secret(ctx, wsID, "GOOGLE_ADS_ACCESS_TOKEN")
	if err != nil {
		return Result{}, err
	}
	devToken, err := c.secret(ctx, wsID, "GOOGLE_ADS_DEVELOPER_TOKEN")
	if err != nil {
		return Result{}, err
	}
	customerID, err := cfgStr(cfg, "customer_id")
	if err != nil {
		return Result{}, err
	}
	userListID, err := cfgStr(cfg, "user_list_id")
	if err != nil {
		return Result{}, err
	}
	version := seg(cfgGet(cfg, "api_version", "v23"))
	root := c.Endpoints.Google + "/" + version
	hdr := map[string]string{"Authorization": "Bearer " + token, "developer-token": devToken}
	if l, ok := cfg["login_customer_id"]; ok && pyTruthy(l) {
		hdr["login-customer-id"] = rowtmpl.PyStr(l)
	}
	createBody, err := json.Marshal(map[string]any{"job": map[string]any{
		"type": "CUSTOMER_MATCH_USER_LIST",
		"customerMatchUserListMetadata": map[string]any{
			"userList": "customers/" + customerID + "/userLists/" + userListID,
			"consent":  map[string]any{"adUserData": "GRANTED", "adPersonalization": "GRANTED"},
		},
	}})
	if err != nil {
		return Result{}, err
	}
	raw, err := c.do(ctx, "POST", root+"/customers/"+seg(customerID)+"/offlineUserDataJobs:create", hdr, "application/json", createBody)
	if err != nil {
		return Result{}, err
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return Result{}, err
	}
	obj, _ := v.(map[string]any)
	jobName, _ := obj["resourceName"].(string)
	if jobName == "" {
		return Result{}, errors.New("Google Ads response missing resourceName")
	}
	// The job name becomes a URL path: accept only the documented resource shape so a hostile
	// response cannot steer the follow-up calls.
	if !jobNameRE.MatchString(jobName) {
		return Result{}, errors.New("Google Ads response has an invalid resourceName")
	}
	key := "create"
	if op == "remove" {
		key = "remove"
	}
	ops := make([]any, 0, len(rows))
	for _, r := range rows {
		ids := []any{}
		if r["email"] != "" {
			ids = append(ids, map[string]any{"hashedEmail": r["email"]})
		}
		if r["phone"] != "" {
			ids = append(ids, map[string]any{"hashedPhoneNumber": r["phone"]})
		}
		ops = append(ops, map[string]any{key: map[string]any{"userIdentifiers": ids}})
	}
	opsBody, err := json.Marshal(map[string]any{"enablePartialFailure": true, "operations": ops})
	if err != nil {
		return Result{}, err
	}
	raw, err = c.do(ctx, "POST", root+"/"+jobName+":addOperations", hdr, "application/json", opsBody)
	if err != nil {
		return Result{}, err
	}
	v, err = decodeJSON(raw)
	if err != nil {
		return Result{}, err
	}
	if msg, bad := googlePartialFailure(v); bad {
		// The accepted subset is deliberately not run: accounting is per batch, so the whole
		// batch stays retryable instead of being overstated.
		return Result{Error: msg, ExternalID: jobName}, nil
	}
	if _, err := c.do(ctx, "POST", root+"/"+jobName+":run", hdr, "application/json", []byte("{}")); err != nil {
		return Result{}, err
	}
	return Result{Success: true, Summary: fmt.Sprintf("queued %s for %d hashed users", op, len(rows)), ExternalID: jobName}, nil
}

// googlePartialFailure summarises rejected operations without provider-returned PII.
func googlePartialFailure(body any) (string, bool) {
	m, ok := body.(map[string]any)
	if !ok {
		return "", false
	}
	failure, ok := m["partialFailureError"].(map[string]any)
	if !ok || len(failure) == 0 {
		return "", false
	}
	rejected := 0
	pending := []any{failure["details"]}
	if failure["details"] == nil {
		pending = []any{[]any{}}
	}
	for len(pending) > 0 {
		v := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if list, isList := child.([]any); isList && k == "errors" {
					rejected += len(list)
				} else {
					pending = append(pending, child)
				}
			}
		case []any:
			pending = append(pending, t...)
		}
	}
	statusSrc := failure["status"]
	status := "PARTIAL_FAILURE"
	if pyTruthy(statusSrc) {
		status = rowtmpl.PyStr(statusSrc)
	}
	status = pyUpperASCII(status)
	if len(status) > 64 {
		status = status[:64]
	}
	code := "unknown"
	if n, isInt := pyInt(failure["code"]); isInt {
		code = n.String()
		if b, isBool := failure["code"].(bool); isBool {
			code = map[bool]string{true: "True", false: "False"}[b]
		}
	}
	count := "unknown"
	if rejected > 0 {
		count = fmt.Sprint(rejected)
	}
	return fmt.Sprintf("Google Ads rejected operations (status=%s, code=%s, count=%s)", status, code, count), true
}
