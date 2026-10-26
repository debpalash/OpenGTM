package manifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"gopkg.in/yaml.v3"
)

// SigStatus is the detached-signature state of a connector manifest.
type SigStatus string

const (
	SigTrusted   SigStatus = "trusted"
	SigUntrusted SigStatus = "untrusted"
	SigInvalid   SigStatus = "invalid"
	SigUnsigned  SigStatus = "unsigned"
)

const (
	signatureVersion = "1"
	// MaxManifestBytes bounds a manifest, signature envelope or trust store
	// read (Python is unbounded; the install path already caps bundles at 1 MB).
	MaxManifestBytes = 1 << 20
	maxYAMLNodes     = 1_000_000 // expansion cap, anchors/aliases can otherwise explode
	maxYAMLDepth     = 512
)

var (
	errDigestMismatch   = errors.New("manifest digest mismatch")
	errEnvelope         = errors.New("unsupported connector signature envelope")
	errTrustStore       = errors.New("invalid connector trust store")
	errNotMapping       = errors.New("manifest is not a mapping")
	errBadSignature     = errors.New("signature verification failed")
	errManifestTooLarge = errors.New("manifest exceeds the 1 MiB limit")
)

// ---------------------------------------------------------------- trust store

type trustEntry struct {
	publicKey    any
	hasPublicKey bool
	publisher    any
	hasPublisher bool
}

// TrustStore is the parsed trusted-publishers.json (key_id -> entry). Python
// loads it lazily inside verify_manifest, after the envelope checks, so a
// broken store only turns a signed manifest invalid; the load error is kept
// and reported at the same point.
type TrustStore struct {
	keys map[string]trustEntry
	err  error
}

// LoadTrustStore reads path. A missing file is an empty store (every signed
// manifest is then untrusted); any other failure is deferred to Verify.
func LoadTrustStore(path string) TrustStore {
	data, err := readLimited(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TrustStore{keys: map[string]trustEntry{}}
		}
		return TrustStore{err: err}
	}
	return ParseTrustStore(data)
}

// ParseTrustStore mirrors signing.load_trust_store on already read bytes.
func ParseTrustStore(data []byte) TrustStore {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return TrustStore{err: err}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return TrustStore{err: errTrustStore}
	}
	version, _ := obj["version"].(float64)
	if bv, isBool := obj["version"].(bool); isBool && bv {
		version = 1 // Python: True == 1
	}
	list, isList := obj["keys"].([]any)
	if version != 1 || !isList {
		return TrustStore{err: errTrustStore}
	}
	keys := make(map[string]trustEntry, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return TrustStore{err: errTrustStore}
		}
		id, present := entry["key_id"]
		if !present {
			return TrustStore{err: errTrustStore}
		}
		switch id.(type) {
		case []any, map[string]any:
			return TrustStore{err: errTrustStore} // unhashable dict key
		}
		sid, isStr := id.(string)
		if !isStr {
			continue // non-string ids can never match a string lookup here
		}
		te := trustEntry{}
		te.publicKey, te.hasPublicKey = entry["public_key"]
		te.publisher, te.hasPublisher = entry["publisher"]
		keys[sid] = te // duplicate key_id: last wins
	}
	return TrustStore{keys: keys}
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifestBytes {
		return nil, errManifestTooLarge
	}
	return data, nil
}

// ------------------------------------------------------------------- verify

// VerifyResult is the dict signing.verify_manifest returns. KeyID is whatever
// JSON value the envelope carried (nil, string, number); Err is set only for
// SigInvalid.
type VerifyResult struct {
	Status    SigStatus
	KeyID     any
	Publisher string
	Err       error
}

// Verify equals signing.verify_manifest. The error is non-nil exactly when the
// status is SigInvalid and says why.
func Verify(path string, trust TrustStore) (SigStatus, error) {
	r := VerifyDetail(path, trust)
	return r.Status, r.Err
}

