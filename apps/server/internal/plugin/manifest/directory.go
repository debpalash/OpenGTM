package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

var (
	nameRE      = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)
	manifestRE  = regexp.MustCompile(`(?s)^.*\.y.*ml$`) // pathlib rglob("*.y*ml")
	allowedHTTP = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true}
)

// ReportEntry is one compatible connector in a directory report.
type ReportEntry struct {
	Path           string
	ManifestSHA256 string
	Signature      signing.Result
	Manifest       *V1
}

// ReportError is one rejected manifest.
type ReportError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
	// Err keeps the typed error (e.g. *ModelError) for callers and tests.
	Err error `json:"-"`
}

// Report mirrors the dict returned by validate_manifest_directory().
type Report struct {
	OK              bool
	ManifestVersion string
	SignaturePolicy string
	Connectors      []ReportEntry
	Errors          []ReportError
}

// PyValue renders the report as the same ordered structure Python returns,
// so `--json` output can be byte-compared with the Python CLI.
func (r *Report) PyValue() *pycompat.Map {
	out := pycompat.NewMap()
	_ = out.Set("ok", r.OK)
	_ = out.Set("manifest_version", r.ManifestVersion)
	_ = out.Set("signature_policy", r.SignaturePolicy)
	_ = out.Set("count", int64(len(r.Connectors)))
	conns := make([]any, 0, len(r.Connectors))
	for _, c := range r.Connectors {
		m := pycompat.NewMap()
		_ = m.Set("path", c.Path)
		_ = m.Set("manifest_sha256", c.ManifestSHA256)
		_ = m.Set("signature", c.Signature.PyValue())
		for _, e := range c.Manifest.CatalogEntry().Entries() {
			_ = m.Set(e.Key, e.Value)
		}
		conns = append(conns, m)
	}
	_ = out.Set("connectors", conns)
	errs := make([]any, 0, len(r.Errors))
	for _, e := range r.Errors {
		m := pycompat.NewMap()
		_ = m.Set("path", e.Path)
		_ = m.Set("error", e.Error)
		errs = append(errs, m)
	}
	_ = out.Set("errors", errs)
	return out
}

// JSON renders the report like json.dumps(report, indent=2).
func (r *Report) JSON() ([]byte, error) {
	return pycompat.Dumps(r.PyValue(), pycompat.DumpOptions{Indent: 2, EnsureASCII: true})
}

// DirectoryOptions configures ValidateDirectory.
type DirectoryOptions struct {
	// SignaturePolicy is "optional" or "required"; empty uses
	// CONNECTOR_SIGNATURE_POLICY like Python. Unknown values fail closed.
	SignaturePolicy string
	// TrustStore is the trusted-publishers JSON path (missing file = empty).
	TrustStore string
}

// FindManifests mirrors sorted(directory.rglob("*.y*ml")): every file or
// directory whose name matches, ordered by path components.
func FindManifests(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var found [][]string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == dir {
			return nil
		}
		if manifestRE.MatchString(d.Name()) {
			rel, _ := filepath.Rel(dir, p)
			found = append(found, strings.Split(rel, string(filepath.Separator)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	out := make([]string, len(found))
	for i, parts := range found {
		out[i] = filepath.Join(parts...)
	}
	return out, nil
}

// readManifest mirrors open(path).read() error text for directories.
func readManifest(path string) ([]byte, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, fmt.Errorf("[Errno 21] Is a directory: '%s'", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !isUTF8(data) {
		return nil, errors.New("'utf-8' codec can't decode the manifest: invalid UTF-8")
	}
	return data, nil
}

// CheckV1 applies the post-load compatibility rules of
// validate_manifest_directory to one manifest, in Python's order.
// names tracks provider ids across the directory (may be nil).
func CheckV1(m *V1, names map[string]bool) error {
	if !nameRE.MatchString(m.Name) {
		return errors.New("name must match ^[a-z][a-z0-9_]{2,63}$")
	}
	if names != nil {
		if names[m.Name] {
			return fmt.Errorf("duplicate provider id '%s'", m.Name)
		}
		names[m.Name] = true
	}
	if !allowedHTTP[pycompat.Upper(m.Request.Method)] {
		return errors.New("request.method must be GET, POST, PUT, or PATCH")
	}
	if !strings.HasPrefix(m.Request.URL, "https://") {
		return errors.New("connector request URL must use HTTPS")
	}
	if len(m.Response.Mappings) == 0 {
		return errors.New("response.mappings must not be empty")
	}
	if !(0 <= m.DefaultConfidence && m.DefaultConfidence <= 1) || m.CostPerLookup < 0 {
		return errors.New("confidence must be 0..1 and cost must be non-negative")
	}
	if !(0 < m.Request.Timeout && m.Request.Timeout <= 120) {
		return errors.New("request.timeout must be between 0 and 120 seconds")
	}
	if m.Auth.Type != "none" && m.Auth.EnvVarName() == "" {
		return errors.New("authenticated connectors require auth.env_var")
	}
	return nil
}

// ValidateDirectory mirrors validate_manifest_directory() exactly: same
// accept/reject decisions, same messages for rule violations, and a
// pydantic-shaped *ModelError for schema errors.
func ValidateDirectory(dir string, opts DirectoryOptions) *Report {
	policy := opts.SignaturePolicy
	if policy == "" {
		policy = signing.PolicyFromEnv()
	}
	policy = signing.NormalizePolicy(policy)
	rep := &Report{ManifestVersion: "1", SignaturePolicy: policy, Connectors: []ReportEntry{}, Errors: []ReportError{}}
	rels, _ := FindManifests(dir)
	names := map[string]bool{}
	for _, rel := range rels {
		full := filepath.Join(dir, rel)
		entry, err := validateOne(full, rel, names, policy, opts.TrustStore)
		if err != nil {
			rep.Errors = append(rep.Errors, ReportError{Path: full, Error: err.Error(), Err: err})
			continue
		}
		rep.Connectors = append(rep.Connectors, *entry)
	}
	rep.OK = len(rep.Errors) == 0
	return rep
}

func validateOne(full, rel string, names map[string]bool, policy, trust string) (*ReportEntry, error) {
	text, err := readManifest(full)
	if err != nil {
		return nil, err
	}
	m, err := LoadV1Bytes(full, text)
	if err != nil {
		return nil, err
	}
	if err := CheckV1(m, names); err != nil {
		return nil, err
	}
	sig := signing.VerifyManifest(full, trust)
	if signing.Rejects(policy, sig) {
		return nil, fmt.Errorf("%s", strings.TrimRightFunc(fmt.Sprintf("connector signature is %s: %s", sig.Status, sig.Error), pycompat.IsSpace))
	}
	canon, err := signing.CanonicalBytes(text)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canon)
	return &ReportEntry{Path: rel, ManifestSHA256: hex.EncodeToString(sum[:]), Signature: sig, Manifest: m}, nil
}
