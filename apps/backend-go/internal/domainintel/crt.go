package domainintel

import (
	"context"
	"sort"
	"strings"
	"time"
)

const (
	crtMaxBytes = 8 << 20
	crtAttempts = 3
)

var interestingSubdomains = map[string]bool{"mail": true, "careers": true, "jobs": true, "hr": true, "recruit": true, "apply": true, "portal": true, "app": true, "shop": true, "store": true}

type crtInfo struct {
	Subdomains  []string
	Count       int
	Interesting []string
}

func (c crtInfo) obj() *Obj {
	o := NewObj()
	o.Set("subdomains", strs(c.Subdomains))
	o.Set("subdomain_count", c.Count)
	o.Set("interesting", strs(c.Interesting))
	return o
}

// crt is get_crtsh_subdomains: up to three attempts (crt.sh often answers 502 under load), then the lowercase
// subdomains of the domain found in the certificate names.
func (a *analyzer) crt(ctx context.Context, domain string) (crtInfo, error) {
	res := crtInfo{Subdomains: []string{}, Interesting: []string{}}
	u := "https://crt.sh/?q=%25." + domain + "&exclude=expired&deduplicate=Y&output=json"
	var rows any
	for attempt := 0; attempt < crtAttempts; attempt++ {
		status, body, err := a.get(ctx, u, map[string]string{"User-Agent": "Mozilla/5.0"}, crtTimeout, crtMaxBytes, sameHostOnly)
		if err == nil && status == 200 && pyStrip(string(body)) != "" {
			// Invalid JSON ends the whole lookup (Python's resp.json() raises into the outer except).
			if doc, ok := decodeJSON(body); ok {
				rows = doc
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
		if attempt < crtAttempts-1 {
			a.sleep(ctx, time.Duration(1500*(attempt+1))*time.Millisecond)
		}
	}
	if !pyTruthy(rows) {
		return res, nil
	}
	list, _ := rows.([]any)
	names := map[string]struct{}{}
	suffix := "." + domain
	for _, r := range list {
		row, ok := asObj(r)
		if !ok {
			continue // Python raises (500) on a non-object row; Go skips it and keeps the partial result (audit F9)
		}
		nv, _ := getOr(row, "name_value", "").(string) // str() of any other type can never end with the domain
		for _, line := range pySplitLines(nv) {
			line = strings.TrimLeft(pyLower(pyStrip(line)), "*.")
			if !strings.HasSuffix(line, suffix) || strings.Contains(line, " ") {
				continue
			}
			if first, _ := firstRune(line); first != 0 && pyIsDigit(first) {
				continue
			}
			names[line] = struct{}{}
		}
	}
	subs := make([]string, 0, len(names))
	for n := range names {
		subs = append(subs, n)
	}
	sort.Strings(subs)
	res.Subdomains = firstN(subs, 200)
	res.Count = len(subs)
	var inter []string
	for _, s := range subs {
		label, _, _ := strings.Cut(s, ".")
		if interestingSubdomains[label] {
			inter = append(inter, s)
		}
	}
	res.Interesting = firstN(append([]string{}, inter...), 25)
	return res, nil
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, 1
	}
	return 0, 0
}