// VerifyDetail is Verify with the key id and publisher.
func VerifyDetail(path string, trust TrustStore) VerifyResult {
	sigPath := path + ".sig"
	if _, err := os.Stat(sigPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return VerifyResult{Status: SigUnsigned}
		}
		return VerifyResult{Status: SigInvalid, Err: err}
	}
	invalid := func(keyID any, err error) VerifyResult {
		return VerifyResult{Status: SigInvalid, KeyID: keyID, Err: err}
	}
	sigBytes, err := readLimited(sigPath)
	if err != nil {
		return invalid(nil, err)
	}
	var envAny any
	if err := json.Unmarshal(sigBytes, &envAny); err != nil {
		return invalid(nil, err)
	}
	env, ok := envAny.(map[string]any)
	if !ok {
		return invalid(nil, errors.New("signature envelope is not an object"))
	}
	keyID := env["key_id"]
	if v, _ := env["signature_version"].(string); v != signatureVersion {
		return invalid(keyID, errEnvelope)
	}
	if a, _ := env["algorithm"].(string); a != "Ed25519" {
		return invalid(keyID, errEnvelope)
	}
	if trust.err != nil {
		return invalid(keyID, trust.err)
	}
	var entry trustEntry
	found := false
	switch k := keyID.(type) {
	case string:
		entry, found = trust.keys[k]
	case []any, map[string]any:
		return invalid(keyID, errors.New("unhashable key_id"))
	}
	if !found {
		return VerifyResult{Status: SigUntrusted, KeyID: keyID}
	}
	data, err := readLimited(path)
	if err != nil {
		return invalid(keyID, err)
	}
	payload, err := CanonicalYAML(data)
	if err != nil {
		return invalid(keyID, err)
	}
	sum := sha256.Sum256(payload)
	if digest, _ := env["manifest_sha256"].(string); hex.EncodeToString(sum[:]) != digest {
		return invalid(keyID, errDigestMismatch)
	}
	pkText, _ := entry.publicKey.(string)
	if !entry.hasPublicKey {
		return invalid(keyID, errors.New("'public_key'"))
	}
	pub, err := strictBase64(pkText)
	if err != nil {
		return invalid(keyID, err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return invalid(keyID, errors.New("An Ed25519 public key is 32 bytes long"))
	}
	sigText, present := env["signature"]
	if !present {
		return invalid(keyID, errors.New("'signature'"))
	}
	sigStr, _ := sigText.(string)
	sig, err := strictBase64(sigStr)
	if err != nil {
		return invalid(keyID, err)
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return invalid(keyID, errBadSignature)
	}
	publisher, _ := keyID.(string)
	if entry.hasPublisher {
		if p, isStr := entry.publisher.(string); isStr {
			publisher = p
		} else {
			publisher = ""
		}
	}
	return VerifyResult{Status: SigTrusted, KeyID: keyID, Publisher: publisher}
}

// strictBase64 is base64.b64decode(..., validate=True): alphabet only (Go's
// decoder silently drops CR/LF), padding required.
func strictBase64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("Only base64 data is allowed")
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("Only base64 data is allowed")
	}
	return b, nil
}

// ---------------------------------------------------------------- canonical

// CanonicalPath is signing.canonical_manifest(path).
func CanonicalPath(path string) ([]byte, error) {
	data, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	return CanonicalYAML(data)
}

