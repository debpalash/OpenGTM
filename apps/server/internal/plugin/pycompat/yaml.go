package pycompat

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// YAMLError is returned when PyYAML's safe_load would raise.
type YAMLError struct{ Msg string }

func (e *YAMLError) Error() string { return e.Msg }

func yerr(format string, args ...any) error { return &YAMLError{Msg: fmt.Sprintf(format, args...)} }

// PyYAML implicit resolvers (yaml/resolver.py), checked in registration order.
var (
	reBool  = regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)
	reFloat = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?` +
		`|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?` +
		`|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*` +
		`|[-+]?\.(?:inf|Inf|INF)` +
		`|\.(?:nan|NaN|NAN))$`)
	reInt = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+` +
		`|[-+]?0[0-7_]+` +
		`|[-+]?(?:0|[1-9][0-9_]*)` +
		`|[-+]?0x[0-9a-fA-F_]+` +
		`|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	reMerge     = regexp.MustCompile(`^(?:<<)$`)
	reNull      = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	reTimestamp = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]` +
		`|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?` +
		`(?:[Tt]|[ \t]+)[0-9][0-9]?` +
		`:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?` +
		`(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
	reValue = regexp.MustCompile(`^(?:=)$`)

	reTimestampParts = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)` +
		`(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?` +
		`(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)

	// PyYAML's Reader rejects these code points anywhere in the stream.
	reNonPrintable = regexp.MustCompile("[^\x09\x0A\x0D\x20-\x7E\u0085 -퟿-�\U00010000-\U0010FFFF]")
)

const (
	tagNull      = "!!null"
	tagBool      = "!!bool"
	tagInt       = "!!int"
	tagFloat     = "!!float"
	tagStr       = "!!str"
	tagTimestamp = "!!timestamp"
	tagMerge     = "!!merge"
	tagValue     = "!!value"
	tagBinary    = "!!binary"
	tagMap       = "!!map"
	tagSeq       = "!!seq"
	tagSet       = "!!set"
	tagOmap      = "!!omap"
	tagPairs     = "!!pairs"
)

func resolvePlain(v string) string {
	switch {
	case reBool.MatchString(v):
		return tagBool
	case reFloat.MatchString(v):
		return tagFloat
	case reInt.MatchString(v):
		return tagInt
	case reMerge.MatchString(v):
		return tagMerge
	case reNull.MatchString(v):
		return tagNull
	case reTimestamp.MatchString(v):
		return tagTimestamp
	case reValue.MatchString(v):
		return tagValue
	}
	return tagStr
}

// LoadYAML parses a single YAML document exactly like yaml.safe_load on the
// UTF-8 decoded text. An empty stream yields nil.
func LoadYAML(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, yerr("'utf-8' codec can't decode the manifest: invalid UTF-8")
	}
	if loc := reNonPrintable.FindIndex(data); loc != nil {
		r, _ := utf8.DecodeRune(data[loc[0]:])
		return nil, yerr("unacceptable character #x%04x: special characters are not allowed", r)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, yerr("%s", err.Error())
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, yerr("expected a single document in the stream but found another document")
	} else if !errors.Is(err, io.EOF) {
		return nil, yerr("%s", err.Error())
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	c := &constructor{done: map[*yaml.Node]any{}, active: map[*yaml.Node]bool{}}
	return c.construct(doc.Content[0])
}

type constructor struct {
	done   map[*yaml.Node]any
	active map[*yaml.Node]bool
}

func nodeTag(n *yaml.Node) string {
	if n.Style&yaml.TaggedStyle != 0 && n.Tag != "" {
		t := n.Tag
		if strings.HasPrefix(t, "tag:yaml.org,2002:") {
			t = "!!" + strings.TrimPrefix(t, "tag:yaml.org,2002:")
		}
		return t
	}
	switch n.Kind {
	case yaml.MappingNode:
		return tagMap
	case yaml.SequenceNode:
		return tagSeq
	case yaml.ScalarNode:
		if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return tagStr
		}
		return resolvePlain(n.Value)
	}
	return ""
}

func (c *constructor) construct(n *yaml.Node) (any, error) {
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return nil, yerr("found undefined alias")
		}
		return c.construct(n.Alias)
	}
	if v, ok := c.done[n]; ok {
		return v, nil
	}
	if c.active[n] {
		return nil, yerr("found unconstructable recursive node")
	}
	c.active[n] = true
	defer delete(c.active, n)
	v, err := c.constructNode(n)
	if err != nil {
		return nil, err
	}
	c.done[n] = v
	return v, nil
}

func (c *constructor) constructNode(n *yaml.Node) (any, error) {
	tag := nodeTag(n)
	switch n.Kind {
	case yaml.ScalarNode:
		return constructScalar(tag, n.Value, n)
	case yaml.SequenceNode:
		switch tag {
		case tagSeq:
			out := make([]any, 0, len(n.Content))
			for _, e := range n.Content {
				v, err := c.construct(e)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		case tagOmap, tagPairs:
			out := make([]any, 0, len(n.Content))
			for _, e := range n.Content {
				e = deref(e)
				if e.Kind != yaml.MappingNode || len(e.Content) != 2 {
					return nil, yerr("while constructing an ordered map: expected a single mapping item")
				}
				k, err := c.construct(e.Content[0])
				if err != nil {
					return nil, err
				}
				v, err := c.construct(e.Content[1])
				if err != nil {
					return nil, err
				}
				out = append(out, Tuple{k, v})
			}
			return out, nil
		}
	case yaml.MappingNode:
		switch tag {
		case tagMap:
			return c.constructMapping(n)
		case tagSet:
			m, err := c.constructMapping(n)
			if err != nil {
				return nil, err
			}
			return Set(m.Keys()), nil
		}
	}
	return nil, yerr("could not determine a constructor for the tag '%s'", longTag(tag))
}

func deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func longTag(t string) string {
	if strings.HasPrefix(t, "!!") {
		return "tag:yaml.org,2002:" + t[2:]
	}
	return t
}

type kv struct{ k, v *yaml.Node }

// flatten mirrors SafeConstructor.flatten_mapping (merge keys).
func (c *constructor) flatten(n *yaml.Node) ([]kv, error) {
	var merge, own []kv
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if deref(k).Kind == yaml.ScalarNode && nodeTag(deref(k)) == tagMerge {
			v = deref(v)
			switch v.Kind {
			case yaml.MappingNode:
				sub, err := c.flatten(v)
				if err != nil {
					return nil, err
				}
				merge = append(merge, sub...)
			case yaml.SequenceNode:
				var subs [][]kv
				for _, s := range v.Content {
					s = deref(s)
					if s.Kind != yaml.MappingNode {
						return nil, yerr("while constructing a mapping: expected a mapping for merging, but found %s", kindName(s))
					}
					sub, err := c.flatten(s)
					if err != nil {
						return nil, err
					}
					subs = append(subs, sub)
				}
				for i := len(subs) - 1; i >= 0; i-- {
					merge = append(merge, subs[i]...)
				}
			default:
				return nil, yerr("while constructing a mapping: expected a mapping or list of mappings for merging, but found %s", kindName(v))
			}
			continue
		}
		own = append(own, kv{k, v})
	}
	return append(merge, own...), nil
}

func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	}
	return "node"
}

func (c *constructor) constructMapping(n *yaml.Node) (*Map, error) {
	pairs, err := c.flatten(n)
	if err != nil {
		return nil, err
	}
	m := NewMap()
	for _, p := range pairs {
		var key any
		kn := deref(p.k)
		if kn.Kind == yaml.ScalarNode && nodeTag(kn) == tagValue {
			key = kn.Value // flatten_mapping retags '=' keys as str
		} else {
			key, err = c.construct(p.k)
			if err != nil {
				return nil, err
			}
		}
		switch key.(type) {
		case []any, *Map, Set, Tuple:
			return nil, yerr("while constructing a mapping: found unhashable key")
		}
		val, err := c.construct(p.v)
		if err != nil {
			return nil, err
		}
		if err := m.Set(key, val); err != nil {
			return nil, yerr("while constructing a mapping: found unhashable key")
		}
	}
	return m, nil
}

var boolValues = map[string]bool{"yes": true, "no": false, "true": true, "false": false, "on": true, "off": false}

func constructScalar(tag, value string, n *yaml.Node) (any, error) {
	switch tag {
	case tagStr:
		return value, nil
	case tagNull:
		return nil, nil
	case tagBool:
		b, ok := boolValues[strings.ToLower(value)]
		if !ok {
			return nil, yerr("invalid boolean %q", value)
		}
		return b, nil
	case tagInt:
		return constructInt(value)
	case tagFloat:
		return constructFloat(value)
	case tagTimestamp:
		return constructTimestamp(value)
	case tagBinary:
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, value)
		b, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, yerr("failed to decode base64 data: %v", err)
		}
		return Bytes(b), nil
	case tagMerge:
		return nil, yerr("could not determine a constructor for the tag 'tag:yaml.org,2002:merge'")
	}
	return nil, yerr("could not determine a constructor for the tag '%s'", longTag(tag))
}

func pyIntParse(s string, base int) (any, error) {
	if s == "" {
		return nil, yerr("invalid literal for int() with base %d: ''", base)
	}
	b, ok := new(big.Int).SetString(s, base)
	if !ok || strings.ContainsAny(s, "+-") {
		return nil, yerr("invalid literal for int() with base %d: '%s'", base, s)
	}
	return normInt(b), nil
}

func normInt(b *big.Int) any {
	if b.IsInt64() {
		return b.Int64()
	}
	return b
}

func constructInt(raw string) (any, error) {
	v := strings.ReplaceAll(raw, "_", "")
	if v == "" {
		return nil, yerr("invalid int %q", raw)
	}
	neg := v[0] == '-'
	if v[0] == '-' || v[0] == '+' {
		v = v[1:]
	}
	var (
		out any
		err error
	)
	switch {
	case v == "0":
		return int64(0), nil
	case strings.HasPrefix(v, "0b"):
		out, err = pyIntParse(v[2:], 2)
	case strings.HasPrefix(v, "0x"):
		out, err = pyIntParse(v[2:], 16)
	case strings.HasPrefix(v, "0"):
		out, err = pyIntParse(v, 8)
	case strings.Contains(v, ":"):
		total := new(big.Int)
		base := big.NewInt(1)
		parts := strings.Split(v, ":")
		for i := len(parts) - 1; i >= 0; i-- {
			d, ok := new(big.Int).SetString(parts[i], 10)
			if !ok {
				return nil, yerr("invalid literal for int() with base 10: '%s'", parts[i])
			}
			total.Add(total, new(big.Int).Mul(d, base))
			base.Mul(base, big.NewInt(60))
		}
		out = normInt(total)
	default:
		out, err = pyIntParse(v, 10)
	}
	if err != nil {
		return nil, err
	}
	if neg {
		switch t := out.(type) {
		case int64:
			if t == math.MinInt64 {
				return new(big.Int).Neg(big.NewInt(t)), nil
			}
			return -t, nil
		case *big.Int:
			return normInt(new(big.Int).Neg(t)), nil
		}
	}
	return out, nil
}

// pyFloat mirrors Python's float(str) for the strings PyYAML hands it.
func pyFloat(s string) (float64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	unsigned := t
	if t != "" && (t[0] == '+' || t[0] == '-') {
		unsigned = t[1:]
	}
	switch unsigned {
	case "nan":
		return math.NaN(), nil
	case "inf", "infinity":
	default:
		if strings.ContainsAny(t, "xp_") || t == "" {
			return 0, fmt.Errorf("could not convert string to float: '%s'", s)
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil && !isRangeErr(err) {
		return 0, fmt.Errorf("could not convert string to float: '%s'", s)
	}
	return f, nil
}

func constructFloat(raw string) (any, error) {
	v := strings.ToLower(strings.ReplaceAll(raw, "_", ""))
	if v == "" {
		return nil, yerr("invalid float %q", raw)
	}
	sign := 1.0
	if v[0] == '-' {
		sign = -1
	}
	if v[0] == '-' || v[0] == '+' {
		v = v[1:]
	}
	switch {
	case v == ".inf":
		return sign * math.Inf(1), nil
	case v == ".nan":
		return math.NaN(), nil
	case strings.Contains(v, ":"):
		parts := strings.Split(v, ":")
		total, base := 0.0, 1.0
		for i := len(parts) - 1; i >= 0; i-- {
			d, err := pyFloat(parts[i])
			if err != nil {
				return nil, yerr("%v", err)
			}
			total += d * base
			base *= 60
		}
		return sign * total, nil
	}
	f, err := pyFloat(v)
	if err != nil {
		return nil, yerr("%v", err)
	}
	return sign * f, nil
}

func constructTimestamp(raw string) (any, error) {
	m := reTimestampParts.FindStringSubmatch(raw)
	if m == nil {
		return nil, yerr("invalid timestamp %q", raw)
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if month < 1 || month > 12 {
		return nil, yerr("month must be in 1..12")
	}
	if year < 1 {
		return nil, yerr("year %d is out of range", year)
	}
	if day < 1 || day > daysIn(year, month) {
		return nil, yerr("day is out of range for month")
	}
	if m[4] == "" {
		return Timestamp{Time: time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), DateOnly: true}, nil
	}
	hour, minute, second := atoi(m[4]), atoi(m[5]), atoi(m[6])
	if hour > 23 {
		return nil, yerr("hour must be in 0..23")
	}
	if minute > 59 {
		return nil, yerr("minute must be in 0..59")
	}
	if second > 59 {
		return nil, yerr("second must be in 0..59")
	}
	frac := m[7]
	if len(frac) > 6 {
		frac = frac[:6]
	}
	for len(frac) < 6 {
		frac += "0"
	}
	usec := atoi(frac)
	loc := time.UTC
	aware := false
	if m[8] != "" {
		aware = true
		if m[9] != "" {
			off := atoi(m[10])*3600 + atoi(m[11])*60
			if m[9] == "-" {
				off = -off
			}
			if off <= -86400 || off >= 86400 {
				return nil, yerr("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24)")
			}
			loc = time.FixedZone("", off)
		}
	}
	return Timestamp{Time: time.Date(year, time.Month(month), day, hour, minute, second, usec*1000, loc), Aware: aware}, nil
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
