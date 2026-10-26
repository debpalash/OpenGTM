package firmo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"encoding/xml"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const (
	secTickersURL     = "https://www.sec.gov/files/company_tickers.json"
	secFTSURL         = "https://efts.sec.gov/LATEST/search-index"
	secSubmissionsFmt = "https://data.sec.gov/submissions/CIK%s.json"
	secArchivesFmt    = "https://www.sec.gov/Archives/edgar/data/%d/%s/primary_doc.xml"
	secDefaultUA      = "OpenGTM Enrichment admin@yupcha.com"
	// secInterval spaces requests 125 ms apart (8 req/s), below SEC's
	// documented 10 req/s fair-access ceiling, process-wide.
	secInterval = 125 * time.Millisecond
)

var secLimiter = &limiter{interval: secInterval}

var (
	secSuffixRE = regexp.MustCompile(`(?i)\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|ltd|limited|co|company|plc|lp|holdings|group|the)\b`)
	secCIKTail  = regexp.MustCompile(`(?i)\s*\(cik\s+\d+\)\s*$`)
	secDateRE   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

func secNorm(name string) string {
	s := strings.ToLower(name)
	s = gleifNonAlnumRE.ReplaceAllString(s, " ")
	s = secSuffixRE.ReplaceAllString(s, " ")
	return providers.PyStrip(spacesRE.ReplaceAllString(s, " "))
}

// secEdgar ports providers/sec_edgar.py (SecEdgarProvider.enrich). The
// poller entry points resolve_cik and list_form_d_since belong to the intent
// poller (signal scan) and are not enrichment calls. Python follows
// redirects; Go never does (egress hardening). The SEC asks for a descriptive
// User-Agent: Input.Settings["SEC_EDGAR_USER_AGENT"] (global operator
// config) or the default.
type secEdgar struct{}

func (secEdgar) Name() string { return "sec_edgar" }

type secClient struct {
	c  *http.Client
	ua string
}

func (s secClient) do(ctx context.Context, url string, q [][2]string) (providers.Response, error) {
	if err := secLimiter.acquire(ctx); err != nil {
		return providers.Response{}, err
	}
	return providers.Do(ctx, s.c, providers.Req{Method: http.MethodGet, URL: url, Query: q,
		Header: [][2]string{{"User-Agent", s.ua}}})
}

// json is _get_json: non-200, transport and decode failures are None (nil,
// false); the raw body is returned for order-sensitive callers.
func (s secClient) json(ctx context.Context, url string, q [][2]string) (any, []byte, error) {
	resp, err := s.do(ctx, url, q)
	if cerr := ctx.Err(); cerr != nil {
		return nil, nil, cerr
	}
	if err != nil || resp.Status != 200 {
		return nil, nil, nil
	}
	d, perr := providers.ParseJSON(resp.Body)
	if perr != nil {
		return nil, nil, nil
	}
	return d, resp.Body, nil
}

func (s secClient) text(ctx context.Context, url string) (string, error) {
	resp, err := s.do(ctx, url, nil)
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	if err != nil || resp.Status != 200 {
		return "", nil
	}
	return strings.ToValidUTF8(string(resp.Body), "�"), nil
}

func (p secEdgar) Enrich(ctx context.Context, c *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	company := providers.PyStrip(lead(in, "company"))
	if company == "" {
		return fail("no_company"), nil
	}
	ua := secDefaultUA
	if s, ok := in.Settings["SEC_EDGAR_USER_AGENT"].(string); ok && s != "" {
		ua = s
	}
	out, err := p.run(ctx, secClient{c: c, ua: ua}, company, in)
	return finish(ctx, out, err, 120)
}

func (p secEdgar) run(ctx context.Context, s secClient, company string, in providers.Input) (providers.Output, error) {
	cik, err := p.resolveCIK(ctx, s, company)
	if err != nil {
		return providers.Output{}, err
	}
	if cik == "" {
		return fail("no_cik_match"), nil
	}
	acc, err := p.latestFormD(ctx, s, cik)
	if err != nil {
		return providers.Output{}, err
	}
	all := newFields()
	all.Set("sec_cik", cik)
	if acc != "" {
		n, _ := strconv.Atoi(cik)
		xml, err := s.text(ctx, fmt.Sprintf(secArchivesFmt, n, acc))
		if err != nil {
			return providers.Output{}, err
		}
		if xml != "" {
			fd := parseFormD(xml)
			for _, k := range fd.Keys() {
				v, _ := fd.Get(k)
				all.Set(k, v)
			}
		}
	}
	f := newFields()
	for _, k := range all.Keys() {
		v, _ := all.Get(k)
		if str, ok := v.(string); v == nil || (ok && str == "") {
			continue
		}
		f.Set(k, v)
	}
	if f.Len() <= 1 {
		o := fail("no_filing_data")
		o.Extra = f
		return o, nil
	}
	return success(in, f, 0.8), nil
}

func (p secEdgar) resolveCIK(ctx context.Context, s secClient, company string) (string, error) {
	cik, err := p.fromTickers(ctx, s, company)
	if err != nil || cik != "" {
		return cik, err
	}
	return p.fromFulltext(ctx, s, company)
}

func (p secEdgar) fromTickers(ctx context.Context, s secClient, company string) (string, error) {
	data, raw, err := s.json(ctx, secTickersURL, nil)
	if err != nil || !providers.Truthy(data) {
		return "", err
	}
	target := secNorm(company)
	if target == "" {
		return "", nil
	}
	m, ok := data.(map[string]any)
	if !ok {
		return "", &providers.PyException{Msg: fmt.Sprintf("'%s' object has no attribute 'values'", providers.TypeName(data))}
	}
	for _, row := range orderedValues(raw, m) {
		t, err := providers.PyGet(row, "title", "")
		if err != nil {
			return "", err
		}
		if secNorm(providers.Str(t)) == target {
			cs, _ := get(row, "cik_str")
			return padCIK(cs), nil
		}
	}
	return "", nil
}

// orderedValues is data.values() in document order (Go maps lose it; the
// first matching row wins in Python).
func orderedValues(raw []byte, m map[string]any) []any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return mapValues(m)
	}
	var keys []string
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return mapValues(m)
		}
		k, _ := kt.(string)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return mapValues(m)
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func mapValues(m map[string]any) []any {
	out := make([]any, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func (p secEdgar) fromFulltext(ctx context.Context, s secClient, company string) (string, error) {
	data, _, err := s.json(ctx, secFTSURL, [][2]string{{"q", `"` + company + `"`}, {"forms", "D"}})
	if err != nil || !providers.Truthy(data) {
		return "", err
	}
	hv, err := chain(data, "hits", "hits")
	if err != nil {
		return "", err
	}
	hits, err := elems(orEmptyList(hv))
	if err != nil {
		return "", err
	}
	target := secNorm(company)
	best := ""
	for _, hit := range hits {
		src, err := providers.PyGet(hit, "_source", map[string]any{})
		if err != nil {
			return "", err
		}
		src = orEmpty(src)
		ciksV, err := get(src, "ciks")
		if err != nil {
			return "", err
		}
		if !providers.Truthy(ciksV) {
			continue
		}
		ciks, err := indexable(ciksV)
		if err != nil {
			return "", err
		}
		namesV, err := get(src, "display_names")
		if err != nil {
			return "", err
		}
		names, err := elems(orEmptyList(namesV))
		if err != nil {
			return "", err
		}
		for _, nm := range names {
			str, ok := nm.(string)
			if !ok {
				return "", &providers.PyException{Msg: "expected string or bytes-like object, got '" + providers.TypeName(nm) + "'"}
			}
			if secNorm(secCIKTail.ReplaceAllString(str, "")) == target {
				return padCIK(ciks), nil
			}
		}
		if best == "" {
			best = padCIK(ciks)
		}
	}
	return best, nil
}

// indexable is ciks[0] for a truthy value (list element; first character of
// a string).
func indexable(v any) (any, error) { return indexFirst(v) }

func (p secEdgar) latestFormD(ctx context.Context, s secClient, cik string) (string, error) {
	data, _, err := s.json(ctx, fmt.Sprintf(secSubmissionsFmt, cik), nil)
	if err != nil || !providers.Truthy(data) {
		return "", err
	}
	recent, err := chain(data, "filings", "recent")
	if err != nil {
		return "", err
	}
	recent = orEmpty(recent)
	fv, err := get(recent, "form")
	if err != nil {
		return "", err
	}
	av, err := get(recent, "accessionNumber")
	if err != nil {
		return "", err
	}
	forms, err := elems(orEmptyList(fv))
	if err != nil {
		return "", err
	}
	accs, err := elems(orEmptyList(av))
	if err != nil {
		return "", err
	}
	for i := 0; i < len(forms) && i < len(accs); i++ {
		if forms[i] == "D" {
			a, ok := accs[i].(string)
			if !ok {
				return "", &providers.PyException{Msg: "'" + providers.TypeName(accs[i]) + "' object has no attribute 'replace'"}
			}
			return strings.ReplaceAll(a, "-", ""), nil
		}
	}
	return "", nil
}

// padCIK is _pad_cik: "" stands for None.
func padCIK(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		if t == "" {
			return ""
		}
		n, ok := pyInt(t)
		if !ok {
			return ""
		}
		return fmtCIK(n)
	case int64:
		return fmtCIK(t)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return ""
		}
		return fmtCIK(int64(math.Trunc(t)))
	case bool:
		if t {
			return fmtCIK(1)
		}
		return fmtCIK(0)
	}
	return ""
}

