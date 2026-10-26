// Package connectors is the connector catalog: the manifest directory report
// (validate_manifest_directory) and the /api/connectors payloads built from it.
package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// EffectivePolicy is manifest._signature_policy over an environment value
// (CONNECTOR_SIGNATURE_POLICY; unset means "optional"): strip, lower, anything
// but optional|required is "required".
func EffectivePolicy(raw string, set bool) string {
	if !set {
		return "optional"
	}
	v := strings.ToLower(manifest.PyStrip(raw))
	if v == "optional" || v == "required" {
		return v
	}
	return "required"
}

// Signature is the verify_manifest dict of a listed (never invalid) manifest.
type Signature struct {
	Status    string `json:"status"`
	KeyID     any    `json:"key_id"`
	Publisher any    `json:"publisher"`
}

// UnknownSignature renders catalog entries the report did not list.
type UnknownSignature struct {
	Status string `json:"status"`
}

// Entry is one connector of the report: path, package hash, signature and the
// manifest catalog_entry, in Python key order.
type Entry struct {
	Path               string
	ManifestSHA256     string
	Signature          Signature
	ManifestVersion    string
	ID                 string
	Name               string
	Author             string
	Homepage           string
	License            string
	Description        string
	Capabilities       []string
	Tags               []string
	CostPerLookup      float64
	DefaultConfidence  float64
	CredentialKey      *string
	credentialStripped bool
}

