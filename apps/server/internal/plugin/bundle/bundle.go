// Package bundle builds and installs signed .ogc plugin packages.
//
// v1 (connector) bundles are byte-compatible in layout with the Python
// connector-package / connector-install commands: a deterministic ZIP with
// exactly "connector.yaml" and "connector.yaml.sig" (1980-01-01 timestamps,
// deflate, mode 0644), installed to <destination>/<capability>/<name>.yaml.
//
// v2 bundles contain "plugin.yaml", "plugin.yaml.sig", the wasm module named
// by wasm.module (if any) and fixtures/**, and install to
// <destination>/<name>/. The Ed25519 signature covers plugin.yaml; plugin.yaml
// pins the module's SHA-256, which install verifies, so the executable code is
// covered by the signature transitively. Fixtures are test data only and are
// not covered by the signature.
package bundle

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// Limits.
const (
	V1MaxFileBytes  = 1_000_000 // Python install_bundle limit
	V2MaxFileBytes  = 16 << 20
	V2MaxTotalBytes = 64 << 20
	V2MaxEntries    = 2000

	v1Manifest        = "connector.yaml"
	v1Signature       = "connector.yaml.sig"
	v2Manifest        = "plugin.yaml"
	v2Signature       = "plugin.yaml.sig"
	fixturesDirPrefix = "fixtures/"
)

// InstallLockWait bounds waiting for a concurrent install of the same plugin
// (INSTALL_LOCK_TIMEOUT_SECONDS in Python).
var InstallLockWait = 10 * time.Second

var (
	zipEpoch      = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	capabilityRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	errNotSigned  = errors.New("sign the connector before packaging it")
	errUnexpected = errors.New("connector bundle must contain only connector.yaml and connector.yaml.sig")
)

type entry struct {
	name string
	data []byte
}

func writeZip(output string, entries []entry) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, flate.BestCompression)
	})
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: zipEpoch}
		h.CreatorVersion = 3<<8 | 20 // Unix, like Python's ZipInfo on POSIX
		h.ExternalAttrs = 0o100644 << 16
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	tmp := output + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, output)
}

// DefaultOutput mirrors Path.with_suffix(".ogc").
func DefaultOutput(manifestPath string) string {
	ext := filepath.Ext(manifestPath)
	return strings.TrimSuffix(manifestPath, ext) + ".ogc"
}

// PackV1 mirrors package_manifest(): manifest + detached signature.
func PackV1(manifestPath, output string) (string, error) {
	sig, err := os.ReadFile(manifestPath + ".sig")
	if err != nil {
		return "", errNotSigned
	}
	m, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	if output == "" {
		output = DefaultOutput(manifestPath)
	}
	return output, writeZip(output, []entry{{v1Manifest, m}, {v1Signature, sig}})
}

// Pack builds a bundle for a plugin path: v1 manifests produce the Python
// compatible layout, v2 plugin directories the v2 layout.
func Pack(pluginPath, output string) (string, error) {
	file, err := manifest.Resolve(pluginPath)
	if err != nil {
		return "", err
	}
	p, err := manifest.Load(file)
	if err != nil {
		return "", err
	}
	if p.ManifestVersion == "1" {
		return PackV1(file, output)
	}
	sig, err := os.ReadFile(file + ".sig")
	if err != nil {
		return "", errors.New("sign the plugin before packaging it")
	}
	if filepath.Base(file) != v2Manifest {
		return "", fmt.Errorf("v2 plugins must name their manifest %s", v2Manifest)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	entries := []entry{{v2Manifest, text}, {v2Signature, sig}}
	if p.Wasm != nil {
		mod, err := os.ReadFile(filepath.Join(p.Dir, filepath.FromSlash(p.Wasm.Module)))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(mod)
		if hex.EncodeToString(sum[:]) != p.Wasm.SHA256 {
			return "", errors.New("wasm module sha256 does not match the manifest; rebuild or update wasm.sha256")
		}
		entries = append(entries, entry{p.Wasm.Module, mod})
	}
	fixtures := filepath.Join(p.Dir, "fixtures")
	var names []string
	_ = filepath.WalkDir(fixtures, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(p.Dir, fp)
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(p.Dir, filepath.FromSlash(n)))
		if err != nil {
			return "", err
		}
		entries = append(entries, entry{n, data})
	}
	if output == "" {
		output = filepath.Join(filepath.Dir(p.Dir), p.Name+"-"+p.Version+".ogc")
	}
	return output, writeZip(output, entries)
}

