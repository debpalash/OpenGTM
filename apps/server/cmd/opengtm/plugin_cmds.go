package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/bundle"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/fixture"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// ---- validate ---------------------------------------------------------------

type validatedPlugin struct {
	Path            string         `json:"path"`
	Name            string         `json:"name"`
	ManifestVersion string         `json:"manifest_version"`
	Kind            string         `json:"kind"`
	Runtime         string         `json:"runtime"`
	Version         string         `json:"version"`
	Capabilities    any            `json:"capabilities"`
	ManifestSHA256  string         `json:"manifest_sha256"`
	Signature       signing.Result `json:"signature"`
}

type validateReport struct {
	OK              bool                   `json:"ok"`
	SignaturePolicy string                 `json:"signature_policy"`
	Count           int                    `json:"count"`
	Plugins         []validatedPlugin      `json:"plugins"`
	Errors          []manifest.ReportError `json:"errors"`
}

func isConnectorDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = manifest.Resolve(path)
	return err != nil // a directory without plugin.yaml holds v1 connectors
}

func pluginValidate(_ context.Context, args []string) error {
	fs := newFlags("validate")
	asJSON := fs.Bool("json", false, "machine-readable report")
	policy := fs.String("signature-policy", "", "optional or required (default $CONNECTOR_SIGNATURE_POLICY or optional)")
	trust := fs.String("trust-store", defaultTrustStore(), "trusted publishers JSON")
	paths, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(paths) == 0 {
		return errUsage
	}
	pol := *policy
	if pol == "" {
		pol = defaultPolicy()
	}
	pol = signing.NormalizePolicy(pol)

	// A single connector directory gets the Python-identical v1 report.
	if len(paths) == 1 && isConnectorDir(paths[0]) {
		rep := manifest.ValidateDirectory(paths[0], manifest.DirectoryOptions{SignaturePolicy: pol, TrustStore: *trust})
		if *asJSON {
			out, err := rep.JSON()
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(out))
		} else {
			status := "PASS"
			if !rep.OK {
				status = "FAIL"
			}
			fmt.Fprintf(stdout, "%s: %d compatible connector(s)\n", status, len(rep.Connectors))
			for _, e := range rep.Errors {
				fmt.Fprintf(stdout, "- %s: %s\n", e.Path, e.Error)
			}
		}
		if !rep.OK {
			return errors.New("validation failed")
		}
		return nil
	}

	rep := validateReport{SignaturePolicy: pol, Plugins: []validatedPlugin{}, Errors: []manifest.ReportError{}}
	names := map[string]string{}
	for _, path := range paths {
		if isConnectorDir(path) {
			sub := manifest.ValidateDirectory(path, manifest.DirectoryOptions{SignaturePolicy: pol, TrustStore: *trust})
			for _, c := range sub.Connectors {
				full := filepath.Join(path, c.Path)
				p := manifest.FromV1(c.Manifest)
				rep.Plugins = append(rep.Plugins, validatedPlugin{full, p.Name, "1", p.Kind, p.Runtime, p.Version, p.Capabilities, c.ManifestSHA256, c.Signature})
			}
			rep.Errors = append(rep.Errors, sub.Errors...)
			continue
		}
		vp, err := validateOne(path, pol, *trust)
		if err == nil && names[vp.Name] != "" {
			err = fmt.Errorf("duplicate plugin name '%s' (also %s)", vp.Name, names[vp.Name])
		}
		if err != nil {
			rep.Errors = append(rep.Errors, manifest.ReportError{Path: path, Error: err.Error()})
			continue
		}
		names[vp.Name] = path
		rep.Plugins = append(rep.Plugins, *vp)
	}
	rep.Count = len(rep.Plugins)
	rep.OK = len(rep.Errors) == 0
	if *asJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		status := "PASS"
		if !rep.OK {
			status = "FAIL"
		}
		fmt.Fprintf(stdout, "%s: %d valid plugin(s)\n", status, rep.Count)
		for _, p := range rep.Plugins {
			fmt.Fprintf(stdout, "  %s %s (%s/%s, v%s) signature=%s\n", p.Name, p.Path, p.Kind, p.Runtime, p.Version, p.Signature.Status)
		}
		for _, e := range rep.Errors {
			fmt.Fprintf(stdout, "- %s: %s\n", e.Path, e.Error)
		}
	}
	if !rep.OK {
		return errors.New("validation failed")
	}
	return nil
}

