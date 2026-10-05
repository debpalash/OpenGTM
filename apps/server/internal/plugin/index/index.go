// Package index implements the static plugin index: a signable, mirror
// friendly JSON document that lists plugin versions, where to download each
// bundle, its SHA-256, its Ed25519 signature envelope and its publisher, and
// the client that searches it and installs from it.
//
// The index is untrusted metadata. Trust comes from the Ed25519 signature
// inside each bundle, checked against the trusted-publisher store by
// bundle.Install; the index adds integrity (SHA-256), rollback protection and
// discoverability, and may itself be signed (index.json.sig) so a mirror
// cannot hide or relabel versions. The format is documented in
// docs/plugins/README.md and described by
// packages/contracts/plugin-index.v1.schema.json.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FormatVersion is the only index_version this client understands.
const FormatVersion = 1

// DefaultURL is the placeholder location of the community index. It is not
// published yet; operators point OPENGTM_PLUGIN_INDEX (or plugins.index_url in
// opengtm.yaml) at their own index until a community index exists.
const DefaultURL = "https://debpalash.github.io/opengtm-plugins/index.json"

// Certification levels (informational; the host does not act on them).
const (
	CertUnreviewed = "unreviewed"
	CertBeta       = "beta"
	CertCertified  = "certified"
)

// Index is the document served at index.json.
type Index struct {
	IndexVersion int    `json:"index_version"`
	Name         string `json:"name,omitempty"`
	// GeneratedAt is an RFC 3339 UTC timestamp.
	GeneratedAt string   `json:"generated_at"`
	Plugins     []Plugin `json:"plugins"`
}

// Plugin groups the versions of one plugin.
type Plugin struct {
	Name     string    `json:"name"`
	Versions []Version `json:"versions"`
}

// Capabilities mirrors the manifest's capabilities, shown before install.
type Capabilities struct {
	Network []string `json:"network"`
	Secrets []string `json:"secrets"`
	Browser bool     `json:"browser"`
}

// Signature is the detached signature envelope of the bundle's manifest,
// duplicated here so clients can pin it. Install cross-checks every field
// against the envelope inside the bundle.
type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	// ManifestSHA256 is the SHA-256 of the canonical manifest.
	ManifestSHA256 string `json:"manifest_sha256"`
	// Signature is the base64 Ed25519 signature.
	Signature string `json:"signature"`
}

