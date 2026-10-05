package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/bundle"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// Client searches an index and installs from it into a plugin root.
type Client struct {
	// Source is the index location (see OpenSource).
	Source string
	// Root is the install root: plugins live in <Root>/<name>/.
	Root string
	// TrustStore is the trusted-publisher JSON file.
	TrustStore string
	// Policy is "optional" or "required" (CONNECTOR_SIGNATURE_POLICY).
	Policy string
	HTTP   *http.Client
	// UserAgent is sent on every download.
	UserAgent string
	// AllowDowngrade permits installing an older version over a newer one.
	// It is ignored under the required policy: roll back from a local
	// bundle file instead, which is an explicit offline operator action.
	AllowDowngrade bool
	// AllowPrerelease lets "latest" select a pre-release when there is no
	// stable release.
	AllowPrerelease bool
}

func (c *Client) policy() string { return signing.NormalizePolicy(c.Policy) }

// Loaded is a verified index.
type Loaded struct {
	Index  *Index
	Source *Source
	// Signature is "trusted" when index.json.sig verified against the trust
	// store, or "unsigned".
	Signature signing.Result
}

// Load fetches the index and its signature. A present signature must verify
// against a trusted key whatever the policy; under the required policy the
// signature must be present.
func (c *Client) Load(ctx context.Context) (*Loaded, error) {
	src, err := OpenSource(c.Source, c.HTTP, c.UserAgent)
	if err != nil {
		return nil, err
	}
	doc, sig, err := src.FetchIndex(ctx)
	if err != nil {
		return nil, err
	}
	l := &Loaded{Source: src, Signature: signing.Result{Status: signing.StatusUnsigned}}
	if sig != nil {
		res := signing.VerifyPayload(doc, sig, c.TrustStore)
		if res.Status != signing.StatusTrusted {
			msg := fmt.Sprintf("plugin index signature is %s", res.Status)
			if res.Error != "" {
				msg += ": " + res.Error
			}
			return nil, errors.New(msg)
		}
		l.Signature = res
	} else if c.policy() == signing.PolicyRequired {
		return nil, errors.New("plugin index is not signed (" + src.Location + ".sig is missing) and CONNECTOR_SIGNATURE_POLICY is required")
	}
	if l.Index, err = Parse(doc); err != nil {
		return nil, err
	}
	return l, nil
}

// SplitSpec splits name[@version].
func SplitSpec(spec string) (name, version string, err error) {
	name, version, _ = strings.Cut(strings.TrimSpace(spec), "@")
	if !nameRE.MatchString(name) {
		return "", "", fmt.Errorf("%q is not a plugin name (want name or name@version)", spec)
	}
	if strings.Contains(version, "@") {
		return "", "", fmt.Errorf("%q: more than one @", spec)
	}
	return name, version, nil
}

// Hit is one search result.
type Hit struct {
	Name          string `json:"name"`
	Latest        string `json:"latest"`
	Kind          string `json:"kind"`
	Runtime       string `json:"runtime"`
	Description   string `json:"description"`
	Publisher     string `json:"publisher"`
	Certification string `json:"certification"`
	Installed     string `json:"installed,omitempty"`
}

