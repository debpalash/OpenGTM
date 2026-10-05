// Package pluginrun executes installed plugins as durable plugin_run jobs and
// serves the /api/v2/plugins and /api/v2/plugin-runs endpoints.
package pluginrun

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// Entry is one installed plugin.
type Entry struct {
	Plugin    *manifest.Plugin
	Signature signing.Result
	// Source is "plugin" for v2 plugin directories and "connector" for v1
	// connector manifests.
	Source string
}

// LoadError is a plugin that was found but not installed.
type LoadError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Catalog is the set of plugins this process may run. It is loaded once at
// startup; installing a plugin means adding its directory and restarting.
type Catalog struct {
	byName map[string]*Entry
	Errors []LoadError
}

// skipDirs are never searched below a plugin root: fixtures hold recorded
// responses, examples are opt-in (point a root at them directly), and build
// output can contain stray YAML.
var skipDirs = map[string]bool{
	"fixtures": true, "examples": true, "target": true, "node_modules": true,
}

// LoadCatalog finds v2 plugins under cfg.Dirs and v1 connectors in
// cfg.ConnectorDirs. Rejected plugins are reported in Errors, never run.
func LoadCatalog(cfg config.Plugins) *Catalog {
	c := &Catalog{byName: map[string]*Entry{}}
	for _, root := range cfg.Dirs {
		c.loadPluginRoot(root, cfg)
	}
	for _, dir := range cfg.ConnectorDirs {
		rep := manifest.ValidateDirectory(dir, manifest.DirectoryOptions{
			SignaturePolicy: cfg.SignaturePolicy, TrustStore: cfg.TrustStore,
		})
		for _, e := range rep.Errors {
			c.Errors = append(c.Errors, LoadError{Path: e.Path, Error: e.Error})
		}
		for _, e := range rep.Connectors {
			p := manifest.FromV1(e.Manifest)
			if p.Path == "" {
				p.Path, p.Dir = e.Path, filepath.Dir(e.Path)
			}
			c.add(&Entry{Plugin: p, Signature: e.Signature, Source: "connector"}, e.Path)
		}
	}
	return c
}

func (c *Catalog) loadPluginRoot(root string, cfg config.Plugins) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			c.Errors = append(c.Errors, LoadError{Path: path, Error: err.Error()})
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
			return filepath.SkipDir
		}
		file, err := manifest.Resolve(path)
		if err != nil {
			return nil // not a plugin directory; keep looking below it
		}
		c.loadPlugin(file, cfg)
		return filepath.SkipDir // a plugin's own subdirectories are its assets
	})
	if err != nil {
		c.Errors = append(c.Errors, LoadError{Path: root, Error: err.Error()})
	}
}

func (c *Catalog) loadPlugin(file string, cfg config.Plugins) {
	p, err := manifest.Load(file)
	if err != nil {
		c.Errors = append(c.Errors, LoadError{Path: file, Error: err.Error()})
		return
	}
	sig := signing.VerifyManifest(file, cfg.TrustStore)
	if signing.Rejects(cfg.SignaturePolicy, sig) {
		msg := "signature " + sig.Status
		if sig.Error != "" {
			msg += ": " + sig.Error
		}
		c.Errors = append(c.Errors, LoadError{Path: file, Error: msg + " (policy " + cfg.SignaturePolicy + ")"})
		return
	}
	c.add(&Entry{Plugin: p, Signature: sig, Source: "plugin"}, file)
}

func (c *Catalog) add(e *Entry, path string) {
	if prev, dup := c.byName[e.Plugin.Name]; dup {
		c.Errors = append(c.Errors, LoadError{
			Path:  path,
			Error: "duplicate plugin name " + e.Plugin.Name + " (already loaded from " + prev.Plugin.Path + ")",
		})
		return
	}
	c.byName[e.Plugin.Name] = e
}

// HasRuntime reports whether any installed plugin uses the given runtime.
func (c *Catalog) HasRuntime(runtime string) bool {
	for _, e := range c.byName {
		if e.Plugin.Runtime == runtime {
			return true
		}
	}
	return false
}

// Get returns an installed plugin by name.
func (c *Catalog) Get(name string) (*Entry, bool) {
	e, ok := c.byName[name]
	return e, ok
}

// List returns installed plugins sorted by name.
func (c *Catalog) List() []*Entry {
	out := make([]*Entry, 0, len(c.byName))
	for _, e := range c.byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Plugin.Name < out[j].Plugin.Name })
	return out
}