// Version is one published release of a plugin.
type Version struct {
	Version     string   `json:"version"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind"`
	Runtime     string   `json:"runtime"`
	Author      string   `json:"author,omitempty"`
	License     string   `json:"license,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	// Capabilities are what the plugin may touch (copied from its manifest).
	Capabilities Capabilities `json:"capabilities"`
	// URL locates the bundle: absolute https URL, or a path relative to the
	// index's own location (which makes a directory a complete mirror).
	URL string `json:"url"`
	// SHA256 and Size describe the bundle file.
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
	// Publisher is the publisher name shown to users; the trust store is
	// authoritative for who a key belongs to.
	Publisher string    `json:"publisher"`
	Signature Signature `json:"signature"`
	// Certification is informational: unreviewed (default), beta, certified.
	Certification string `json:"certification,omitempty"`
	PublishedAt   string `json:"published_at,omitempty"`
	// Yanked versions are never chosen by "latest" and need an explicit
	// name@version; Reason is shown when they are.
	Yanked       bool   `json:"yanked,omitempty"`
	YankedReason string `json:"yanked_reason,omitempty"`
}

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	kinds    = map[string]bool{"provider": true, "scraper": true, "signal": true, "destination": true, "function": true, "tool": true}
	runtimes = map[string]bool{"declarative": true, "wasm": true, "process": true}
)

// Parse decodes and validates an index document.
func Parse(data []byte) (*Index, error) {
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return nil, fmt.Errorf("plugin index is not valid JSON: %w", err)
	}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	return &ix, nil
}

// Validate checks the structural rules the JSON Schema expresses plus the
// ones it cannot (duplicates, semantic versions).
func (ix *Index) Validate() error {
	if ix.IndexVersion != FormatVersion {
		return fmt.Errorf("unsupported plugin index_version %d (this client reads %d)", ix.IndexVersion, FormatVersion)
	}
	var errs []error
	seen := map[string]bool{}
	for _, p := range ix.Plugins {
		if !nameRE.MatchString(p.Name) {
			errs = append(errs, fmt.Errorf("plugin name %q is invalid", p.Name))
			continue
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("plugin %s is listed twice", p.Name))
		}
		seen[p.Name] = true
		if len(p.Versions) == 0 {
			errs = append(errs, fmt.Errorf("plugin %s has no versions", p.Name))
		}
		vs := map[string]bool{}
		for _, v := range p.Versions {
			where := p.Name + "@" + v.Version
			if _, err := ParseSemver(v.Version); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
				continue
			}
			if vs[v.Version] {
				errs = append(errs, fmt.Errorf("%s is listed twice", where))
			}
			vs[v.Version] = true
			if !kinds[v.Kind] {
				errs = append(errs, fmt.Errorf("%s: unknown kind %q", where, v.Kind))
			}
			if !runtimes[v.Runtime] {
				errs = append(errs, fmt.Errorf("%s: unknown runtime %q", where, v.Runtime))
			}
			if strings.TrimSpace(v.URL) == "" {
				errs = append(errs, fmt.Errorf("%s: url is required", where))
			}
			if !sha256RE.MatchString(v.SHA256) {
				errs = append(errs, fmt.Errorf("%s: sha256 must be 64 lower-case hex characters", where))
			}
			if v.Size < 0 {
				errs = append(errs, fmt.Errorf("%s: size must not be negative", where))
			}
			if v.Signature.KeyID == "" || v.Signature.Signature == "" || !sha256RE.MatchString(v.Signature.ManifestSHA256) {
				errs = append(errs, fmt.Errorf("%s: signature needs key_id, manifest_sha256 and signature", where))
			}
			if v.Signature.Algorithm != "Ed25519" {
				errs = append(errs, fmt.Errorf("%s: signature algorithm must be Ed25519", where))
			}
			if v.Publisher == "" {
				errs = append(errs, fmt.Errorf("%s: publisher is required", where))
			}
			switch v.Certification {
			case "", CertUnreviewed, CertBeta, CertCertified:
			default:
				errs = append(errs, fmt.Errorf("%s: unknown certification %q", where, v.Certification))
			}
		}
	}
	return errors.Join(errs...)
}

// Find returns the plugin entry by name.
func (ix *Index) Find(name string) (*Plugin, bool) {
	for i := range ix.Plugins {
		if ix.Plugins[i].Name == name {
			return &ix.Plugins[i], true
		}
	}
	return nil, false
}

// Sorted returns the versions newest first.
func (p *Plugin) Sorted() []Version {
	out := append([]Version(nil), p.Versions...)
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := ParseSemver(out[i].Version)
		b, _ := ParseSemver(out[j].Version)
		return a.Compare(b) > 0
	})
	return out
}

// Resolve picks a version. An empty spec or "latest" selects the highest
// non-yanked stable release (a pre-release only when no stable release
// exists and prerelease is true). Anything else is an exact version, which
// may be yanked.
func (p *Plugin) Resolve(spec string, prerelease bool) (Version, error) {
	sorted := p.Sorted()
	if spec == "" || spec == "latest" {
		var pre *Version
		for i := range sorted {
			v := sorted[i]
			if v.Yanked {
				continue
			}
			sv, _ := ParseSemver(v.Version)
			if sv.Pre == "" {
				return v, nil
			}
			if pre == nil && prerelease {
				pre = &sorted[i]
			}
		}
		if pre != nil {
			return *pre, nil
		}
		return Version{}, fmt.Errorf("plugin %s has no installable release (yanked or pre-release only)", p.Name)
	}
	if _, err := ParseSemver(spec); err != nil {
		return Version{}, fmt.Errorf("version %q: %w (ranges are not supported; use an exact version or latest)", spec, err)
	}
	for _, v := range sorted {
		if v.Version == spec {
			return v, nil
		}
	}
	known := make([]string, len(sorted))
	for i, v := range sorted {
		known[i] = v.Version
	}
	return Version{}, fmt.Errorf("plugin %s has no version %s (available: %s)", p.Name, spec, strings.Join(known, ", "))
}

// Latest is the newest non-yanked stable version, or false.
func (p *Plugin) Latest() (Version, bool) {
	v, err := p.Resolve("", false)
	return v, err == nil
}

// ---- semantic versions ----------------------------------------------------

var semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// Semver is a parsed semantic version (build metadata is ignored for
// precedence, as the specification says).
type Semver struct {
	Major, Minor, Patch uint64
	Pre                 string
}

// ParseSemver parses a strict semantic version.
func ParseSemver(s string) (Semver, error) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return Semver{}, fmt.Errorf("%q is not a semantic version", s)
	}
	var v Semver
	nums := [3]*uint64{&v.Major, &v.Minor, &v.Patch}
	for i, n := range nums {
		x, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return Semver{}, fmt.Errorf("%q: version number out of range", s)
		}
		*n = x
	}
	v.Pre = m[4]
	return v, nil
}

// Compare orders versions by semver precedence: -1, 0 or 1.
func (a Semver) Compare(b Semver) int {
	for _, p := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1 // a release outranks its pre-releases
	case b.Pre == "":
		return -1
	}
	ap, bp := strings.Split(a.Pre, "."), strings.Split(b.Pre, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if c := comparePre(ap[i], bp[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(ap) < len(bp):
		return -1
	case len(ap) > len(bp):
		return 1
	}
	return 0
}

func comparePre(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aerr == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// CompareVersions compares two version strings; an unparsable string sorts
// lowest so a corrupt installed manifest never blocks an update.
func CompareVersions(a, b string) int {
	av, aerr := ParseSemver(a)
	bv, berr := ParseSemver(b)
	switch {
	case aerr != nil && berr != nil:
		return 0
	case aerr != nil:
		return -1
	case berr != nil:
		return 1
	}
	return av.Compare(bv)
}