// InstallResult mirrors the dict returned by install_bundle().
type InstallResult struct {
	ID         string         `json:"id"`
	Capability string         `json:"capability,omitempty"`
	Manifest   string         `json:"manifest"`
	Signature  signing.Result `json:"signature"`
	Version    string         `json:"version,omitempty"`
}

func readEntries(bundle string, maxFile, maxTotal int64, maxEntries int) (map[string][]byte, []string, error) {
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()
	if len(zr.File) > maxEntries {
		return nil, nil, fmt.Errorf("bundle has too many entries (%d)", len(zr.File))
	}
	out := map[string][]byte{}
	var order []string
	var total int64
	for _, f := range zr.File {
		if _, dup := out[f.Name]; dup {
			return nil, nil, fmt.Errorf("bundle contains duplicate entry %q", f.Name)
		}
		if int64(f.UncompressedSize64) > maxFile || int64(f.CompressedSize64) > maxFile {
			return nil, nil, fmt.Errorf("bundle entry %q exceeds the %d byte file limit", f.Name, maxFile)
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("bundle entry %q is a symlink", f.Name)
		}
		if strings.HasSuffix(f.Name, "/") {
			continue // directory entry
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxFile+1))
		rc.Close()
		if err != nil {
			return nil, nil, err
		}
		if int64(len(data)) > maxFile {
			return nil, nil, fmt.Errorf("bundle entry %q exceeds the %d byte file limit", f.Name, maxFile)
		}
		total += int64(len(data))
		if total > maxTotal {
			return nil, nil, errors.New("bundle exceeds the total size limit")
		}
		out[f.Name] = data
		order = append(order, f.Name)
	}
	return out, order, nil
}

// Install installs a v1 or v2 bundle after verifying a trusted signature and
// the full manifest contract in a staging directory.
func Install(bundle, destination, trustStore string, replace bool) (*InstallResult, error) {
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		return nil, err
	}
	isV2 := false
	for _, f := range zr.File {
		if f.Name == v2Manifest {
			isV2 = true
		}
	}
	zr.Close()
	if isV2 {
		return installV2(bundle, destination, trustStore, replace)
	}
	return InstallV1(bundle, destination, trustStore, replace)
}

