package firmo

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// This file is the intent poller's entry into the SEC EDGAR adapter
// (SecEdgarProvider.resolve_cik / list_form_d_since). Plan 08 Task 11 owns it;
// it reuses secClient (rate limiter, SEC User-Agent, hop-vetted redirects,
// 1 MiB body bound) so every poller fetch goes through the same egress client.

// SecFiling is one Form D / D-A filing (FormDFiling). Accession is dashless.
// Fields is the parsed Form D scalar map (funding_amount is int64 or
// json.Number). RelatedPersons carry name, title and source.
type SecFiling struct {
	CIK            string
	Accession      string
	Form           string
	FilingDate     string
	Fields         map[string]any
	RelatedPersons []map[string]string
}

var secBareCIK = regexp.MustCompile(`^[0-9]{1,10}$`)

// SecResolveCIK is SecEdgarProvider.resolve_cik: a "sec_cik:<n>" literal, a bare
// CIK, or a company name resolved over the network. "" means None.
func SecResolveCIK(ctx context.Context, c *http.Client, ua, target string) (string, error) {
	return secEdgar{}.resolvePublic(ctx, secClient{c: c, ua: secUA(ua)}, target)
}

func secUA(ua string) string {
	if ua == "" {
		return secDefaultUA
	}
	return ua
}

func (p secEdgar) resolvePublic(ctx context.Context, s secClient, target string) (string, error) {
	t := providers.PyStrip(target)
	if t == "" {
		return "", nil
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "sec_cik:") {
		return padCIK(providers.PyStrip(low[strings.Index(low, ":")+1:])), nil
	}
	if secBareCIK.MatchString(t) {
		return padCIK(t), nil
	}
	cik, err := p.resolveCIK(ctx, s, t)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", nil // Python: the caller's blanket except degrades to no filings
	}
	return cik, nil
}

// SecListFormDSince is list_form_d_since: every D / D/A filing strictly newer
// than since (dashes ignored, newest first, stops at the cursor). It never
// fails on vendor errors: a 5xx or parse problem degrades to the filings
// collected so far (Python returns out). Only a cancelled context is an error.
func SecListFormDSince(ctx context.Context, c *http.Client, ua, target, since string) ([]SecFiling, error) {
	s := secClient{c: c, ua: secUA(ua)}
	p := secEdgar{}
	cik, err := p.resolvePublic(ctx, s, target)
	if err != nil || cik == "" {
		return nil, err
	}
	cursor := strings.ReplaceAll(since, "-", "")
	var out []SecFiling
	data, _, err := s.json(ctx, fmt.Sprintf(secSubmissionsFmt, cik), nil)
	if err != nil || !providers.Truthy(data) {
		return nil, err
	}
	recent, perr := chain(data, "filings", "recent")
	if perr != nil {
		return nil, nil
	}
	recent = orEmpty(recent)
	var lists [3][]any
	for i, k := range []string{"form", "accessionNumber", "filingDate"} {
		v, gerr := get(recent, k)
		if gerr != nil {
			return nil, nil
		}
		l, eerr := elems(orEmptyList(v))
		if eerr != nil {
			return nil, nil
		}
		lists[i] = l
	}
	forms, accs, dates := lists[0], lists[1], lists[2]
	n, _ := pyInt(cik)
	for i, form := range forms {
		if form != "D" && form != "D/A" {
			continue
		}
		var accRaw any
		if i < len(accs) {
			accRaw = accs[i]
		}
		var acc string
		if providers.Truthy(accRaw) {
			a, ok := accRaw.(string)
			if !ok {
				return out, nil // AttributeError in Python: partial list
			}
			acc = strings.ReplaceAll(a, "-", "")
		}
		if acc == "" {
			continue
		}
		if cursor != "" && acc <= cursor {
			break
		}
		fdate := ""
		if i < len(dates) {
			if ds, ok := dates[i].(string); ok {
				fdate = ds
			}
		}
		xml, terr := s.text(ctx, fmt.Sprintf(secArchivesFmt, n, acc))
		if terr != nil {
			return nil, terr
		}
		f := SecFiling{CIK: cik, Accession: acc, Form: form.(string), FilingDate: fdate, Fields: map[string]any{}}
		if xml != "" {
			fd := parseFormD(xml)
			for _, k := range fd.Keys() {
				v, _ := fd.Get(k)
				f.Fields[k] = v
			}
			if root, xerr := parseXML(xml); xerr == nil {
				for _, person := range relatedPersons(root) {
					m := map[string]string{}
					for _, kv := range person {
						if sv, ok := kv.v.(string); ok {
							m[kv.k] = sv
						}
					}
					f.RelatedPersons = append(f.RelatedPersons, m)
				}
			}
		}
		out = append(out, f)
	}
	return out, nil
}