func validateOne(path, policy, trust string) (*validatedPlugin, error) {
	file, err := manifest.Resolve(path)
	if err != nil {
		return nil, err
	}
	p, err := manifest.Load(file)
	if err != nil {
		return nil, err
	}
	sig := signing.VerifyManifest(file, trust)
	if signing.Rejects(policy, sig) {
		return nil, fmt.Errorf("%s", strings.TrimSpace(fmt.Sprintf("plugin signature is %s: %s", sig.Status, sig.Error)))
	}
	digest, err := signing.Digest(file)
	if err != nil {
		return nil, err
	}
	if p.Wasm != nil {
		data, err := os.ReadFile(filepath.Join(p.Dir, filepath.FromSlash(p.Wasm.Module)))
		if err != nil {
			return nil, fmt.Errorf("wasm module: %w", err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != p.Wasm.SHA256 {
			return nil, errors.New("wasm module sha256 does not match wasm.sha256 (rebuild, or run the plugin's build script)")
		}
	}
	return &validatedPlugin{file, p.Name, p.ManifestVersion, p.Kind, p.Runtime, p.Version, p.Capabilities, digest, sig}, nil
}

// ---- test / dev ---------------------------------------------------------------

func runTests(ctx context.Context, path, only string, asJSON bool) (bool, error) {
	p, err := manifest.Load(path)
	if err != nil {
		return false, err
	}
	cases, err := fixture.LoadCases(p.Dir)
	if err != nil {
		return false, err
	}
	if only != "" {
		var kept []*fixture.Case
		for _, c := range cases {
			if c.Name == only {
				kept = append(kept, c)
			}
		}
		cases = kept
	}
	if len(cases) == 0 {
		return false, fmt.Errorf("%s: no fixtures/<case>/case.yaml found (record one with `opengtm plugin record`)", p.Dir)
	}
	ex, done, err := loadExtractor(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	var results []fixture.Result
	pass := true
	for _, c := range cases {
		r := fixture.RunCase(ctx, p, c, fixture.Deps{Extractor: ex, Version: version})
		results = append(results, r)
		pass = pass && r.Pass
	}
	if asJSON {
		return pass, printJSON(map[string]any{"plugin": p.Name, "pass": pass, "cases": results})
	}
	failed := 0
	for _, r := range results {
		if r.Pass {
			fmt.Fprintf(stdout, "PASS %s\n", r.Case)
		} else {
			failed++
			fmt.Fprintf(stdout, "FAIL %s\n", r.Case)
			for _, prob := range r.Problems {
				fmt.Fprintf(stdout, "     %s\n", prob)
			}
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(stdout, "     warning: %s\n", w)
		}
	}
	fmt.Fprintf(stdout, "%s: %d passed, %d failed\n", p.Name, len(results)-failed, failed)
	return pass, nil
}

func pluginTest(ctx context.Context, args []string) error {
	fs := newFlags("test")
	only := fs.String("case", "", "run a single case")
	asJSON := fs.Bool("json", false, "machine-readable results")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	pass, err := runTests(ctx, pos[0], *only, *asJSON)
	if err != nil {
		return err
	}
	if !pass {
		return errors.New("fixture tests failed")
	}
	return nil
}

// fingerprint summarizes file names, sizes and mtimes under dir.
func fingerprint(dir string) string {
	var parts []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == "target" || d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", p, info.Size(), info.ModTime().UnixNano()))
		}
		return nil
	})
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func pluginDev(ctx context.Context, args []string) error {
	fs := newFlags("dev")
	interval := fs.Duration("interval", time.Second, "poll interval")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) > 1 {
		return errUsage
	}
	path := "."
	if len(pos) == 1 {
		path = pos[0]
	}
	file, err := manifest.Resolve(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	last := ""
	fmt.Fprintf(stdout, "watching %s (Ctrl-C to stop)\n", dir)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if fp := fingerprint(dir); fp != last {
			last = fp
			fmt.Fprintf(stdout, "\n[%s] change detected, running tests\n", time.Now().Format("15:04:05"))
			if _, err := runTests(ctx, dir, "", false); err != nil {
				fmt.Fprintln(stdout, "error:", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ---- record / run -------------------------------------------------------------

func liveClientOptions() egress.Options {
	return egress.Options{Version: version, ProxyURL: os.Getenv("OPENGTM_EGRESS_PROXY")}
}

func pluginRecord(ctx context.Context, args []string) error {
	fs := newFlags("record")
	inputs := kvFlag{}
	fs.Var(inputs, "input", "plugin input key=value (repeatable)")
	inputJSON := fs.String("input-json", "", "inputs as a JSON object")
	var secretEnv listFlag
	fs.Var(&secretEnv, "secret-env", "declared secret read from the environment variable of the same name (repeatable)")
	caseName := fs.String("case", "recorded", "fixture case name")
	overwrite := fs.Bool("overwrite", false, "replace an existing case")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	p, err := manifest.Load(pos[0])
	if err != nil {
		return err
	}
	in, err := coerceInputs(p, inputs, *inputJSON)
	if err != nil {
		return err
	}
	secrets, err := secretsFromEnv(p, secretEnv)
	if err != nil {
		return err
	}
	ex, done, err := loadExtractor(ctx)
	if err != nil {
		return err
	}
	defer done()
	dir, out, err := fixture.Record(ctx, p, fixture.RecordOptions{
		Case: *caseName, Inputs: in, Secrets: secrets, Overwrite: *overwrite,
		Deps: fixture.Deps{Extractor: ex, Version: version}, ClientOptions: liveClientOptions(),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "recorded %s (fields=%d records=%d error=%q)\n", dir, len(out.Fields), len(out.Records), out.Error)
	return nil
}

func pluginRun(ctx context.Context, args []string) error {
	fs := newFlags("run")
	inputs := kvFlag{}
	fs.Var(inputs, "input", "plugin input key=value (repeatable)")
	inputJSON := fs.String("input-json", "", "inputs as a JSON object")
	var secretEnv listFlag
	fs.Var(&secretEnv, "secret-env", "declared secret read from the environment variable of the same name (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	p, err := manifest.Load(pos[0])
	if err != nil {
		return err
	}
	in, err := coerceInputs(p, inputs, *inputJSON)
	if err != nil {
		return err
	}
	secrets, err := secretsFromEnv(p, secretEnv)
	if err != nil {
		return err
	}
	ex, done, err := loadExtractor(ctx)
	if err != nil {
		return err
	}
	defer done()
	client, err := egress.New(liveClientOptions())
	if err != nil {
		return err
	}
	out, err := fixture.Execute(ctx, p, client, in, nil, secrets, fixture.Deps{Extractor: ex, Version: version})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"plugin": p.Name, "version": p.Version, "kind": p.Kind, "inputs": in, "result": out})
}

// ---- pack / sign / verify / install / keygen ---------------------------------

func pluginPack(_ context.Context, args []string) error {
	fs := newFlags("pack")
	output := fs.String("output", "", "bundle path (default: <manifest>.ogc for v1, <name>-<version>.ogc next to the plugin for v2)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	out, err := bundle.Pack(pos[0], *output)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Packaged %s -> %s\n", pos[0], out)
	return nil
}

func pluginSign(_ context.Context, args []string) error {
	fs := newFlags("sign")
	key := fs.String("private-key", "", "PEM (PKCS#8) Ed25519 private key")
	keyID := fs.String("key-id", "", "key id present in the trust store")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 || *key == "" || *keyID == "" {
		return errUsage
	}
	file, err := manifest.Resolve(pos[0])
	if err != nil {
		return err
	}
	if _, err := manifest.Load(file); err != nil {
		return fmt.Errorf("refusing to sign an invalid manifest: %w", err)
	}
	out, err := signing.SignManifest(file, *key, *keyID)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signed %s -> %s\n", file, out)
	return nil
}

func pluginVerify(_ context.Context, args []string) error {
	fs := newFlags("verify")
	trust := fs.String("trust-store", defaultTrustStore(), "trusted publishers JSON")
	policy := fs.String("signature-policy", signing.PolicyRequired, "optional or required")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	file, err := manifest.Resolve(pos[0])
	if err != nil {
		return err
	}
	res := signing.VerifyManifest(file, *trust)
	if err := printJSON(res); err != nil {
		return err
	}
	if signing.Rejects(signing.NormalizePolicy(*policy), res) {
		return fmt.Errorf("signature is %s", res.Status)
	}
	return nil
}

func pluginInstall(_ context.Context, args []string) error {
	fs := newFlags("install")
	dest := fs.String("destination", "", "install root (required)")
	trust := fs.String("trust-store", defaultTrustStore(), "trusted publishers JSON")
	replace := fs.Bool("replace", false, "replace an installed plugin with the same name")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 || *dest == "" {
		return errUsage
	}
	res, err := bundle.Install(pos[0], *dest, *trust, *replace)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func pluginKeygen(_ context.Context, args []string) error {
	fs := newFlags("keygen")
	keyID := fs.String("key-id", "", "key id, e.g. your-handle-2026")
	out := fs.String("out", "", "private key path (default <key-id>.pem)")
	publisher := fs.String("publisher", "", "publisher name for the trust entry (default key id)")
	if _, err := parseArgs(fs, args); err != nil || *keyID == "" {
		return errUsage
	}
	if *out == "" {
		*out = *keyID + ".pem"
	}
	if *publisher == "" {
		*publisher = *keyID
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pemBytes, err := signing.EncodePrivateKey(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(pemBytes); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "wrote private key %s (keep it outside the repository)\nadd this entry to the trust store \"keys\" list:\n", *out)
	return printJSON(signing.TrustEntry(*keyID, *publisher, pub))
}
