package collect

import "strings"

// urllib.parse.urlparse, the parts the legacy helpers read.

// cleanURL: urlsplit strips leading C0 controls and spaces and drops tab/CR/LF.
func cleanURL(u string) string {
	u = strings.TrimLeft(u, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	return strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
}

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

// urlParts is urlparse(u): scheme, netloc, path (params folded into the path's last segment
// split off as Python does only for the schemes that use them; the helpers here never need them).
type urlParts struct{ Scheme, Netloc, Path, Query, Fragment string }

func urlparse(u string) urlParts {
	u = cleanURL(u)
	var p urlParts
	if s, rest, ok := splitScheme(u); ok {
		p.Scheme, u = s, rest
	}
	if strings.HasPrefix(u, "//") {
		end := len(u)
		for _, c := range "/?#" {
			if i := strings.IndexRune(u[2:], c); i >= 0 && i+2 < end {
				end = i + 2
			}
		}
		p.Netloc, u = u[2:end], u[end:]
	}
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u, p.Fragment = u[:i], u[i+1:]
	}
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u, p.Query = u[:i], u[i+1:]
	}
	if usesParams[p.Scheme] {
		seg := u
		if j := strings.LastIndexByte(u, '/'); j >= 0 {
			seg = u[j:]
		}
		if i := strings.IndexByte(seg, ';'); i >= 0 {
			u = u[:len(u)-len(seg)+i]
		}
	}
	p.Path = u
	return p
}

var usesParams = map[string]bool{"": true, "ftp": true, "hdl": true, "prospero": true, "http": true, "imap": true,
	"https": true, "shttp": true, "rtsp": true, "rtspu": true, "sip": true, "sips": true, "mms": true, "sftp": true,
	"tel": true}
