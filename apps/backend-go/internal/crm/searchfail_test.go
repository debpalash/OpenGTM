package crm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A failed duplicate-check search must never fall through to a create.

type searchCase struct {
	name   string
	search func(r *http.Request) (*http.Response, error)
}

func resp(r *http.Request, code int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: code, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

func failingSearches() []searchCase {
	return []searchCase{
		{"5xx", func(r *http.Request) (*http.Response, error) { return resp(r, 503, `{"message":"down"}`) }},
		{"429 after retries", func(r *http.Request) (*http.Response, error) { return resp(r, 429, `{}`) }},
		{"malformed body", func(r *http.Request) (*http.Response, error) { return resp(r, 200, `<html>oops`) }},
		{"timeout", func(r *http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }},
		{"transport error", func(r *http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }},
	}
}

type recorder struct {
	mu    sync.Mutex
	posts []string
}

func (rc *recorder) add(r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.posts = append(rc.posts, r.Method+" "+r.URL.Path)
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestHubSpotSearchFailureDoesNotCreate(t *testing.T) {
	for _, tc := range failingSearches() {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			res, env := newResolver(t, st, nil)
			withWorkspaceToken(t, env, st, testWS, testToken)
			var rec recorder
			rt := rtFunc(func(r *http.Request) (*http.Response, error) {
				rec.add(r)
				if strings.HasSuffix(r.URL.Path, "/search") {
					return tc.search(r)
				}
				return resp(r, 201, `{"id":"99"}`)
			})
			h := &HubSpot{HTTP: &http.Client{Transport: rt}, Secrets: res, Sleep: noSleep}
			r := h.PushLeadAsContact(context.Background(), testWS, mapLead{"email": "a@b.co", "contact_person": "A B"}, nil)
			if r.Success || r.Error == "" {
				t.Fatalf("want failure, got %+v", r)
			}
			for _, p := range rec.posts {
				if p == "POST /crm/v3/objects/contacts" {
					t.Fatalf("create issued after failed search: %v", rec.posts)
				}
			}
		})
	}
}

func TestHubSpotEmptySearchCreates(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withWorkspaceToken(t, env, st, testWS, testToken)
	var rec recorder
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		rec.add(r)
		if strings.HasSuffix(r.URL.Path, "/search") {
			return resp(r, 200, `{"total":0,"results":[]}`)
		}
		return resp(r, 201, `{"id":"99"}`)
	})
	h := &HubSpot{HTTP: &http.Client{Transport: rt}, Secrets: res, Sleep: noSleep}
	r := h.PushLeadAsContact(context.Background(), testWS, mapLead{"email": "a@b.co", "contact_person": "A B"}, nil)
	if !r.Success || r.Action != "created" {
		t.Fatalf("got %+v", r)
	}
}

func TestSalesforceSearchFailureDoesNotCreate(t *testing.T) {
	for _, tc := range failingSearches() {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			res, env := newResolver(t, st, nil)
			withSFSecrets(t, env, st, testWS, sfInst, sfToken)
			var rec recorder
			rt := rtFunc(func(r *http.Request) (*http.Response, error) {
				rec.add(r)
				if r.Method == "GET" {
					return tc.search(r)
				}
				return resp(r, 201, `{"id":"00Q1"}`)
			})
			s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res, Sleep: noSleep}
			r := s.PushLeadAsContact(context.Background(), testWS, mapLead{"company": "Co", "email": "a@b.co", "contact_person": "A B"}, nil)
			if r.Success || r.Error == "" {
				t.Fatalf("want failure, got %+v", r)
			}
			for _, p := range rec.posts {
				if strings.HasPrefix(p, "POST ") {
					t.Fatalf("create issued after failed search: %v", rec.posts)
				}
			}
		})
	}
}

func TestSalesforceEmptySearchCreates(t *testing.T) {
	st := newStore()
	res, env := newResolver(t, st, nil)
	withSFSecrets(t, env, st, testWS, sfInst, sfToken)
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return resp(r, 200, `{"records":[]}`)
		}
		return resp(r, 201, `{"id":"00Q1"}`)
	})
	s := &Salesforce{HTTP: &http.Client{Transport: rt}, Secrets: res, Sleep: noSleep}
	r := s.PushLeadAsContact(context.Background(), testWS, mapLead{"company": "Co", "email": "a@b.co", "contact_person": "A B"}, nil)
	if !r.Success || r.Action != "created" {
		t.Fatalf("got %+v", r)
	}
}

// searchFixDeviation rewrites a recorded Python push transcript whose email
// search failed (non-200) and was followed by a create. Python treated that
// as "not found" and created a duplicate; Go stops at the failed search.
func searchFixDeviation(tr *transcript) {
	if tr.Call.Function != "push_lead_as_contact" || len(tr.Exchanges) < 2 {
		return
	}
	first := tr.Exchanges[0].Response
	if first.Status == 200 {
		return
	}
	prefix := "HubSpot contact search failed (retryable): "
	if strings.HasPrefix(tr.file, "salesforce__") {
		prefix = "Salesforce Lead search failed (retryable): "
	}
	tr.Exchanges = tr.Exchanges[:1]
	msg := prefix + "HTTP " + strconv.Itoa(first.Status) + ": " + first.Body
	b, _ := json.Marshal(map[string]any{"success": false, "error": msg})
	tr.Result = b
}
