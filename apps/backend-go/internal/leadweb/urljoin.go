package leadweb

import "strings"

// urljoin ports CPython 3.13 urllib.parse.urljoin for http(s) base URLs (the
// only bases used here), so resolved links are byte-identical to legacy: no
// percent-encoding or normalisation beyond what Python does.
func urljoin(base, ref string) string {
	if base == "" {
		return ref
	}
	if ref == "" {
		return base
	}
	bs, bn, bp, bpar, bq, _ := urlparse6(base, "")
	s, n, p, par, q, f := urlparse6(ref, bs)
	if s != bs || !usesRelative[s] {
		return ref
	}
	if usesNetloc[s] {
		if n != "" {
			return urlunparse6(s, n, p, par, q, f)
		}
		n = bn
	}
	if p == "" && par == "" {
		p, par = bp, bpar
		if q == "" {
			q = bq
		}
		return urlunparse6(s, n, p, par, q, f)
	}
	baseParts := strings.Split(bp, "/")
	if baseParts[len(baseParts)-1] != "" {
		baseParts = baseParts[:len(baseParts)-1]
	}
	var segs []string
	if strings.HasPrefix(p, "/") {
		segs = strings.Split(p, "/")
	} else {
		segs = append(append([]string{}, baseParts...), strings.Split(p, "/")...)
		if len(segs) > 2 {
			mid := segs[1 : len(segs)-1]
			kept := []string{segs[0]}
			for _, x := range mid {
				if x != "" {
					kept = append(kept, x)
				}
			}
			segs = append(kept, segs[len(segs)-1])
		}
	}
	var resolved []string
	for _, seg := range segs {
		switch seg {
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
		case ".":
		default:
			resolved = append(resolved, seg)
		}
	}
	if last := segs[len(segs)-1]; last == "." || last == ".." {
		resolved = append(resolved, "")
	}
	path := strings.Join(resolved, "/")
	if path == "" {
		path = "/"
	}
	return urlunparse6(s, n, path, par, q, f)
}

var (
	usesRelative = set3("", "ftp", "http", "gopher", "nntp", "imap", "wais", "file", "https", "shttp", "mms",
		"prospero", "rtsp", "rtsps", "rtspu", "sftp", "svn", "svn+ssh", "ws", "wss")
	usesNetloc = set3("", "ftp", "http", "gopher", "nntp", "telnet", "imap", "wais", "file", "mms", "https", "shttp",
		"snews", "prospero", "rtsp", "rtsps", "rtspu", "rsync", "svn", "svn+ssh", "sftp", "nfs", "git", "git+ssh",
		"ws", "wss", "itms-services")
	usesParams = set3("", "ftp", "hdl", "prospero", "http", "imap", "https", "shttp", "rtsp", "rtsps", "rtspu",
		"sip", "sips", "mms", "sftp", "tel")
)

func set3(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// cleanURL: urlsplit strips leading C0 controls and spaces and drops tab/CR/LF.
func cleanURL(u string) string {
	u = strings.TrimLeft(u, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	return strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
}

// splitScheme is urlsplit's scheme detection on a cleaned URL.
func splitScheme(u string) (scheme, rest string, ok bool) {
	i := strings.IndexByte(u, ':')
	if i <= 0 {
		return "", u, false
	}
	c0 := u[0]
	if !(c0 >= 'a' && c0 <= 'z' || c0 >= 'A' && c0 <= 'Z') {
		return "", u, false
	}
	for _, c := range u[:i] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return "", u, false
		}
	}
	return strings.ToLower(u[:i]), u[i+1:], true
}

func urlparse6(u, defScheme string) (scheme, netloc, path, params, query, fragment string) {
	u = cleanURL(u)
	scheme = defScheme
	if s, rest, ok := splitScheme(u); ok {
		scheme, u = s, rest
	}
	if strings.HasPrefix(u, "//") {
		end := len(u)
		for _, c := range "/?#" {
			if i := strings.IndexRune(u[2:], c); i >= 0 && i+2 < end {
				end = i + 2
			}
		}
		netloc, u = u[2:end], u[end:]
	}
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u, fragment = u[:i], u[i+1:]
	}
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u, query = u[:i], u[i+1:]
	}
	if usesParams[scheme] && strings.Contains(u, ";") {
		var i int
		if j := strings.LastIndexByte(u, '/'); j >= 0 {
			i = strings.IndexByte(u[j:], ';')
			if i >= 0 {
				i += j
			}
		} else {
			i = strings.IndexByte(u, ';')
		}
		if i >= 0 {
			u, params = u[:i], u[i+1:]
		}
	}
	return scheme, netloc, u, params, query, fragment
}

func urlunparse6(scheme, netloc, path, params, query, fragment string) string {
	u := path
	if params != "" {
		u += ";" + params
	}
	switch {
	case netloc != "":
		if u != "" && !strings.HasPrefix(u, "/") {
			u = "/" + u
		}
		u = "//" + netloc + u
	case strings.HasPrefix(u, "//"):
		u = "//" + u
	case scheme != "" && usesNetloc[scheme] && (u == "" || strings.HasPrefix(u, "/")):
		u = "//" + u
	}
	if scheme != "" {
		u = scheme + ":" + u
	}
	if query != "" {
		u += "?" + query
	}
	if fragment != "" {
		u += "#" + fragment
	}
	return u
}
