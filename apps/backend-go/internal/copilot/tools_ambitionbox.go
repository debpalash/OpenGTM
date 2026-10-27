package copilot

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// AmbitionClient is the part of *ambitionbox.Client the AmbitionBox tools use (the client itself is
// parity-tested against Python in internal/ambitionbox; the tools only map arguments).
type AmbitionClient interface {
	SearchCompanies(ctx context.Context, p ambitionbox.SearchParams) (*ambitionbox.Obj, error)
	CollectCompaniesWith(ctx context.Context, p ambitionbox.CollectParams) (ambitionbox.Collection, error)
	GetCompanyJobs(ctx context.Context, companyID, page int) (*ambitionbox.Obj, error)
}

// pyIntConv is int(v): integers, floats truncated toward zero, bools and decimal strings.
func pyIntConv(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strings.TrimFunc(x, isPySpaceRune), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// abLimit is max(1, min(int(args.get("limit", 10)), 100)).
func abLimit(a map[string]any) (int, bool) {
	n := int64(10)
	if v, present := a["limit"]; present {
		var ok bool
		if n, ok = pyIntConv(v); !ok {
			return 0, false
		}
	}
	return int(max(1, min(n, 100))), true
}

// optStr is an optional string argument (absent or null -> "", isSet false).
func optStr(a map[string]any, key, dflt string) (s string, isSet, ok bool) {
	v, present := a[key]
	if !present {
		return dflt, dflt != "", true
	}
	switch x := v.(type) {
	case nil:
		return "", false, true
	case string:
		return x, true, true
	}
	return "", false, false
}

// abFailure is execute()'s generic failure text for an upstream error.
func abFailure(name string, err error) string { return toolErr(name, runePrefix(err.Error(), 300)) }

func (t *Turn) ambitionboxSearch(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "ambitionbox_search"
	cl := t.Engine.Ambition
	if cl == nil {
		return "", false, nil
	}
	limit, ok := abLimit(a)
	if !ok {
		return "", false, nil
	}
	var industry []string
	if v := a["industry"]; pyTruthy(v) {
		s, isStr := v.(string)
		if !isStr {
			return "", false, nil
		}
		industry = []string{s}
	}
	sortBy, _, ok1 := optStr(a, "sort_by", "popular")
	rating, _, ok2 := optStr(a, "rating", "")
	page, ok3 := argInt(a, "page", 1)
	if !ok1 || !ok2 || !ok3 {
		return "", false, nil
	}
	if limit > 20 {
		maxPages := min(25, (limit+19)/20+5)
		coll, err := cl.CollectCompaniesWith(ctx, ambitionbox.CollectParams{Requested: limit, MaxPages: maxPages,
			Industry: industry, SortBy: sortBy, Rating: rating})
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			return abFailure(name, err), true, nil
		}
		res := coll.Summary(false)
		companies := make([]any, len(coll.Records))
		for i, r := range coll.Records {
			companies[i] = r.Data
		}
		res.Set("companies", companies)
		res.Set("total", len(coll.Records))
		res.Set("page", 1)
		res.Set("requested_limit", limit)
		out, err := objJSON(res)
		return out, err == nil, err
	}
	res, err := cl.SearchCompanies(ctx, ambitionbox.SearchParams{Page: int(page), Limit: limit, SortBy: sortBy,
		Industry: industry, Rating: rating})
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return abFailure(name, err), true, nil
	}
	out, err := objJSON(res)
	return out, err == nil, err
}

func (t *Turn) ambitionboxJobs(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "ambitionbox_jobs"
	cl := t.Engine.Ambition
	if cl == nil {
		return "", false, nil
	}
	raw, present := a["company_id"]
	if !present {
		return keyErr(name, "company_id"), true, nil
	}
	id, ok := raw.(int64)
	page, pok := argInt(a, "page", 1)
	if !ok || !pok {
		return "", false, nil
	}
	res, err := cl.GetCompanyJobs(ctx, int(id), int(page))
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return abFailure(name, err), true, nil
	}
	out, err := objJSON(res)
	return out, err == nil, err
}

func (t *Turn) importAmbitionbox(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "import_ambitionbox_to_workbook"
	if t.Pool == nil {
		return "", false, nil
	}
	limit, ok := abLimit(a)
	if !ok {
		return "", false, nil
	}
	industry, _, ok1 := optStr(a, "industry", "")
	sortBy, _, ok2 := optStr(a, "sort_by", "popular")
	_, _, ok3 := optStr(a, "rating", "")
	if !ok1 || !ok2 || !ok3 {
		return "", false, nil
	}
	wbName := "AmbitionBox Companies"
	if industry != "" {
		wbName = "AmbitionBox — " + industry + " Companies"
	}
	if v := a["name"]; pyTruthy(v) {
		s, isStr := v.(string)
		if !isStr {
			return "", false, nil
		}
		wbName = s
	}
	p := ambitionimport.StartParams{WorkspaceID: t.WS, Name: wbName, SortBy: sortBy, Limit: limit}
	if s, isStr := a["industry"].(string); isStr {
		p.Industry = &s
	}
	if s, isStr := a["rating"].(string); isStr {
		p.Rating = &s
	}
	res, err := ambitionimport.Start(ctx, ambitionimport.Deps{Pool: t.Pool, Log: t.logger(), Executor: t.Engine.AmbitionImportExecutor}, p)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, err
	}
	o := wbcrud.NewObj()
	o.Set("workbook_id", res.WorkbookID)
	o.Set("name", res.Name)
	o.Set("run_id", res.RunID)
	o.Set("job_id", res.JobID)
	o.Set("status", res.Status)
	o.Set("requested_limit", res.RequestedLimit)
	o.Set("source", res.Source)
	o.Set("url", res.URL)
	o.Set("message", "Started a durable import of "+strconv.Itoa(limit)+" AmbitionBox companies into '"+res.Name+
		"'. Progress is checkpointed under run "+res.RunID+". Open at "+res.URL)
	out, err := objJSON(o)
	return out, err == nil, err
}