type entryHead struct {
	Path           string    `json:"path"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	Signature      Signature `json:"signature"`
}

type entryBody struct {
	ManifestVersion   string   `json:"manifest_version"`
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Author            string   `json:"author"`
	Homepage          string   `json:"homepage"`
	License           string   `json:"license"`
	Description       string   `json:"description"`
	Capabilities      []string `json:"capabilities"`
	Tags              []string `json:"tags"`
	CostPerLookup     float64  `json:"cost_per_lookup"`
	DefaultConfidence float64  `json:"default_confidence"`
}

func (e Entry) body() entryBody {
	return entryBody{e.ManifestVersion, e.ID, e.Name, e.Author, e.Homepage, e.License, e.Description,
		nonNil(e.Capabilities), nonNil(e.Tags), e.CostPerLookup, e.DefaultConfidence}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// MarshalJSON keeps Python key order; Compatibility drops credential_key.
func (e Entry) MarshalJSON() ([]byte, error) {
	type withKey struct {
		entryHead
		entryBody
		CredentialKey *string `json:"credential_key"`
	}
	type withoutKey struct {
		entryHead
		entryBody
	}
	h := entryHead{e.Path, e.ManifestSHA256, e.Signature}
	if e.credentialStripped {
		return json.Marshal(withoutKey{h, e.body()})
	}
	return json.Marshal(withKey{h, e.body(), e.CredentialKey})
}

// ReportError is one rejected manifest (or file) of the report.
type ReportError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Report is validate_manifest_directory's result.
type Report struct {
	OK              bool          `json:"ok"`
	ManifestVersion string        `json:"manifest_version"`
	SignaturePolicy string        `json:"signature_policy"`
	Count           int           `json:"count"`
	Connectors      []Entry       `json:"connectors"`
	Errors          []ReportError `json:"errors"`
}

// Compatibility strips credential_key from every entry (the /compatibility
// view). The input is not modified.
func Compatibility(r Report) Report {
	out := r
	out.Connectors = make([]Entry, len(r.Connectors))
	for i, e := range r.Connectors {
		e.CredentialKey, e.credentialStripped = nil, true
		out.Connectors[i] = e
	}
	out.Errors = append([]ReportError{}, r.Errors...)
	return out
}

// ValidateDirectory equals validate_manifest_directory over dir. Symlinks (files
// or directories) and a ".." path element are never followed: each is reported
// as an error entry (plan D4).
func ValidateDirectory(dir, policy string, trust manifest.TrustStore) Report {
	return scanDirectory(dir, policy, trust).report
}

type loadedManifest struct {
	Path  string
	Entry manifest.Entry
	Sig   manifest.VerifyResult
}

// scanResult is one pass over the directory: the validation report and the
// manifests load_all_manifests would load, from the same bytes.
type scanResult struct {
	report Report
	loaded []loadedManifest
}

type candidate struct {
	full    string // str(Path)
	rel     string
	symlink bool
}

func scanDirectory(dir, policy string, trust manifest.TrustStore) scanResult {
	if policy != "optional" {
		policy = "required"
	}
	res := scanResult{report: Report{ManifestVersion: "1", SignaturePolicy: policy,
		Connectors: []Entry{}, Errors: []ReportError{}}}
	fail := func(p, msg string) { res.report.Errors = append(res.report.Errors, ReportError{Path: p, Error: msg}) }
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == ".." {
			fail(dir, "manifest directory must not contain '..'")
			return finish(res)
		}
	}
	dir = filepath.Clean(dir)
	var cands []candidate
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == dir {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				if ok, _ := path.Match("*.y*ml", d.Name()); ok || linkIsDir(p) {
					cands = append(cands, candidate{full: p, rel: rel, symlink: true})
				}
			case d.Type().IsRegular():
				if ok, _ := path.Match("*.y*ml", d.Name()); ok {
					cands = append(cands, candidate{full: p, rel: rel})
				}
			}
			return nil
		})
	}
	sort.Slice(cands, func(i, j int) bool { return pyPathLess(cands[i].rel, cands[j].rel) })
	names := map[string]bool{}
	for _, c := range cands {
		if c.symlink {
			fail(c.full, "symlinks are not followed in the connector manifest directory")
			continue
		}
		data, err := readManifestFile(c.full)
		if err != nil {
			fail(c.full, err.Error())
			continue
		}
		sig := manifest.VerifyData(data, c.full+".sig", trust)
		sigRejected := sig.Status == manifest.SigInvalid || sig.Status == manifest.SigUntrusted ||
			(policy == "required" && sig.Status != manifest.SigTrusted)
		entry, derr := manifest.Decode(data, c.full)
		if !sigRejected && derr == nil {
			res.loaded = append(res.loaded, loadedManifest{Path: c.full, Entry: entry, Sig: sig})
		}
		if derr != nil {
			fail(c.full, derr.Error())
			continue
		}
		if msg := checkManifest(entry.Manifest, names); msg != "" {
			fail(c.full, msg)
			continue
		}
		if sigRejected {
			fail(c.full, strings.TrimRightFunc(fmt.Sprintf("connector signature is %s: %s", sig.Status, errText(sig.Err)), unicode.IsSpace))
			continue
		}
		res.report.Connectors = append(res.report.Connectors, newEntry(filepath.ToSlash(c.rel), entry, sig))
	}
	return finish(res)
}

func finish(res scanResult) scanResult {
	res.report.Count = len(res.report.Connectors)
	res.report.OK = len(res.report.Errors) == 0
	return res
}

func errText(err error) string {
	if err == nil || errors.Is(err, manifest.ErrBadSignature) {
		// Python's InvalidSignature has an empty message
		return ""
	}
	return err.Error()
}

// checkManifest is the rule chain of validate_manifest_directory, in order.
// names is the set of ids already seen (an id is reserved even when a later
// rule rejects its manifest, as Python).
func checkManifest(m manifest.Manifest, names map[string]bool) string {
	if !nameRE.MatchString(m.Name) {
		return "name must match ^[a-z][a-z0-9_]{2,63}$"
	}
	if names[m.Name] {
		return fmt.Sprintf("duplicate provider id '%s'", m.Name)
	}
	names[m.Name] = true
	switch strings.ToUpper(m.Request.Method) {
	case "GET", "POST", "PUT", "PATCH":
	default:
		return "request.method must be GET, POST, PUT, or PATCH"
	}
	if !strings.HasPrefix(m.Request.URL, "https://") {
		return "connector request URL must use HTTPS"
	}
	if len(m.Response.Mappings) == 0 {
		return "response.mappings must not be empty"
	}
	if !(m.DefaultConfidence >= 0 && m.DefaultConfidence <= 1) || m.CostPerLookup < 0 {
		return "confidence must be 0..1 and cost must be non-negative"
	}
	if !(m.Request.Timeout > 0 && m.Request.Timeout <= 120) {
		return "request.timeout must be between 0 and 120 seconds"
	}
	if m.Auth.Type != "none" && m.Auth.EnvVar == "" {
		return "authenticated connectors require auth.env_var"
	}
	return ""
}

func newEntry(rel string, e manifest.Entry, sig manifest.VerifyResult) Entry {
	m := e.Manifest
	s := Signature{Status: string(sig.Status), KeyID: sig.KeyID}
	if sig.Status == manifest.SigTrusted {
		s.Publisher = sig.Publisher
	}
	out := Entry{
		Path: rel, ManifestSHA256: e.SHA256, Signature: s,
		ManifestVersion: m.ManifestVersion, ID: m.Name, Name: m.DisplayName,
		Author: m.Author, Homepage: m.Homepage, License: m.License, Description: m.Description,
		Capabilities: allCapabilities(m, e.MappingKeys), Tags: m.Tags,
		CostPerLookup: m.CostPerLookup, DefaultConfidence: m.DefaultConfidence,
	}
	if out.Name == "" {
		out.Name = pyTitle(strings.ReplaceAll(m.Name, "_", " "))
	}
	if m.Auth.EnvVar != "" {
		k := m.Auth.EnvVar
		out.CredentialKey = &k
	}
	return out
}

// allCapabilities is ProviderManifest.all_capabilities: capability, extras and
// the mapping keys (authored order), de-duplicated, empty strings dropped.
func allCapabilities(m manifest.Manifest, mappingKeys []string) []string {
	keys := mappingKeys
	if keys == nil {
		for k := range m.Response.Mappings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, c := range append(append([]string{m.Capability}, m.Capabilities...), keys...) {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// pyTitle is str.title(): a letter starts a word unless the previous character
// is a letter.
func pyTitle(s string) string {
	var b strings.Builder
	prevLetter := false
	for _, r := range s {
		switch {
		case !unicode.IsLetter(r):
			b.WriteRune(r)
			prevLetter = false
		case prevLetter:
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(unicode.ToTitle(r))
			prevLetter = true
		}
	}
	return b.String()
}

func linkIsDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// pyPathLess orders like sorted(Path): component-wise.
func pyPathLess(a, b string) bool {
	x, y := strings.Split(filepath.ToSlash(a), "/"), strings.Split(filepath.ToSlash(b), "/")
	for k := 0; k < len(x) && k < len(y); k++ {
		if x[k] != y[k] {
			return x[k] < y[k]
		}
	}
	return len(x) < len(y)
}

var errManifestTooLarge = errors.New("manifest exceeds the 1 MiB limit")

// readManifestFile is one bounded read; a path that changed into a symlink
// between listing and opening is refused.
func readManifestFile(p string) ([]byte, error) {
	before, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("symlinks are not followed in the connector manifest directory")
	}
	data, err := io.ReadAll(io.LimitReader(f, manifest.MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > manifest.MaxManifestBytes {
		return nil, errManifestTooLarge
	}
	return data, nil
}
