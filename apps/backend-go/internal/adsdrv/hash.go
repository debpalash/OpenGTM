// Package adsdrv drives the paid-media audience platforms (Meta, LinkedIn, Google Ads). Only
// normalised SHA-256 identifiers leave the system; credentials come from the workspace secret
// store and never appear in errors or logs. Behaviour mirrors legacy/python
// apps/api/services/destinations/ads.py.
package adsdrv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

// pyTruthy is Python truthiness for decoded JSON values (json.Number aware).
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return err != nil || f != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// orEmptyStr is str(v or "").
func orEmptyStr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return rowtmpl.PyStr(v)
}

// pySpace is str.isspace: unicode.IsSpace plus the four ASCII separators Python also counts.
func pySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func hashEmailAny(v any) string {
	var b strings.Builder
	for _, r := range orEmptyStr(v) {
		if !pySpace(r) {
			b.WriteRune(r)
		}
	}
	n := pyLower(b.String())
	if !strings.Contains(n, "@") {
		return ""
	}
	return sha(n)
}

func hashPhoneAny(v any) string {
	var b strings.Builder
	digits := 0
	for _, r := range orEmptyStr(v) {
		if unicode.IsDigit(r) { // re \d is Unicode Nd, as is unicode.IsDigit
			b.WriteRune(r)
			digits++
		}
	}
	if digits < 7 || digits > 15 {
		return ""
	}
	return sha(b.String())
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// HashEmail is hash_email: whitespace removed, lower-cased (Python full case mapping), SHA-256
// hex, empty unless the result contains "@".
func HashEmail(v string) string { return hashEmailAny(v) }

// HashPhone is hash_phone: digits only (Unicode digits count), SHA-256 hex, empty unless 7..15.
func HashPhone(v string) string { return hashPhoneAny(v) }

// HashedIdentifiers reads email from "email" and phone from "phone" then "mobile_phone".
func HashedIdentifiers(snapshot map[string]any) map[string]string {
	ids := map[string]string{}
	email := hashEmailAny(snapshot["email"])
	phoneSrc := snapshot["phone"]
	if !pyTruthy(phoneSrc) {
		phoneSrc = snapshot["mobile_phone"]
	}
	phone := hashPhoneAny(phoneSrc)
	if email != "" {
		ids["email"] = email
	}
	if phone != "" {
		ids["phone"] = phone
	}
	return ids
}

// IdentifiersSupported reports whether the platform can consume the identifier set.
func IdentifiersSupported(destType string, ids map[string]string) bool {
	if destType == "linkedin_ads" {
		return ids["email"] != ""
	}
	return ids["email"] != "" || ids["phone"] != ""
}

// pyLower is Python str.lower(): full case mapping, so U+0130 becomes "i" + U+0307 and a
// capital sigma at the end of a word becomes the final form.
func pyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i̇")
		case r == 0x3a3:
			if finalSigma(rs, i) {
				b.WriteRune(0x3c2)
			} else {
				b.WriteRune(0x3c3)
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func isCased(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }

func isCaseIgnorable(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	return strings.ContainsRune("'.:·‘’․‧︓﹒﹕＇．：", r)
}

func finalSigma(rs []rune, i int) bool {
	before := false
	for j := i - 1; j >= 0; j-- {
		if isCaseIgnorable(rs[j]) {
			continue
		}
		before = isCased(rs[j])
		break
	}
	if !before {
		return false
	}
	for j := i + 1; j < len(rs); j++ {
		if isCaseIgnorable(rs[j]) {
			continue
		}
		return !isCased(rs[j])
	}
	return true
}

var upperSpecial = map[rune]string{
	0xdf: "SS", 0x149: "ʼN", 0x1f0: "J̌", 0x1e96: "H̱", 0x1e97: "T̈", 0x1e98: "W̊",
	0x1e99: "Y̊", 0x1e9a: "Aʾ", 0xfb00: "FF", 0xfb01: "FI", 0xfb02: "FL", 0xfb03: "FFI",
	0xfb04: "FFL", 0xfb05: "ST", 0xfb06: "ST",
}

// pyUpperASCII is str.upper() followed by dropping everything outside [A-Z0-9_]; only the
// ASCII survivors matter, so the special full-case expansions are limited to those that
// produce ASCII letters.
func pyUpperASCII(s string) string {
	var b strings.Builder
	emit := func(r rune) {
		if r < utf8.RuneSelf && (r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			b.WriteRune(r)
		}
	}
	for _, r := range s {
		if sp, ok := upperSpecial[r]; ok {
			for _, x := range sp {
				emit(x)
			}
			continue
		}
		emit(unicode.ToUpper(r))
	}
	return b.String()
}
