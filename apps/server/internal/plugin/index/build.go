package index

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/bundle"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// Meta is optional per-release metadata the bundle cannot carry.
type Meta struct {
	Certification string `json:"certification,omitempty"`
	PublishedAt   string `json:"published_at,omitempty"`
	Yanked        bool   `json:"yanked,omitempty"`
	YankedReason  string `json:"yanked_reason,omitempty"`
}

// BuildOptions configures Build.
type BuildOptions struct {
	// Dir holds the .ogc bundles (not searched recursively).
	Dir string
	// BaseURL, when set, prefixes every bundle url ("https://host/path").
	// When empty the urls are bare file names, relative to the index, which
	// makes the directory a self-contained mirror.
	BaseURL string
	Name    string
	// TrustStore verifies every bundle's signature and supplies publisher
	// names. A bundle whose signature is not trusted is refused unless
	// SkipVerify is set (then the publisher is the key id).
	TrustStore string
	SkipVerify bool
	// GeneratedAt stamps the index; the zero time means now.
	GeneratedAt time.Time
	// Meta is keyed by "name@version". A key matching no bundle is an error.
	Meta map[string]Meta
}

// BuildResult is the generated index plus things worth telling the publisher.
type BuildResult struct {
	Index    *Index
	Warnings []string
}

// Build generates an index from a directory of signed v2 plugin bundles.
// The same bundles always yield the same index (apart from generated_at).
func Build(opts BuildOptions) (*BuildResult, error) {
	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	base := ""
	if opts.BaseURL != "" {
		u, err := url.Parse(opts.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("--base-url %q must be an http(s) URL", opts.BaseURL)
		}
		base = strings.TrimRight(opts.BaseURL, "/")
	}
	var warnings []string
	byName := map[string]*Plugin{}
	usedMeta := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ogc") {
			continue
		}
		file := filepath.Join(opts.Dir, e.Name())
		info, err := bundle.Inspect(file)
		if err != nil {
			if errors.Is(err, bundle.ErrVerifyV1) {
				warnings = append(warnings, e.Name()+": skipped, v1 connector bundles are not listed in the plugin index")
				continue
			}
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		p := info.Plugin
		var env struct {
			Algorithm string `json:"algorithm"`
			KeyID     string `json:"key_id"`
			Digest    string `json:"manifest_sha256"`
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal(info.Envelope, &env); err != nil {
			return nil, fmt.Errorf("%s: signature envelope: %w", e.Name(), err)
		}
		publisher := env.KeyID
		if !opts.SkipVerify {
			res := signing.VerifyPayload(info.Canonical, info.Envelope, opts.TrustStore)
			if res.Status != signing.StatusTrusted {
				return nil, fmt.Errorf("%s: signature is %s%s; clients would refuse it (add the publisher key to --trust-store, or pass --skip-verify)",
					e.Name(), res.Status, errSuffix(res.Error))
			}
			publisher = fmt.Sprint(res.Publisher)
		}
		var ref string
		if base != "" {
			ref = base + "/" + url.PathEscape(e.Name())
		} else {
			ref = (&url.URL{Path: e.Name()}).String()
		}
		v := Version{
			Version: p.Version, DisplayName: p.DisplayName, Description: strings.TrimSpace(p.Description),
			Kind: p.Kind, Runtime: p.Runtime, Author: p.Author, License: p.License, Homepage: p.Homepage,
			Tags: p.Tags,
			Capabilities: Capabilities{
				Network: nonNil(p.Capabilities.Network), Secrets: nonNil(p.Capabilities.Secrets), Browser: p.Capabilities.Browser,
			},
			URL: ref, SHA256: info.SHA256, Size: info.Size, Publisher: publisher,
			Signature: Signature{Algorithm: env.Algorithm, KeyID: env.KeyID, ManifestSHA256: env.Digest, Signature: env.Signature},
		}
		key := p.Name + "@" + p.Version
		if m, ok := opts.Meta[key]; ok {
			usedMeta[key] = true
			v.Certification, v.PublishedAt, v.Yanked, v.YankedReason = m.Certification, m.PublishedAt, m.Yanked, m.YankedReason
		}
		entry := byName[p.Name]
		if entry == nil {
			entry = &Plugin{Name: p.Name}
			byName[p.Name] = entry
		}
		for _, other := range entry.Versions {
			if other.Version == p.Version {
				return nil, fmt.Errorf("%s: %s is already provided by %s", e.Name(), key, other.URL)
			}
		}
		entry.Versions = append(entry.Versions, v)
	}
	var unused []string
	for k := range opts.Meta {
		if !usedMeta[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return nil, fmt.Errorf("metadata given for releases with no bundle: %s", strings.Join(unused, ", "))
	}
	ix := &Index{IndexVersion: FormatVersion, Name: opts.Name, Plugins: []Plugin{}}
	for _, p := range byName {
		ix.Plugins = append(ix.Plugins, Plugin{Name: p.Name, Versions: p.Sorted()})
	}
	sort.Slice(ix.Plugins, func(i, j int) bool { return ix.Plugins[i].Name < ix.Plugins[j].Name })
	at := opts.GeneratedAt
	if at.IsZero() {
		at = time.Now()
	}
	ix.GeneratedAt = at.UTC().Format(time.RFC3339)
	if err := ix.Validate(); err != nil {
		return nil, fmt.Errorf("generated index is invalid: %w", err)
	}
	if len(ix.Plugins) == 0 {
		warnings = append(warnings, "no .ogc plugin bundles found in "+opts.Dir)
	}
	return &BuildResult{Index: ix, Warnings: warnings}, nil
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Encode renders the index as the exact bytes that get signed and served:
// two-space indented JSON with a trailing newline.
func Encode(ix *Index) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep "->" and "&" readable in descriptions
	if err := enc.Encode(ix); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write stores the index at out atomically. With key it also writes
// <out>.sig (the signature covers the exact file bytes); without one a stale
// <out>.sig from an earlier build is removed so it cannot mismatch.
func Write(out string, ix *Index, key ed25519.PrivateKey, keyID string) error {
	doc, err := Encode(ix)
	if err != nil {
		return err
	}
	var sig []byte
	if key != nil {
		if sig, err = signing.SignPayload(doc, key, keyID); err != nil {
			return err
		}
	}
	// Publish the signature first: a client that fetches the new document
	// sees a matching signature, and one that races sees an old document
	// with a new signature (a refusal, never an unverified install).
	if sig != nil {
		if err := writeAtomic(out+".sig", sig); err != nil {
			return err
		}
	} else if err := os.Remove(out + ".sig"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomic(out, doc)
}

func writeAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+path.Base(filepath.ToSlash(p))+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