// CanonicalYAML is signing.canonical_manifest on the file's bytes: PyYAML
// safe_load (YAML 1.1 scalar resolution) then
// json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=False).
// These bytes are what is signed and hashed (Entry.SHA256).
func CanonicalYAML(data []byte) ([]byte, error) {
	tree, err := loadYAML(data)
	if err != nil {
		return nil, err
	}
	if _, ok := tree.(*pyMap); !ok {
		return nil, errNotMapping
	}
	var b bytes.Buffer
	if err := dumpValue(&b, tree); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// CanonicalManifest renders a decoded Manifest in the canonical form.
// Note: Python hashes the manifest as authored (raw YAML mapping), not the
// pydantic model dump, so for a typed Manifest this is the canonical form of
// the fully spelled-out model; it equals CanonicalYAML of any YAML that spells
// every field out (the fixed point CanonicalYAML(CanonicalManifest(m)) ==
// CanonicalManifest(m)). The authoritative Python-equal hash of a shipped
// manifest is Entry.SHA256 from LoadIndex.
func CanonicalManifest(m Manifest) ([]byte, error) {
	strs := func(in []string) []any {
		out := make([]any, len(in))
		for i, s := range in {
			out[i] = s
		}
		return out
	}
	dyn := func(in map[string]any) any {
		out := make(map[string]any, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	maps := make(map[string]any, len(m.Response.Mappings))
	for k, v := range m.Response.Mappings {
		maps[k] = v
	}
	var nullable = func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	tree := map[string]any{
		"manifest_version": m.ManifestVersion, "name": m.Name, "display_name": m.DisplayName,
		"author": m.Author, "homepage": m.Homepage, "license": m.License, "tags": strs(m.Tags),
		"capability": m.Capability, "capabilities": strs(m.Capabilities), "description": m.Description,
		"default_confidence": m.DefaultConfidence, "cost_per_lookup": m.CostPerLookup,
		"auth": map[string]any{
			"type": m.Auth.Type, "param": nullable(m.Auth.Param),
			"value": nullable(m.Auth.Value), "env_var": nullable(m.Auth.EnvVar),
		},
		"request": map[string]any{
			"method": m.Request.Method, "url": m.Request.URL, "headers": dyn(m.Request.Headers),
			"query": dyn(m.Request.Query), "body_template": m.Request.BodyTemplate, "timeout": m.Request.Timeout,
		},
		"response": map[string]any{
			"error_path": nullable(m.Response.ErrorPath), "error_message_path": nullable(m.Response.ErrorMessagePath),
			"mappings": maps,
		},
		"input_fields": strs(m.InputFields),
	}
	var b bytes.Buffer
	if err := dumpValue(&b, tree); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ------------------------------------------------------------ python values

// pyEnt/pyMap model a Python dict whose keys keep their YAML type (str, int,
// bool, None, float) because json.dumps(sort_keys=True) sorts and stringifies
// them by type.
type pyEnt struct{ key, val any }
type pyMap struct{ ents []pyEnt }

func (m *pyMap) set(k, v any) {
	for i := range m.ents {
		if pyKeyEq(m.ents[i].key, k) {
			m.ents[i].val = v // dict keeps the first key object, last value wins
			return
		}
	}
	m.ents = append(m.ents, pyEnt{k, v})
}

func numKey(v any) (*big.Float, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case *big.Int:
		return new(big.Float).SetInt(x), true
	case int64:
		return new(big.Float).SetInt64(x), true
	case float64:
		if math.IsNaN(x) {
			return nil, false
		}
		return new(big.Float).SetFloat64(x), true
	}
	return nil, false
}

func pyKeyEq(a, b any) bool {
	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	fa, oka := numKey(a)
	fb, okb := numKey(b)
	return oka && okb && fa.Cmp(fb) == 0
}

// ---------------------------------------------------------------- yaml load

func loadYAML(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("manifest is not valid UTF-8")
	}
	// read_text() applies universal newlines before PyYAML sees the text.
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	dec := yaml.NewDecoder(strings.NewReader(text))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errNotMapping // empty or comment-only document
		}
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected a single document in the stream")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errNotMapping
	}
	c := &constructor{}
	return c.node(doc.Content[0], 0)
}

type constructor struct{ nodes int }

func (c *constructor) node(n *yaml.Node, depth int) (any, error) {
	if depth > maxYAMLDepth {
		return nil, errors.New("yaml: nesting too deep (or recursive anchor)")
	}
	c.nodes++
	if c.nodes > maxYAMLNodes {
		return nil, errors.New("yaml: document expands to too many nodes")
	}
	switch n.Kind {
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, errors.New("yaml: dangling alias")
		}
		return c.node(n.Alias, depth+1)
	case yaml.ScalarNode:
		return constructScalar(n)
	case yaml.SequenceNode:
		if err := checkCollectionTag(n, "!!seq"); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(n.Content))
		for _, e := range n.Content {
			v, err := c.node(e, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		if err := checkCollectionTag(n, "!!map"); err != nil {
			return nil, err
		}
		return c.mapping(n, depth)
	}
	return nil, errors.New("yaml: unsupported node")
}

func checkCollectionTag(n *yaml.Node, want string) error {
	if n.Style&yaml.TaggedStyle != 0 && n.Tag != want {
		return fmt.Errorf("yaml: tag %s cannot be constructed by safe_load", n.Tag)
	}
	return nil
}

