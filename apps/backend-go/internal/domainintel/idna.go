package domainintel

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var errIDNA = errors.New("idna: label cannot be encoded")

// idnaEncode is str.encode("idna") (Python's IDNA 2003 codec: nameprep, punycode, 63-byte labels), erroring where
// Python raises UnicodeError.
func idnaEncode(host string) (string, error) {
	if host == "" {
		return "", nil
	}
	if isASCII(host) {
		labels := strings.Split(host, ".")
		for _, l := range labels[:len(labels)-1] {
			if len(l) == 0 || len(l) >= 64 {
				return "", errIDNA
			}
		}
		if len(labels[len(labels)-1]) >= 64 {
			return "", errIDNA
		}
		return host, nil
	}
	labels := splitIDNADots(host)
	trailing := ""
	if len(labels) > 0 && labels[len(labels)-1] == "" {
		trailing = "."
		labels = labels[:len(labels)-1]
	}
	var out strings.Builder
	for i, l := range labels {
		if i > 0 {
			out.WriteByte('.')
		}
		a, err := toASCII(l)
		if err != nil {
			return "", err
		}
		out.WriteString(a)
	}
	return out.String() + trailing, nil
}

// splitIDNADots splits on the four IDNA label separators (re.compile("[.。．｡]").split).
func splitIDNADots(s string) []string {
	return splitDots(s)
}

func toASCII(label string) (string, error) {
	if isASCII(label) {
		if len(label) > 0 && len(label) < 64 {
			return label, nil
		}
		return "", errIDNA
	}
	label, err := nameprep(label)
	if err != nil {
		return "", err
	}
	if isASCII(label) {
		if len(label) > 0 && len(label) < 64 {
			return label, nil
		}
		return "", errIDNA
	}
	if strings.HasPrefix(label, "xn--") {
		return "", errIDNA
	}
	out := "xn--" + punycodeEncode(label)
	if len(out) < 64 {
		return out, nil
	}
	return "", errIDNA
}

func mapB2(r rune) (string, bool) {
	i := sort.Search(len(tabB2), func(i int) bool { return tabB2[i].from >= r })
	if i < len(tabB2) && tabB2[i].from == r {
		return tabB2[i].to, true
	}
	return "", false
}

// nameprep is encodings.idna.nameprep (RFC 3491 over the RFC 3454 tables, Unicode 3.2 as in stringprep).
func nameprep(label string) (string, error) {
	var b strings.Builder
	for _, r := range label {
		if inSpans(tabB1, r) {
			continue
		}
		if m, ok := mapB2(r); ok {
			b.WriteString(m)
		} else {
			b.WriteRune(r)
		}
	}
	label = norm.NFKC.String(b.String())
	rs := []rune(label)
	for _, r := range rs {
		if inSpans(tabC12, r) || inSpans(tabC22, r) || inSpans(tabC3, r) || inSpans(tabC4, r) || inSpans(tabC5, r) ||
			inSpans(tabC6, r) || inSpans(tabC7, r) || inSpans(tabC8, r) || inSpans(tabC9, r) {
			return "", errIDNA
		}
	}
	randAL := make([]bool, len(rs))
	any := false
	for i, r := range rs {
		randAL[i] = inSpans(tabD1, r)
		any = any || randAL[i]
	}
	if any {
		for _, r := range rs {
			if inSpans(tabD2, r) {
				return "", errIDNA
			}
		}
		if !randAL[0] || !randAL[len(rs)-1] {
			return "", errIDNA
		}
	}
	return label, nil
}

const (
	pcBase        = 36
	pcTMin        = 1
	pcTMax        = 26
	pcSkew        = 38
	pcDamp        = 700
	pcInitialBias = 72
	pcInitialN    = 128
)

func pcAdapt(delta, numPoints int, first bool) int {
	if first {
		delta /= pcDamp
	} else {
		delta /= 2
	}
	delta += delta / numPoints
	k := 0
	for delta > ((pcBase-pcTMin)*pcTMax)/2 {
		delta /= pcBase - pcTMin
		k += pcBase
	}
	return k + (pcBase-pcTMin+1)*delta/(delta+pcSkew)
}

func pcDigit(d int) byte {
	if d < 26 {
		return byte('a' + d)
	}
	return byte('0' + d - 26)
}

// punycodeEncode is RFC 3492 encoding of the label (Python's "punycode" codec: basic characters, "-", extended).
func punycodeEncode(s string) string {
	rs := []rune(s)
	var out []byte
	for _, r := range rs {
		if r < 128 {
			out = append(out, byte(r))
		}
	}
	h := len(out)
	b := h
	if b > 0 {
		out = append(out, '-')
	}
	n, delta, bias := pcInitialN, 0, pcInitialBias
	for h < len(rs) {
		m := int(utf8.MaxRune) + 1
		for _, r := range rs {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range rs {
			if int(r) < n {
				delta++
			}
			if int(r) == n {
				q := delta
				for k := pcBase; ; k += pcBase {
					t := k - bias
					if t < pcTMin {
						t = pcTMin
					} else if t > pcTMax {
						t = pcTMax
					}
					if q < t {
						break
					}
					out = append(out, pcDigit(t+(q-t)%(pcBase-t)))
					q = (q - t) / (pcBase - t)
				}
				out = append(out, pcDigit(q))
				bias = pcAdapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return string(out)
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if r == '.' || r == '。' || r == '．' || r == '｡' {
			out = append(out, s[start:i])
			start = i + utf8.RuneLen(r)
		}
	}
	return append(out, s[start:])
}
