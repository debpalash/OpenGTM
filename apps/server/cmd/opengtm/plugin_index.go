package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/bundle"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/index"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

func init() {
	pluginCommands = append(pluginCommands,
		pluginCommand{"search", "[query...] [--index src] [--json]", "search the plugin index", pluginSearch},
		pluginCommand{"info", "<name[@version]> [--index src] [--json]", "show a plugin's versions, capabilities and publisher", pluginInfo},
		pluginCommand{"update", "[name...] [--dry-run] [--index src] [--destination dir]", "upgrade installed plugins to their newest release", pluginUpdate},
		pluginCommand{"remove", "<name...> [--destination dir]", "uninstall plugins", pluginRemove},
		pluginCommand{"list", "[--destination dir] [--json]", "list installed plugins and their signature status", pluginList},
		pluginCommand{"index", "build <bundle-dir> [--base-url url] [--output index.json] [--sign-key key.pem --key-id id] | verify <index> [--deep]", "build or verify a plugin index", pluginIndex},
		pluginCommand{"install", "<name[@version] | bundle.ogc> [--index src] [--destination dir] [--trust-store file] [--replace] [--allow-downgrade]",
			"install from the plugin index, or verify and install a local signed bundle", pluginInstallCmd},
	)
}

// indexFlags are shared by every command that talks to an index.
type indexFlags struct {
	index, dest, trust, policy string
	asJSON                     bool
}

func addIndexFlags(fs *flag.FlagSet, f *indexFlags, needIndex bool) {
	if needIndex {
		fs.StringVar(&f.index, "index", "", "index: https URL, directory or index.json (default $OPENGTM_PLUGIN_INDEX, plugins.index_url in opengtm.yaml, then "+index.DefaultURL+")")
	}
	fs.StringVar(&f.dest, "destination", "", "install root (default: first $OPENGTM_PLUGIN_DIRS entry, else ~/.opengtm/plugins)")
	fs.StringVar(&f.trust, "trust-store", "", "trusted publishers JSON (default $OPENGTM_PLUGIN_TRUST_STORE, then $OPENGTM_TRUST_STORE)")
	fs.StringVar(&f.policy, "signature-policy", "", "optional or required (default $CONNECTOR_SIGNATURE_POLICY or optional)")
	fs.BoolVar(&f.asJSON, "json", false, "machine-readable output")
}