// InstallV1 mirrors install_bundle().
func InstallV1(bundle, destination, trustStore string, replace bool) (*InstallResult, error) {
	// Python checks the entry names before sizes; keep that order.
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		return nil, err
	}
	names := map[string]int{}
	for _, f := range zr.File {
		names[f.Name]++
	}
	zr.Close()
	if len(names) != 2 || names[v1Manifest] == 0 || names[v1Signature] == 0 {
		return nil, errUnexpected
	}
	files, _, err := readEntries(bundle, V1MaxFileBytes, 2*V1MaxFileBytes, 16)
	if err != nil {
		if strings.Contains(err.Error(), "file limit") {
			return nil, errors.New("connector bundle exceeds the 1 MB file limit")
		}
		return nil, err
	}
	stage, err := os.MkdirTemp("", "opengtm-connector-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	mPath := filepath.Join(stage, v1Manifest)
	if err := os.WriteFile(mPath, files[v1Manifest], 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(mPath+".sig", files[v1Signature], 0o644); err != nil {
		return nil, err
	}
	result := signing.VerifyManifest(mPath, trustStore)
	if result.Status != signing.StatusTrusted {
		return nil, fmt.Errorf("connector signature is %s", result.Status)
	}
	rep := manifest.ValidateDirectory(stage, manifest.DirectoryOptions{SignaturePolicy: signing.PolicyRequired, TrustStore: trustStore})
	if !rep.OK {
		return nil, errors.New(rep.Errors[0].Error)
	}
	m := rep.Connectors[0].Manifest
	if !capabilityRE.MatchString(m.Capability) {
		return nil, errors.New("connector capability is not a safe package path")
	}
	targetDir := filepath.Join(destination, m.Capability)
	target := filepath.Join(targetDir, m.Name+".yaml")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, err
	}
	unlock, err := lockFile(filepath.Join(targetDir, "."+m.Name+".install.lock"), InstallLockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if !replace && (exists(target) || exists(target+".sig")) {
		return nil, fmt.Errorf("connector %s is already installed", m.Name)
	}
	staged := filepath.Join(targetDir, fmt.Sprintf(".%s.%s.yaml", m.Name, randHex(8)))
	defer os.Remove(staged)
	defer os.Remove(staged + ".sig")
	if err := os.WriteFile(staged, files[v1Manifest], 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(staged+".sig", files[v1Signature], 0o644); err != nil {
		return nil, err
	}
	prevM, errM := os.ReadFile(target)
	prevS, errS := os.ReadFile(target + ".sig")
	if err := replacePair(staged, target); err != nil {
		// Restore the exact prior pair, or remove a half-installed new pair.
		restore(target, prevM, errM == nil)
		restore(target+".sig", prevS, errS == nil)
		return nil, err
	}
	return &InstallResult{ID: m.Name, Capability: m.Capability, Manifest: target, Signature: result}, nil
}

// replaceHook lets tests inject a failing rename.
var replaceHook = os.Rename

func replacePair(staged, target string) error {
	if err := replaceHook(staged+".sig", target+".sig"); err != nil {
		return err
	}
	return replaceHook(staged, target)
}

func restore(path string, prev []byte, had bool) {
	if had {
		_ = os.WriteFile(path, prev, 0o644)
	} else {
		_ = os.Remove(path)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func safeEntryName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") || path.Clean(name) != name {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func installV2(bundle, destination, trustStore string, replace bool) (*InstallResult, error) {
	files, order, err := readEntries(bundle, V2MaxFileBytes, V2MaxTotalBytes, V2MaxEntries)
	if err != nil {
		return nil, err
	}
	if files[v2Manifest] == nil || files[v2Signature] == nil {
		return nil, errors.New("plugin bundle must contain plugin.yaml and plugin.yaml.sig")
	}
	p, err := manifest.LoadBytes(v2Manifest, files[v2Manifest])
	if err != nil {
		return nil, err
	}
	if p.ManifestVersion != "2" {
		return nil, errors.New("plugin.yaml must be manifest_version 2")
	}
	for _, name := range order {
		allowed := name == v2Manifest || name == v2Signature ||
			(p.Wasm != nil && name == p.Wasm.Module) ||
			strings.HasPrefix(name, fixturesDirPrefix)
		if !allowed || !safeEntryName(name) {
			return nil, fmt.Errorf("plugin bundle contains unexpected entry %q", name)
		}
	}
	stage, err := os.MkdirTemp(destinationOrTemp(destination), ".opengtm-plugin-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	for _, name := range order {
		target := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, files[name], 0o644); err != nil {
			return nil, err
		}
	}
	mPath := filepath.Join(stage, v2Manifest)
	result := signing.VerifyManifest(mPath, trustStore)
	if result.Status != signing.StatusTrusted {
		return nil, fmt.Errorf("plugin signature is %s", result.Status)
	}
	if p.Wasm != nil {
		mod := files[p.Wasm.Module]
		if mod == nil {
			return nil, fmt.Errorf("plugin bundle is missing its wasm module %s", p.Wasm.Module)
		}
		sum := sha256.Sum256(mod)
		if hex.EncodeToString(sum[:]) != p.Wasm.SHA256 {
			return nil, errors.New("wasm module sha256 does not match the signed manifest")
		}
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return nil, err
	}
	target := filepath.Join(destination, p.Name)
	unlock, err := lockFile(filepath.Join(destination, "."+p.Name+".install.lock"), InstallLockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if exists(target) && !replace {
		return nil, fmt.Errorf("plugin %s is already installed", p.Name)
	}
	backup := ""
	if exists(target) {
		backup = filepath.Join(destination, fmt.Sprintf(".%s.%s.previous", p.Name, randHex(8)))
		if err := os.Rename(target, backup); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(stage, target); err != nil {
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return nil, err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return &InstallResult{ID: p.Name, Manifest: filepath.Join(target, v2Manifest), Signature: result, Version: p.Version}, nil
}

// destinationOrTemp stages next to the destination so the final rename is
// atomic on the same filesystem.
func destinationOrTemp(dest string) string {
	if err := os.MkdirAll(dest, 0o755); err == nil {
		return dest
	}
	return ""
}