func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Style == 0 && k.Value == "<<"
}

// mapping mirrors SafeConstructor.flatten_mapping + construct_mapping.
func (c *constructor) mapping(n *yaml.Node, depth int) (any, error) {
	out := &pyMap{}
	var merged []pyEnt
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMergeKey(k) {
			vs, err := c.mergeSources(v, depth)
			if err != nil {
				return nil, err
			}
			merged = append(merged, vs...)
		}
	}
	for _, e := range merged {
		out.set(e.key, e.val)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMergeKey(k) {
			continue
		}
		kv, err := c.node(k, depth+1)
		if err != nil {
			return nil, err
		}
		switch kv.(type) {
		case *pyMap, []any:
			return nil, errors.New("yaml: unhashable mapping key")
		}
		vv, err := c.node(v, depth+1)
		if err != nil {
			return nil, err
		}
		out.set(kv, vv)
	}
	return out, nil
}

func (c *constructor) mergeSources(v *yaml.Node, depth int) ([]pyEnt, error) {
	for v.Kind == yaml.AliasNode && v.Alias != nil {
		v = v.Alias
	}
	one := func(m *yaml.Node) ([]pyEnt, error) {
		val, err := c.node(m, depth+1)
		if err != nil {
			return nil, err
		}
		pm, ok := val.(*pyMap)
		if !ok {
			return nil, errors.New("yaml: expected a mapping for merging")
		}
		return pm.ents, nil
	}
	switch v.Kind {
	case yaml.MappingNode:
		return one(v)
	case yaml.SequenceNode:
		// PyYAML reverses the list, so the FIRST mapping wins on conflicts.
		var all []pyEnt
		for i := len(v.Content) - 1; i >= 0; i-- {
			s := v.Content[i]
			for s.Kind == yaml.AliasNode && s.Alias != nil {
				s = s.Alias
			}
			if s.Kind != yaml.MappingNode {
				return nil, errors.New("yaml: expected a mapping for merging")
			}
			ents, err := one(s)
			if err != nil {
				return nil, err
			}
			all = append(all, ents...)
		}
		return all, nil
	}
	return nil, errors.New("yaml: expected a mapping or list of mappings for merging")
}

// PyYAML 1.1 implicit resolvers (resolver.py), anchored.
var (
	reBool  = regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)
	reFloat = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	reInt   = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	reNull  = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	reTime  = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
)

func constructScalar(n *yaml.Node) (any, error) {
	v := n.Value
	switch {
	case n.Style&yaml.TaggedStyle != 0:
		switch n.Tag {
		case "!!str":
			return v, nil
		case "!!int":
			return constructInt(v)
		case "!!float":
			return constructFloat(v)
		case "!!bool":
			return constructBool(v)
		case "!!null":
			return nil, nil
		}
		return nil, fmt.Errorf("yaml: tag %s cannot be constructed by safe_load", n.Tag)
	case n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return v, nil
	}
	switch {
	case reNull.MatchString(v):
		return nil, nil
	case reBool.MatchString(v):
		return constructBool(v)
	case reFloat.MatchString(v):
		return constructFloat(v)
	case reInt.MatchString(v):
		return constructInt(v)
	case reTime.MatchString(v):
		return nil, errors.New("Object of type datetime is not JSON serializable")
	case v == "<<" || v == "=":
		return nil, fmt.Errorf("yaml: could not determine a constructor for the tag of %q", v)
	}
	return v, nil
}

func constructBool(v string) (any, error) {
	switch strings.ToLower(v) {
	case "yes", "true", "on":
		return true, nil
	case "no", "false", "off":
		return false, nil
	}
	return nil, fmt.Errorf("yaml: invalid bool %q", v)
}