// newIndexClient resolves flags, environment and opengtm.yaml, in that order.
func newIndexClient(f indexFlags) (*index.Client, error) {
	cfg, err := config.LoadPlugins("", os.LookupEnv)
	if err != nil {
		return nil, err
	}
	c := &index.Client{
		Source:     firstNonEmpty(f.index, cfg.IndexURL, index.DefaultURL),
		Root:       f.dest,
		TrustStore: f.trust,
		Policy:     firstNonEmpty(f.policy, cfg.SignaturePolicy, signing.PolicyOptional),
		UserAgent:  "OpenGTM/" + version + " (plugin client)",
	}
	if c.Root == "" {
		if len(cfg.Dirs) > 0 {
			c.Root = cfg.Dirs[0]
		} else if home, err := os.UserHomeDir(); err == nil {
			c.Root = filepath.Join(home, ".opengtm", "plugins")
		} else {
			return nil, errors.New("no install root: pass --destination or set OPENGTM_PLUGIN_DIRS")
		}
	}
	if c.TrustStore == "" {
		c.TrustStore = firstNonEmpty(cfg.TrustStore, defaultTrustStore())
	}
	return c, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// hint explains how to fix the most common failure: an unreachable index.
func hint(c *index.Client, err error) error {
	if err == nil {
		return nil
	}
	if c.Source == index.DefaultURL && strings.Contains(err.Error(), "fetch plugin index") {
		return fmt.Errorf("%w\nthe default index %s is a placeholder; point OPENGTM_PLUGIN_INDEX (or --index) at a real index, a mirror directory or an index.json", err, index.DefaultURL)
	}
	return err
}

// ---- install ------------------------------------------------------------------

func isBundlePath(arg string) bool {
	return strings.HasSuffix(arg, ".ogc") || strings.ContainsAny(arg, `/\`)
}

func pluginInstallCmd(ctx context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("install")
	addIndexFlags(fs, &f, true)
	replace := fs.Bool("replace", false, "replace an installed plugin with the same name")
	downgrade := fs.Bool("allow-downgrade", false, "allow installing an older version over a newer one (refused under the required policy)")
	pre := fs.Bool("pre", false, "let `latest` select a pre-release when there is no stable release")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	if isBundlePath(pos[0]) {
		// Offline: a local .ogc file, verified against the trust store.
		res, err := bundle.Install(pos[0], c.Root, c.TrustStore, *replace)
		if err != nil {
			return err
		}
		return printJSON(res)
	}
	c.AllowDowngrade, c.AllowPrerelease = *downgrade, *pre
	res, err := c.Install(ctx, pos[0], *replace)
	if err != nil {
		return hint(c, err)
	}
	if f.asJSON {
		return printJSON(res)
	}
	verb := "Installed"
	if res.Previous != "" {
		verb = fmt.Sprintf("Replaced %s with", res.Previous)
	}
	fmt.Fprintf(stdout, "%s %s@%s -> %s\n  publisher %s (key %s), sha256 %s\n", verb, res.Name, res.Version, res.Path, res.Publisher, res.KeyID, res.SHA256)
	if !res.IndexSigned {
		fmt.Fprintln(stdout, "  note: the index is not signed; the bundle's own signature was verified")
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stdout, "  warning:", w)
	}
	fmt.Fprintln(stdout, "  restart `opengtm serve`/`worker` to load it")
	return nil
}

// ---- search / info ------------------------------------------------------------

func pluginSearch(ctx context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("search")
	addIndexFlags(fs, &f, true)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	hits, err := c.Search(ctx, strings.Join(pos, " "))
	if err != nil {
		return hint(c, err)
	}
	if f.asJSON {
		return printJSON(map[string]any{"plugins": hits})
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "no matching plugins")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLATEST\tINSTALLED\tKIND\tRUNTIME\tCERTIFICATION\tPUBLISHER\tDESCRIPTION")
	for _, h := range hits {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, h.Latest, dash(h.Installed), h.Kind, h.Runtime, h.Certification, h.Publisher, truncate(h.Description, 60))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func pluginInfo(ctx context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("info")
	addIndexFlags(fs, &f, true)
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	name, want, err := index.SplitSpec(pos[0])
	if err != nil {
		return err
	}
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	p, sig, err := c.Info(ctx, name)
	if err != nil {
		return hint(c, err)
	}
	v, err := p.Resolve(want, true)
	if err != nil {
		return err
	}
	installed := ""
	if list, err := c.List(); err == nil {
		for _, i := range list {
			if i.Name == name {
				installed = i.Version
			}
		}
	}
	if f.asJSON {
		return printJSON(map[string]any{"plugin": p, "selected": v.Version, "installed": installed, "index_signature": sig})
	}
	fmt.Fprintf(stdout, "%s %s", p.Name, v.Version)
	if v.DisplayName != "" {
		fmt.Fprintf(stdout, " (%s)", v.DisplayName)
	}
	fmt.Fprintf(stdout, "\n  %s\n", v.Description)
	fmt.Fprintf(stdout, "  kind %s/%s, license %s, author %s\n", v.Kind, v.Runtime, dash(v.License), dash(v.Author))
	if v.Homepage != "" {
		fmt.Fprintf(stdout, "  homepage %s\n", v.Homepage)
	}
	fmt.Fprintf(stdout, "  publisher %s (key %s), certification %s\n", v.Publisher, v.Signature.KeyID, firstNonEmpty(v.Certification, index.CertUnreviewed))
	fmt.Fprintf(stdout, "  network %s\n  secrets %s, browser %v\n", dashList(v.Capabilities.Network), dashList(v.Capabilities.Secrets), v.Capabilities.Browser)
	fmt.Fprintf(stdout, "  bundle %s (%d bytes)\n  sha256 %s\n", v.URL, v.Size, v.SHA256)
	if v.Yanked {
		fmt.Fprintf(stdout, "  YANKED: %s\n", firstNonEmpty(v.YankedReason, "no reason given"))
	}
	fmt.Fprintf(stdout, "  installed %s, index signature %s\n  versions:", dash(installed), sig.Status)
	for _, x := range p.Versions {
		mark := ""
		if x.Yanked {
			mark = " (yanked)"
		}
		fmt.Fprintf(stdout, " %s%s", x.Version, mark)
	}
	fmt.Fprintln(stdout)
	return nil
}

func dashList(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// ---- update / remove / list ----------------------------------------------------

func pluginUpdate(ctx context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("update")
	addIndexFlags(fs, &f, true)
	dry := fs.Bool("dry-run", false, "report what would change without installing")
	pre := fs.Bool("pre", false, "consider pre-releases when no stable release is newer")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	c.AllowPrerelease = *pre
	st, err := c.Update(ctx, pos, *dry)
	if err != nil {
		return hint(c, err)
	}
	failed := false
	if f.asJSON {
		if err := printJSON(map[string]any{"plugins": st}); err != nil {
			return err
		}
	} else if len(st) == 0 {
		fmt.Fprintln(stdout, "no plugins installed in", c.Root)
	}
	for _, s := range st {
		if s.Status == "failed" {
			failed = true
		}
		if f.asJSON {
			continue
		}
		switch s.Status {
		case "updated", "would-update":
			fmt.Fprintf(stdout, "%-14s %s: %s -> %s\n", s.Status, s.Name, s.From, s.To)
		case "failed":
			fmt.Fprintf(stdout, "failed         %s: %s\n", s.Name, s.Error)
		default:
			fmt.Fprintf(stdout, "%-14s %s (%s)\n", s.Status, s.Name, s.From)
		}
	}
	if failed {
		return errors.New("some updates failed")
	}
	return nil
}

func pluginRemove(_ context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("remove")
	addIndexFlags(fs, &f, false)
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	for _, name := range pos {
		if err := c.Remove(name); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", name)
	}
	return nil
}

func pluginList(_ context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("list")
	addIndexFlags(fs, &f, false)
	if pos, err := parseArgs(fs, args); err != nil || len(pos) != 0 {
		return errUsage
	}
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	list, err := c.List()
	if err != nil {
		return err
	}
	if f.asJSON {
		return printJSON(map[string]any{"root": c.Root, "plugins": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no plugins installed in", c.Root)
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tKIND\tRUNTIME\tSIGNATURE\tPUBLISHER")
	for _, p := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\n", p.Name, p.Version, p.Kind, p.Runtime, p.Signature.Status, dash(fmt.Sprint(orNil(p.Signature.Publisher))))
	}
	return tw.Flush()
}

func orNil(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// ---- index build / verify ------------------------------------------------------

func pluginIndex(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "build":
		return pluginIndexBuild(ctx, args[1:])
	case "verify":
		return pluginIndexVerify(ctx, args[1:])
	}
	return errUsage
}

func pluginIndexBuild(_ context.Context, args []string) error {
	fs := newFlags("index build")
	base := fs.String("base-url", "", "prefix for bundle URLs (https://host/path); default: bare file names, relative to the index")
	out := fs.String("output", "", "index file to write (default <bundle-dir>/index.json)")
	name := fs.String("name", "", "human-readable index name")
	trust := fs.String("trust-store", "", "trusted publishers JSON used to verify bundles and name publishers")
	skip := fs.Bool("skip-verify", false, "list bundles without verifying their signature (publisher = key id)")
	signKey := fs.String("sign-key", "", "PEM Ed25519 private key; writes <output>.sig over the exact index bytes")
	keyID := fs.String("key-id", "", "key id for --sign-key")
	metaFile := fs.String("meta", "", `JSON {"name@version": {"certification":"beta","yanked":true,"yanked_reason":"...","published_at":"..."}}`)
	generated := fs.String("generated-at", "", "RFC 3339 timestamp (default $SOURCE_DATE_EPOCH, else now)")
	asJSON := fs.Bool("json", false, "print the index")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 || (*signKey == "") != (*keyID == "") {
		return errUsage
	}
	opts := index.BuildOptions{Dir: pos[0], BaseURL: *base, Name: *name, TrustStore: firstNonEmpty(*trust, defaultTrustStore()), SkipVerify: *skip}
	switch {
	case *generated != "":
		if opts.GeneratedAt, err = time.Parse(time.RFC3339, *generated); err != nil {
			return fmt.Errorf("--generated-at: %w", err)
		}
	case os.Getenv("SOURCE_DATE_EPOCH") != "":
		secs, err := strconv.ParseInt(os.Getenv("SOURCE_DATE_EPOCH"), 10, 64)
		if err != nil {
			return fmt.Errorf("SOURCE_DATE_EPOCH: %w", err)
		}
		opts.GeneratedAt = time.Unix(secs, 0)
	}
	if *metaFile != "" {
		raw, err := os.ReadFile(*metaFile)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&opts.Meta); err != nil {
			return fmt.Errorf("--meta %s: %w", *metaFile, err)
		}
	}
	res, err := index.Build(opts)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(pos[0], index.IndexFile)
	}
	var key ed25519.PrivateKey
	if *signKey != "" {
		if key, err = signing.LoadPrivateKey(*signKey); err != nil {
			return err
		}
	}
	if err := index.Write(*out, res.Index, key, *keyID); err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if *asJSON {
		return printJSON(res.Index)
	}
	n := 0
	for _, p := range res.Index.Plugins {
		n += len(p.Versions)
	}
	signed := "unsigned"
	if key != nil {
		signed = "signed by " + *keyID + " (" + *out + ".sig)"
	}
	fmt.Fprintf(stdout, "Wrote %s: %d plugin(s), %d release(s), %s\n", *out, len(res.Index.Plugins), n, signed)
	if key == nil {
		fmt.Fprintln(stdout, "  clients with CONNECTOR_SIGNATURE_POLICY=required will refuse an unsigned index; pass --sign-key and --key-id")
	}
	return nil
}

func pluginIndexVerify(ctx context.Context, args []string) error {
	var f indexFlags
	fs := newFlags("index verify")
	addIndexFlags(fs, &f, false)
	deep := fs.Bool("deep", false, "also download every bundle and check its sha256 and size")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	f.index = pos[0]
	c, err := newIndexClient(f)
	if err != nil {
		return err
	}
	l, err := c.Load(ctx)
	if err != nil {
		return err
	}
	bad := 0
	n := 0
	for _, p := range l.Index.Plugins {
		for _, v := range p.Versions {
			n++
			loc, err := l.Source.Resolve(v.URL)
			if err == nil && *deep {
				err = checkBundle(ctx, l.Source, loc, v)
			}
			if err != nil {
				bad++
				fmt.Fprintf(stdout, "FAIL %s@%s: %v\n", p.Name, v.Version, err)
			}
		}
	}
	fmt.Fprintf(stdout, "index %s: signature %s, %d plugin(s), %d release(s), %d problem(s)\n", l.Source.Location, l.Signature.Status, len(l.Index.Plugins), n, bad)
	if bad > 0 {
		return errors.New("index verification failed")
	}
	return nil
}

func checkBundle(ctx context.Context, src *index.Source, loc string, v index.Version) error {
	h := sha256.New()
	cw := &countingWriter{}
	if err := src.FetchBundle(ctx, loc, io.MultiWriter(h, cw), index.MaxBundleBytes); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.SHA256 {
		return fmt.Errorf("sha256 %s does not match the index (%s)", got, v.SHA256)
	}
	if v.Size > 0 && cw.n != v.Size {
		return fmt.Errorf("bundle is %d bytes, the index says %d", cw.n, v.Size)
	}
	return nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
