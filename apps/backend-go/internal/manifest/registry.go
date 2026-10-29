package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one loaded manifest with the sha256 of its canonical form, the
// value the worker and spend contracts carry (go_executor.load_manifest_index).
type Entry struct {
	Manifest Manifest
	SHA256   string
	// MappingKeys is response.mappings in authored order (Go maps lose it);
	// set by Decode only.
	MappingKeys []string
}

// ErrPathTraversal is returned for a manifests directory that contains a ".."
// element.
var ErrPathTraversal = errors.New("manifest: directory must not contain '..'")

// normalizePolicy is manifest._signature_policy on an already read value:
// strip + lower, unknown values (including empty) mean "required".
func normalizePolicy(policy string) string {
	p := strings.ToLower(PyStrip(policy))
	if p == "optional" || p == "required" {
		return p
	}
	return "required"
}

// LoadIndex equals go_executor.load_manifest_index over dir: every
// *.y*ml file (recursive, Python Path ordering, later duplicates win) whose
// signature is neither invalid nor untrusted (and trusted under policy
// "required") and whose model validates. One bad manifest is logged and
// skipped. A missing dir is an empty index. Symlinked files and directories
// are never followed.
func LoadIndex(dir string, policy string, trust TrustStore) (map[string]Entry, error) {
	for _, part := range strings.FieldsFunc(dir, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part == ".." {
			return nil, ErrPathTraversal
		}
	}
	policy = normalizePolicy(policy)
	index := map[string]Entry{}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return index, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return index, nil
	}
	var paths []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow: a link could point outside dir
		}
		if d.Type().IsRegular() {
			if ok, _ := path.Match("*.y*ml", d.Name()); ok {
				paths = append(paths, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortPyPaths(paths)
	for _, p := range paths {
		entry, err := loadEntry(p, policy, trust)
		if err != nil {
			slog.Warn("manifest: skipping manifest", "path", p, "error", err.Error())
			continue
		}
		index[entry.Manifest.Name] = entry
	}
	return index, nil
}

// sortPyPaths orders like sorted(Path): component-wise, so "a/b.yaml" sorts
// before "a.yaml" (plain string order would not).
func sortPyPaths(paths []string) {
	sort.Slice(paths, func(i, j int) bool {
		a := strings.Split(filepath.ToSlash(paths[i]), "/")
		b := strings.Split(filepath.ToSlash(paths[j]), "/")
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
}

func loadEntry(p, policy string, trust TrustStore) (Entry, error) {
	// One read: the bytes that are verified are the bytes that are hashed and decoded.
	data, err := readManifest(p)
	if err != nil {
		return Entry{}, err
	}
	res := VerifyData(data, p+".sig", trust)
	if res.Status == SigInvalid || res.Status == SigUntrusted || (policy == "required" && res.Status != SigTrusted) {
		return Entry{}, fmt.Errorf("connector signature is %s", res.Status)
	}
	m, canonical, _, err := decodeManifest(data)
	if err != nil {
		return Entry{}, err
	}
	sum := sha256.Sum256(canonical)
	return Entry{Manifest: m, SHA256: hex.EncodeToString(sum[:])}, nil
}

// decodeManifest is load_manifest + canonical_manifest on the same bytes:
// the YAML mapping gets the body -> body_template alias, then must satisfy
// ProviderManifest (version "1", required name, capability, request.url).
func decodeManifest(data []byte) (Manifest, []byte, []string, error) {
	tree, err := loadYAML(data)
	if err != nil {
		return Manifest{}, nil, nil, err
	}
	root, ok := tree.(*pyMap)
	if !ok {
		return Manifest{}, nil, nil, errNotMapping
	}
	var canonical bytes.Buffer
	if err := dumpValue(&canonical, root); err != nil {
		return Manifest{}, nil, nil, err
	}
	// Coerce on a copy: the hash is over the manifest as authored.
	raw := &pyMap{ents: append([]pyEnt(nil), root.ents...)}
	var req *pyMap
	if cur, present := pyGet(raw, "request"); present && cur != nil {
		c, isMap := cur.(*pyMap)
		if !isMap {
			return Manifest{}, nil, nil, errors.New("request: Input should be a valid dictionary")
		}
		req = &pyMap{ents: append([]pyEnt(nil), c.ents...)}
	} else {
		req = &pyMap{}
	}
	if body, has := pyGet(req, "body"); has {
		if _, hasTpl := pyGet(req, "body_template"); !hasTpl {
			req.ents = removeKey(req.ents, "body")
			req.set("body_template", body)
		}
	}
	raw.set("request", req)
	if v, present := pyGet(raw, "manifest_version"); present {
		if s, isStr := v.(string); !isStr || s != "1" {
			return Manifest{}, nil, nil, errors.New("manifest_version: Input should be '1'")
		}
	}
	for _, f := range [][2]any{{raw, "name"}, {raw, "capability"}, {req, "url"}} {
		if v, present := pyGet(f[0].(*pyMap), f[1].(string)); !present || v == nil {
			return Manifest{}, nil, nil, fmt.Errorf("%s: Field required", f[1])
		}
	}
	var js bytes.Buffer
	if err := dumpValue(&js, raw); err != nil {
		return Manifest{}, nil, nil, err
	}
	var m Manifest
	if err := json.Unmarshal(js.Bytes(), &m); err != nil {
		return Manifest{}, nil, nil, fmt.Errorf("manifest: %w", err)
	}
	switch m.Auth.Type {
	case "header", "bearer", "query", "none":
	default:
		return Manifest{}, nil, nil, errors.New("1 validation error for ProviderManifest\nauth.type\n  Input should be 'header', 'bearer', 'query' or 'none'")
	}
	var keys []string
	if resp, ok := pyGet(root, "response"); ok {
		if rm, ok := resp.(*pyMap); ok {
			if mp, ok := pyGet(rm, "mappings"); ok {
				if mm, ok := mp.(*pyMap); ok {
					for _, ent := range mm.ents {
						if k, ok := ent.key.(string); ok {
							keys = append(keys, k)
						}
					}
				}
			}
		}
	}
	return m, canonical.Bytes(), keys, nil
}

func pyGet(m *pyMap, key string) (any, bool) {
	for _, e := range m.ents {
		if s, ok := e.key.(string); ok && s == key {
			return e.val, true
		}
	}
	return nil, false
}

func removeKey(ents []pyEnt, key string) []pyEnt {
	out := ents[:0:0]
	for _, e := range ents {
		if s, ok := e.key.(string); ok && s == key {
			continue
		}
		out = append(out, e)
	}
	return out
}