// Search lists plugins whose name, display name, description, kind, tags or
// publisher contain every word of query (case-insensitive); an empty query
// lists everything.
func (c *Client) Search(ctx context.Context, query string) ([]Hit, error) {
	l, err := c.Load(ctx)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	installed := map[string]string{}
	if list, err := c.List(); err == nil {
		for _, p := range list {
			installed[p.Name] = p.Version
		}
	}
	hits := []Hit{}
	for _, p := range l.Index.Plugins {
		v, ok := p.Latest()
		if !ok {
			v = p.Sorted()[0]
		}
		hay := strings.ToLower(strings.Join(append([]string{p.Name, v.DisplayName, v.Description, v.Kind, v.Publisher}, v.Tags...), " "))
		match := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		cert := v.Certification
		if cert == "" {
			cert = CertUnreviewed
		}
		hits = append(hits, Hit{
			Name: p.Name, Latest: v.Version, Kind: v.Kind, Runtime: v.Runtime, Description: v.Description,
			Publisher: v.Publisher, Certification: cert, Installed: installed[p.Name],
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
	return hits, nil
}

// Info returns one plugin's entry (all versions, newest first) and the index
// signature state.
func (c *Client) Info(ctx context.Context, name string) (*Plugin, signing.Result, error) {
	l, err := c.Load(ctx)
	if err != nil {
		return nil, signing.Result{}, err
	}
	p, ok := l.Index.Find(name)
	if !ok {
		return nil, signing.Result{}, fmt.Errorf("plugin %q is not in the index %s", name, l.Source.Location)
	}
	out := Plugin{Name: p.Name, Versions: p.Sorted()}
	return &out, l.Signature, nil
}

// Installed is a plugin present in the install root.
type Installed struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Kind      string         `json:"kind"`
	Runtime   string         `json:"runtime"`
	Path      string         `json:"path"`
	Signature signing.Result `json:"signature"`
}

// List reports the v2 plugins installed under Root.
func (c *Client) List() ([]Installed, error) {
	entries, err := os.ReadDir(c.Root)
	if errors.Is(err, os.ErrNotExist) {
		return []Installed{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Installed{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		file := filepath.Join(c.Root, e.Name(), "plugin.yaml")
		p, err := manifest.Load(file)
		if err != nil {
			continue // not a plugin directory
		}
		out = append(out, Installed{
			Name: p.Name, Version: p.Version, Kind: p.Kind, Runtime: p.Runtime,
			Path: filepath.Join(c.Root, e.Name()), Signature: signing.VerifyManifest(file, c.TrustStore),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) installedVersion(name string) (string, bool) {
	p, err := manifest.Load(filepath.Join(c.Root, name, "plugin.yaml"))
	if err != nil {
		if _, serr := os.Lstat(filepath.Join(c.Root, name)); serr == nil {
			return "", true // present but unreadable
		}
		return "", false
	}
	return p.Version, true
}

// Remove uninstalls a plugin from Root.
func (c *Client) Remove(name string) error { return bundle.Remove(c.Root, name) }

// Result describes an install.
type Result struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Previous  string         `json:"previous,omitempty"`
	Path      string         `json:"path"`
	Publisher string         `json:"publisher"`
	KeyID     string         `json:"key_id"`
	SHA256    string         `json:"sha256"`
	Source    string         `json:"source"`
	Signature signing.Result `json:"signature"`
	// IndexSigned reports whether the index itself carried a trusted signature.
	IndexSigned bool     `json:"index_signed"`
	Warnings    []string `json:"warnings,omitempty"`
}

// Install installs name[@version] from the index. Without replace an
// installed plugin is left alone.
func (c *Client) Install(ctx context.Context, spec string, replace bool) (*Result, error) {
	name, want, err := SplitSpec(spec)
	if err != nil {
		return nil, err
	}
	l, err := c.Load(ctx)
	if err != nil {
		return nil, err
	}
	return c.installFrom(ctx, l, name, want, replace)
}

func (c *Client) installFrom(ctx context.Context, l *Loaded, name, want string, replace bool) (*Result, error) {
	p, ok := l.Index.Find(name)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not in the index %s", name, l.Source.Location)
	}
	v, err := p.Resolve(want, c.AllowPrerelease)
	if err != nil {
		return nil, err
	}
	cur, installed := c.installedVersion(name)
	if installed && !replace {
		return nil, fmt.Errorf("plugin %s is already installed (%s); use update, or install with --replace", name, orUnknown(cur))
	}
	if installed {
		if err := c.checkDowngrade(name, cur, v.Version); err != nil {
			return nil, err
		}
	}

	loc, err := l.Source.Resolve(v.URL)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", name, v.Version, err)
	}
	tmp, err := os.CreateTemp("", "opengtm-plugin-*.ogc")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	limit := int64(MaxBundleBytes)
	if v.Size > 0 && v.Size < limit {
		limit = v.Size
	}
	h := sha256.New()
	cnt := &countWriter{}
	if err := l.Source.FetchBundle(ctx, loc, io.MultiWriter(tmp, h, cnt), limit); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("download %s@%s: %w", name, v.Version, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.SHA256 {
		return nil, fmt.Errorf("%s@%s: bundle sha256 %s does not match the index (%s); nothing was installed", name, v.Version, got, v.SHA256)
	}
	if v.Size > 0 && cnt.n != v.Size {
		return nil, fmt.Errorf("%s@%s: bundle is %d bytes, the index says %d; nothing was installed", name, v.Version, cnt.n, v.Size)
	}

	res := &Result{Name: name, Version: v.Version, SHA256: v.SHA256, Source: l.Source.Location, IndexSigned: l.Signature.Status == signing.StatusTrusted}
	if installed {
		res.Previous = cur
	}
	ir, err := bundle.InstallWithOptions(tmp.Name(), c.Root, c.TrustStore, bundle.InstallOptions{
		Replace: replace,
		Verify:  func(cand bundle.Candidate) error { return c.verifyCandidate(cand, name, v, res) },
	})
	if err != nil {
		return nil, err
	}
	res.Path = filepath.Dir(ir.Manifest)
	res.Signature = ir.Signature
	res.KeyID = fmt.Sprint(ir.Signature.KeyID)
	res.Publisher = fmt.Sprint(ir.Signature.Publisher)
	return res, nil
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown version"
	}
	return "version " + v
}

func (c *Client) checkDowngrade(name, current, target string) error {
	if current == "" || CompareVersions(target, current) >= 0 {
		return nil
	}
	switch {
	case c.policy() == signing.PolicyRequired:
		return fmt.Errorf("refusing to downgrade %s from %s to %s under the required signature policy (install a local bundle file to roll back deliberately)", name, current, target)
	case !c.AllowDowngrade:
		return fmt.Errorf("refusing to downgrade %s from %s to %s (pass --allow-downgrade to override)", name, current, target)
	}
	return nil
}

// verifyCandidate cross-checks the bundle against its index entry once its
// signature is verified: the index may not lie about what a bundle is.
func (c *Client) verifyCandidate(cand bundle.Candidate, name string, v Version, res *Result) error {
	if cand.Name != name || cand.Version != v.Version {
		return fmt.Errorf("bundle contains %s@%s but the index lists it as %s@%s; refusing", cand.Name, cand.Version, name, v.Version)
	}
	if cand.Installed {
		if err := c.checkDowngrade(name, cand.InstalledVersion, cand.Version); err != nil {
			return err
		}
	}
	p := cand.Plugin
	if p.Kind != v.Kind || p.Runtime != v.Runtime {
		return fmt.Errorf("index says %s@%s is a %s/%s plugin, its signed manifest says %s/%s; refusing", name, v.Version, v.Kind, v.Runtime, p.Kind, p.Runtime)
	}
	if !sameSet(p.Capabilities.Network, v.Capabilities.Network) || !sameSet(p.Capabilities.Secrets, v.Capabilities.Secrets) ||
		p.Capabilities.Browser != v.Capabilities.Browser {
		return fmt.Errorf("index capabilities for %s@%s differ from its signed manifest; refusing", name, v.Version)
	}
	var env struct {
		Algorithm string `json:"algorithm"`
		KeyID     string `json:"key_id"`
		Digest    string `json:"manifest_sha256"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(cand.Envelope, &env); err != nil {
		return fmt.Errorf("bundle signature envelope: %w", err)
	}
	if env.KeyID != v.Signature.KeyID || env.Digest != v.Signature.ManifestSHA256 ||
		env.Signature != v.Signature.Signature || env.Algorithm != v.Signature.Algorithm {
		return fmt.Errorf("signature in the index for %s@%s does not match the bundle's own signature; refusing", name, v.Version)
	}
	if cand.ManifestSHA256 != v.Signature.ManifestSHA256 {
		return fmt.Errorf("manifest digest of %s@%s does not match the index; refusing", name, v.Version)
	}
	if pub := fmt.Sprint(cand.Signature.Publisher); pub != v.Publisher {
		res.Warnings = append(res.Warnings, fmt.Sprintf("index names publisher %q, the trust store names %q (the trust store is authoritative)", v.Publisher, pub))
	}
	return nil
}

func sameSet(a, b []string) bool {
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// UpdateStatus is the outcome for one plugin of an update.
type UpdateStatus struct {
	Name   string  `json:"name"`
	From   string  `json:"from"`
	To     string  `json:"to,omitempty"`
	Status string  `json:"status"` // updated | would-update | up-to-date | not-in-index | failed
	Error  string  `json:"error,omitempty"`
	Result *Result `json:"result,omitempty"`
}

// Update brings installed plugins (all, or the named ones) to their newest
// release. With dryRun it only reports what would change.
func (c *Client) Update(ctx context.Context, names []string, dryRun bool) ([]UpdateStatus, error) {
	l, err := c.Load(ctx)
	if err != nil {
		return nil, err
	}
	list, err := c.List()
	if err != nil {
		return nil, err
	}
	byName := map[string]Installed{}
	for _, p := range list {
		byName[p.Name] = p
	}
	if len(names) == 0 {
		for _, p := range list {
			names = append(names, p.Name)
		}
	}
	out := []UpdateStatus{}
	for _, name := range names {
		cur, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("plugin %s is not installed under %s", name, c.Root)
		}
		st := UpdateStatus{Name: name, From: cur.Version}
		p, found := l.Index.Find(name)
		if !found {
			st.Status = "not-in-index"
			out = append(out, st)
			continue
		}
		v, err := p.Resolve("", c.AllowPrerelease)
		if err != nil || CompareVersions(v.Version, cur.Version) <= 0 {
			st.Status = "up-to-date"
			out = append(out, st)
			continue
		}
		st.To = v.Version
		if dryRun {
			st.Status = "would-update"
			out = append(out, st)
			continue
		}
		res, err := c.installFrom(ctx, l, name, v.Version, true)
		if err != nil {
			st.Status, st.Error = "failed", err.Error()
		} else {
			st.Status, st.Result = "updated", res
		}
		out = append(out, st)
	}
	return out, nil
}
