package domainintel

import (
	"context"
	"net/url"
	"strings"
)

// Python dict order matters: the first pattern contained in a name wins.
var hostingProviders = []struct{ pattern, name string }{
	{"cloudflare", "Cloudflare"}, {"awsdns", "Amazon AWS"}, {"googledomains", "Google Domains"}, {"google.com", "Google Cloud"},
	{"azure-dns", "Microsoft Azure"}, {"digitalocean", "DigitalOcean"}, {"vercel-dns", "Vercel"}, {"netlify", "Netlify"},
	{"godaddy", "GoDaddy"}, {"namecheap", "Namecheap"}, {"hostgator", "HostGator"}, {"bluehost", "Bluehost"},
	{"squarespace", "Squarespace"}, {"wix", "Wix"}, {"shopify", "Shopify"},
}

var emailProviders = []struct{ pattern, name string }{
	{"google.com", "Google Workspace"}, {"googlemail.com", "Google Workspace"}, {"outlook.com", "Microsoft 365"},
	{"protection.outlook", "Microsoft 365"}, {"pphosted.com", "Proofpoint"}, {"mimecast", "Mimecast"}, {"zoho.com", "Zoho Mail"},
	{"mailgun", "Mailgun"}, {"sendgrid", "SendGrid"}, {"amazonaws", "Amazon SES"}, {"secureserver.net", "GoDaddy Email"},
	{"yahoodns", "Yahoo Mail"}, {"privateemail", "Namecheap Email"},
}

const (
	dohURL      = "https://cloudflare-dns.com/dns-query"
	dohMaxBytes = 1 << 20
)

type dnsInfo struct {
	A, MX, NS, TXT []string
	Hosting, Email string
	HasSPF         bool
	HasDMARC       bool
}

// query is get_dns_records.query_dns: the Answer data strings, [] on any failure. A non-string data value
// (Python would misbehave or raise later) makes the whole answer unusable here.
func (a *analyzer) query(ctx context.Context, name, typ string) []string {
	q := url.Values{"name": {name}, "type": {typ}}
	status, body, err := a.get(ctx, dohURL+"?"+q.Encode(), map[string]string{"Accept": "application/dns-json"}, dohTimeout, dohMaxBytes, sameHostOnly)
	if err != nil || status != 200 {
		return []string{}
	}
	doc, ok := decodeJSON(body)
	if !ok {
		return []string{}
	}
	obj, ok := asObj(doc)
	if !ok {
		return []string{}
	}
	ans, present := obj.Get("Answer")
	if !present {
		return []string{}
	}
	list, ok := ans.([]any) // any other shape: Python's iteration or .get raises and the query answers []
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		eo, ok := asObj(e)
		if !ok {
			return []string{}
		}
		v, present := eo.Get("data")
		if !present {
			out = append(out, "")
			continue
		}
		s, ok := v.(string)
		if !ok {
			return []string{}
		}
		out = append(out, s)
	}
	return out
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// buildDNS is the post-processing of get_dns_records over the five raw answers.
func buildDNS(aRec, mxRec, nsRec, txtRec, dmarc []string) dnsInfo {
	d := dnsInfo{A: firstN(aRec, 5), MX: []string{}, NS: []string{}, TXT: firstN(txtRec, 10)}
	for _, r := range firstN(mxRec, 5) {
		if _, rest, ok := strings.Cut(r, " "); ok {
			r = rest
		}
		d.MX = append(d.MX, r)
	}
	for _, r := range firstN(nsRec, 5) {
		d.NS = append(d.NS, strings.TrimRight(r, "."))
	}
	for _, ns := range d.NS {
		l := pyLower(ns)
		for _, p := range hostingProviders {
			if strings.Contains(l, p.pattern) {
				d.Hosting = p.name
				break
			}
		}
		if d.Hosting != "" {
			break
		}
	}
	for _, mx := range d.MX {
		l := pyLower(mx)
		for _, p := range emailProviders {
			if strings.Contains(l, p.pattern) {
				d.Email = p.name
				break
			}
		}
		if d.Email != "" {
			break
		}
	}
	for _, t := range txtRec {
		if strings.Contains(t, "v=spf1") {
			d.HasSPF = true
		}
	}
	d.HasDMARC = len(dmarc) > 0
	return d
}

func (d dnsInfo) obj() *Obj {
	o := NewObj()
	o.Set("a_records", strs(d.A))
	o.Set("mx_records", strs(d.MX))
	o.Set("ns_records", strs(d.NS))
	o.Set("txt_records", strs(d.TXT))
	o.Set("hosting_provider", d.Hosting)
	o.Set("email_provider", d.Email)
	o.Set("has_spf", d.HasSPF)
	o.Set("has_dmarc", d.HasDMARC)
	return o
}

func strs(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