func fmtCIK(n int64) string { return fmt.Sprintf("%010d", n) }

// pyInt is int(str): optional sign, digits with single underscores between
// them, surrounding whitespace allowed.
func pyInt(s string) (int64, bool) {
	s = providers.PyStrip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// ── Form D XML ────────────────────────────────────────────────────────────

type xnode struct {
	local    string
	text     string
	children []*xnode
}

// iter is Element.iter(): the node itself, then descendants in document order.
func (n *xnode) iter(fn func(*xnode)) {
	fn(n)
	for _, c := range n.children {
		c.iter(fn)
	}
}

func (n *xnode) find(name string) *xnode {
	if n == nil {
		return nil
	}
	var hit *xnode
	n.iter(func(x *xnode) {
		if hit == nil && x.local == name {
			hit = x
		}
	})
	return hit
}

// text is _text: the stripped text before the first child, "" when absent.
func (n *xnode) textOf(name string) string {
	node := n.find(name)
	if node == nil || node.text == "" {
		return ""
	}
	return providers.PyStrip(node.text)
}

// parseXML builds a tree like ElementTree.fromstring; any error (including
// junk after the root element) is reported.
func parseXML(s string) (*xnode, error) {
	dec := xml.NewDecoder(strings.NewReader(s))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var root *xnode
	var stack []*xnode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{local: t.Name.Local}
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("junk after document element")
				}
				root = n
			} else {
				top := stack[len(stack)-1]
				top.children = append(top.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(t)) > 0 {
					return nil, fmt.Errorf("junk after document element")
				}
				continue
			}
			top := stack[len(stack)-1]
			if len(top.children) == 0 {
				top.text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no element found")
	}
	return root, nil
}

func asciiDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseFormD is _parse_form_d: {} on any parse failure.
func parseFormD(xmlText string) *fields {
	out := newFields()
	root, err := parseXML(xmlText)
	if err != nil {
		return out
	}
	amount := root.textOf("totalAmountSold")
	if amount == "" {
		amount = root.textOf("totalOfferingAmount")
	}
	if asciiDigits(amount) {
		trimmed := strings.TrimLeft(amount, "0")
		if trimmed != "" {
			if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
				out.Set("funding_amount", n)
			} else {
				out.Set("funding_amount", json.Number(trimmed))
			}
		}
	}
	dateVal := ""
	if dof := root.find("dateOfFirstSale"); dof != nil {
		dateVal = dof.textOf("value")
	}
	if dateVal == "" {
		dateVal = root.textOf("dateOfFirstSale")
	}
	if m := secDateRE.FindString(dateVal); m != "" {
		out.Set("funding_date", m)
	}
	if ind := root.textOf("industryGroupType"); ind != "" && strings.ToLower(ind) != "other" {
		out.Set("industry", ind)
	}
	out.Set("funding_stage_signal", "private_placement_form_d")
	if dms := relatedPersons(root); len(dms) > 0 {
		out.Set("decision_makers", dumps(dms))
	}
	return out
}

func relatedPersons(root *xnode) []jo {
	var people []jo
	rpl := root.find("relatedPersonsList")
	if rpl == nil {
		return nil
	}
	rpl.iter(func(info *xnode) {
		if info.local != "relatedPersonInfo" {
			return
		}
		nameNode := info.find("relatedPersonName")
		var parts []string
		for _, k := range []string{"firstName", "middleName", "lastName"} {
			p := nameNode.textOf(k)
			if p != "" && strings.ToUpper(p) != "N/A" {
				parts = append(parts, p)
			}
		}
		name := providers.PyStrip(strings.Join(parts, " "))
		if name == "" {
			return
		}
		var rels []string
		seen := map[string]bool{}
		info.iter(func(r *xnode) {
			if r.local == "relationship" && providers.PyStrip(r.text) != "" {
				t := providers.PyStrip(r.text)
				if !seen[t] {
					seen[t] = true
					rels = append(rels, t)
				}
			}
		})
		people = append(people, jo{{"name", name}, {"title", strings.Join(rels, ", ")}, {"source", "sec_form_d"}})
	})
	return people
}
