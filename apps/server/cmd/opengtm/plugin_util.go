package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// kernelExtractor provides the extraction kernel for declarative scrapers.
// The Rust kernel integration (internal/kernels) sets it from an init() in
// its own cmd/opengtm file; while it is nil, scraper runs fail with
// declarative.ErrNoExtractor instead of faking results.
var kernelExtractor func(ctx context.Context) (declarative.Extractor, func(), error)

func loadExtractor(ctx context.Context) (declarative.Extractor, func(), error) {
	if kernelExtractor == nil {
		return nil, func() {}, nil
	}
	return kernelExtractor(ctx)
}

// stdout/stderr are swappable for tests.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

// errUsage signals that usage was printed.
var errUsage = errors.New("invalid usage")

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("opengtm plugin "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseArgs parses flags placed anywhere among positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// kvFlag collects repeated key=value flags.
type kvFlag map[string]string

func (k kvFlag) String() string { return "" }

func (k kvFlag) Set(v string) error {
	key, val, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	k[key] = val
	return nil
}

// listFlag collects repeated values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// coerceInputs converts --input strings using the plugin's inputs schema
// (integer, number, boolean, object and array properties are parsed).
func coerceInputs(p *manifest.Plugin, raw map[string]string, jsonInputs string) (map[string]any, error) {
	out := map[string]any{}
	if jsonInputs != "" {
		if err := json.Unmarshal([]byte(jsonInputs), &out); err != nil {
			return nil, fmt.Errorf("--input-json: %w", err)
		}
	}
	props := map[string]any{}
	if p.Inputs != nil {
		props, _ = p.Inputs["properties"].(map[string]any)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := raw[k]
		prop, _ := props[k].(map[string]any)
		typ, _ := prop["type"].(string)
		var err error
		switch typ {
		case "integer":
			out[k], err = strconv.ParseInt(v, 10, 64)
		case "number":
			out[k], err = strconv.ParseFloat(v, 64)
		case "boolean":
			out[k], err = strconv.ParseBool(v)
		case "object", "array":
			var x any
			err = json.Unmarshal([]byte(v), &x)
			out[k] = x
		default:
			out[k] = v
		}
		if err != nil {
			return nil, fmt.Errorf("--input %s: %w", k, err)
		}
	}
	return out, nil
}

// secretsFromEnv resolves --secret-env names; only declared secrets are kept.
func secretsFromEnv(p *manifest.Plugin, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		if !p.HasSecret(n) {
			return nil, fmt.Errorf("--secret-env %s: not declared in capabilities.secrets", n)
		}
		v, ok := os.LookupEnv(n)
		if !ok {
			return nil, fmt.Errorf("--secret-env %s: environment variable is not set", n)
		}
		out[n] = v
	}
	return out, nil
}

// defaultTrustStore is $OPENGTM_TRUST_STORE, then the server's
// $OPENGTM_PLUGIN_TRUST_STORE, then the repository trust store.
func defaultTrustStore() string {
	for _, name := range []string{"OPENGTM_TRUST_STORE", "OPENGTM_PLUGIN_TRUST_STORE"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return "docs/connectors/trusted-publishers.json"
}

func defaultPolicy() string { return signing.PolicyFromEnv() }

func printJSON(v any) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