func constructInt(v string) (any, error) {
	v = strings.ReplaceAll(v, "_", "")
	if v == "" {
		return nil, errors.New("yaml: empty int")
	}
	sign := 1
	if v[0] == '-' {
		sign = -1
	}
	if v[0] == '-' || v[0] == '+' {
		v = v[1:]
	}
	parse := func(s string, base int) (*big.Int, error) {
		i, ok := new(big.Int).SetString(s, base)
		if !ok || s == "" || strings.ContainsAny(s, "+-_ ") {
			return nil, fmt.Errorf("yaml: invalid int %q", s)
		}
		return i, nil
	}
	var out *big.Int
	var err error
	switch {
	case v == "0":
		out = new(big.Int)
	case strings.HasPrefix(v, "0b"):
		out, err = parse(v[2:], 2)
	case strings.HasPrefix(v, "0x"):
		out, err = parse(v[2:], 16)
	case strings.HasPrefix(v, "0"):
		out, err = parse(v, 8)
	case strings.Contains(v, ":"):
		parts := strings.Split(v, ":")
		out = new(big.Int)
		base := big.NewInt(1)
		for i := len(parts) - 1; i >= 0; i-- {
			d, e := parse(parts[i], 10)
			if e != nil {
				return nil, e
			}
			out.Add(out, new(big.Int).Mul(d, base))
			base = new(big.Int).Mul(base, big.NewInt(60))
		}
	default:
		out, err = parse(v, 10)
	}
	if err != nil {
		return nil, err
	}
	if sign < 0 {
		out = new(big.Int).Neg(out)
	}
	return out, nil
}

func constructFloat(v string) (any, error) {
	v = strings.ToLower(strings.ReplaceAll(v, "_", ""))
	if v == "" {
		return nil, errors.New("yaml: empty float")
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
			d, err := strconv.ParseFloat(parts[i], 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, err
			}
			total += d * base
			base *= 60
		}
		return sign * total, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, fmt.Errorf("yaml: invalid float %q", v)
	}
	return sign * f, nil
}

// ---------------------------------------------------------------- json dump

func dumpValue(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case *big.Int:
		b.WriteString(x.String())
	case float64:
		s, err := jsonFloat(x)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		return dumpString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := dumpValue(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		m := &pyMap{}
		for k, e := range x {
			m.ents = append(m.ents, pyEnt{k, e})
		}
		return dumpValue(b, m)
	case *pyMap:
		return dumpMap(b, x)
	default:
		return fmt.Errorf("Object of type %T is not JSON serializable", v)
	}
	return nil
}

func jsonFloat(f float64) (string, error) {
	switch {
	case math.IsNaN(f):
		return "NaN", nil
	case math.IsInf(f, 1):
		return "Infinity", nil
	case math.IsInf(f, -1):
		return "-Infinity", nil
	}
	return canonjson.FloatRepr(f)
}

// dumpMap sorts like Python's sorted(dict.items()): all-str keys by code point
// (== UTF-8 byte order), all-numeric keys numerically, anything else raises.
func dumpMap(b *bytes.Buffer, m *pyMap) error {
	ents := append([]pyEnt(nil), m.ents...)
	if len(ents) > 1 {
		allStr, allNum := true, true
		for _, e := range ents {
			if _, ok := e.key.(string); !ok {
				allStr = false
			}
			if _, ok := numKey(e.key); !ok {
				allNum = false
			}
		}
		switch {
		case allStr:
			sort.SliceStable(ents, func(i, j int) bool { return ents[i].key.(string) < ents[j].key.(string) })
		case allNum:
			sort.SliceStable(ents, func(i, j int) bool {
				fi, _ := numKey(ents[i].key)
				fj, _ := numKey(ents[j].key)
				return fi.Cmp(fj) < 0
			})
		default:
			return errors.New("'<' not supported between instances of mixed key types")
		}
	}
	b.WriteByte('{')
	for i, e := range ents {
		if i > 0 {
			b.WriteByte(',')
		}
		var ks string
		switch k := e.key.(type) {
		case string:
			ks = k
		case nil:
			ks = "null"
		case bool:
			ks = strconv.FormatBool(k)
		case *big.Int:
			ks = k.String()
		case int64:
			ks = strconv.FormatInt(k, 10)
		case float64:
			s, err := jsonFloat(k)
			if err != nil {
				return err
			}
			ks = s
		default:
			return fmt.Errorf("keys must be str, int, float, bool or None, not %T", e.key)
		}
		if err := dumpString(b, ks); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := dumpValue(b, e.val); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// dumpString is json's encoder with ensure_ascii=False: only ", \ and C0
// controls are escaped; everything else (including U+007F, U+2028) is literal.
func dumpString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("string is not valid UTF-8 (lone surrogate)")
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}
