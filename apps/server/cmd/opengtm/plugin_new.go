package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var scaffoldNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// Placeholders: __NAME__ (plugin name), __ENV__ (secret name), __CRATE__.
var declarativeProvider = map[string]string{
	"plugin.yaml": `# Declarative provider. Docs: docs/plugins/README.md
manifest_version: "2"
name: __NAME__
kind: provider
runtime: declarative
version: 0.1.0
author: your-github-handle
license: Apache-2.0
description: Domain -> employee count from the Example API.
capabilities:
  network: ["https://api.example.com/v1"]
  secrets: [__ENV__]
limits:
  requests_per_second_per_domain: 1
  timeout_seconds: 20
inputs: { domain: string }
outputs: company
capability: company_size
default_confidence: 0.7
cost_per_lookup: 0
auth: { type: bearer, env_var: __ENV__ }
request:
  method: GET
  url: https://api.example.com/v1/lookup
  query: { domain: "{{input.domain}}" }
response:
  error_path: error
  error_message_path: message
  mappings:
    company_size: "$.data.employees"
`,
	"fixtures/basic/case.yaml": `description: A known domain returns its employee count.
input: { domain: acme.example }
secrets: { __ENV__: test-key }
http:
  - method: GET
    url: https://api.example.com/v1/lookup?domain=acme.example
    status: 200
    headers: { Content-Type: application/json }
    body_file: response.json
expect:
  match: exact
  fields: { company_size: 120 }
`,
	"fixtures/basic/response.json": "{\"data\": {\"employees\": 120}}\n",
}

var declarativeScraper = map[string]string{
	"plugin.yaml": `# Declarative scraper. Docs: docs/plugins/README.md
manifest_version: "2"
name: __NAME__
kind: scraper
runtime: declarative
version: 0.1.0
author: your-github-handle
license: Apache-2.0
description: People listed on a company's /team page.
capabilities:
  network: ["https://*.example.com"]
  secrets: []
  browser: false
limits:
  requests_per_second_per_domain: 1
  timeout_seconds: 20
  max_pages: 5
inputs: { domain: string }
outputs: person
scrape:
  start: "https://{{input.domain}}/team"
  items: "css:.team-member"
  fields:
    full_name: "css:.name::text"
    title: "css:.role::text"
  paginate: { next: "css:a.next::attr(href)", max_pages: 3 }
`,
	"fixtures/basic/case.yaml": `description: One team page with two people (extraction runs in the Rust kernel).
input: { domain: www.example.com }
http:
  - method: GET
    url: https://www.example.com/team
    status: 200
    headers: { Content-Type: text/html }
    body_file: team.html
expect:
  match: exact
  records:
    - { full_name: Ada Lovelace, title: CEO }
    - { full_name: Alan Turing, title: CTO }
`,
	"fixtures/basic/team.html": `<html><body>
<div class="team-member"><h3 class="name">Ada Lovelace</h3><p class="role">CEO</p></div>
<div class="team-member"><h3 class="name">Alan Turing</h3><p class="role">CTO</p></div>
</body></html>
`,
}

const wasmManifest = `# WebAssembly __KIND__ (Rust + extism-pdk). Run ./build.sh to compile the
# module and pin its sha256 below. Docs: docs/plugins/wasm-abi.md
manifest_version: "2"
name: __NAME__
kind: __KIND__
runtime: wasm
version: 0.1.0
author: your-github-handle
license: Apache-2.0
description: Describe what this plugin does.
capabilities:
  network: ["https://api.example.com/v1"]
  secrets: []
limits:
  timeout_seconds: 10
  memory_mb: 32
  max_pages: 2
inputs: { __INPUT__: string }
__EXTRA__wasm:
  module: plugin.wasm
  sha256: "0000000000000000000000000000000000000000000000000000000000000000"
`

const wasmCargo = `[package]
name = "__CRATE__"
version = "0.1.0"
edition = "2021"
publish = false

[lib]
crate-type = ["cdylib"]

[dependencies]
extism-pdk = "1.4"
serde_json = "1"

[profile.release]
opt-level = "s"
lto = true
strip = true
codegen-units = 1
panic = "abort"

[workspace]
`

const wasmBuild = `#!/usr/bin/env bash
# Build the module and pin its sha256 in plugin.yaml.
#   rustup target add wasm32-unknown-unknown   # once
set -euo pipefail
cd "$(dirname "$0")"
cargo_home="${CARGO_HOME:-$HOME/.cargo}"
export RUSTFLAGS="--remap-path-prefix=${cargo_home}=/cargo --remap-path-prefix=$(pwd)=/src ${RUSTFLAGS:-}"
cargo build --release --target wasm32-unknown-unknown
cp target/wasm32-unknown-unknown/release/__CRATE__.wasm plugin.wasm
sum=$(sha256sum plugin.wasm | cut -d' ' -f1)
sed -i.bak -E "s/^(  sha256: ).*/\1\"${sum}\"/" plugin.yaml && rm -f plugin.yaml.bak
echo "plugin.wasm sha256 ${sum}"
`

const wasmPrelude = `use extism_pdk::*;
use serde_json::{json, Value};

#[host_fn]
extern "ExtismHost" {
    fn opengtm_fetch(request: Json<Value>) -> Json<Value>;
    fn opengtm_log(level: String, message: String);
}

fn fetch_json(url: &str) -> Result<Value, Error> {
    let Json(resp) = unsafe { opengtm_fetch(Json(json!({"method": "GET", "url": url})))? };
    if let Some(err) = resp.get("error") {
        return Err(Error::msg(err.to_string()));
    }
    let body = resp.get("body").and_then(Value::as_str).unwrap_or("null");
    Ok(serde_json::from_str(body)?)
}
`

var wasmBodies = map[string]string{
	"provider": `
/// Input: {"inputs": {...}, "config": {...}}. Output: {"fields": {...}}.
#[plugin_fn]
pub fn enrich(Json(input): Json<Value>) -> FnResult<Json<Value>> {
    let domain = input["inputs"]["domain"].as_str().unwrap_or_default();
    let _ = unsafe { opengtm_log("info".into(), format!("looking up {domain}")) };
    let data = fetch_json(&format!("https://api.example.com/v1/lookup?domain={domain}"))?;
    Ok(Json(json!({"fields": {"company_size": data["data"]["employees"]}, "confidence": 0.7})))
}
`,
	"scraper": `
/// Input: {"inputs": {...}}. Output: {"records": [{"fields": {...}}]}.
#[plugin_fn]
pub fn scrape(Json(input): Json<Value>) -> FnResult<Json<Value>> {
    let domain = input["inputs"]["domain"].as_str().unwrap_or_default();
    let data = fetch_json(&format!("https://api.example.com/v1/people?domain={domain}"))?;
    let records: Vec<Value> = data["people"].as_array().cloned().unwrap_or_default()
        .into_iter().map(|p| json!({"fields": {"full_name": p["name"]}})).collect();
    Ok(Json(json!({"records": records})))
}
`,
	"function": `
/// Input: {"inputs": {"text": "..."}}. Output: {"result": ...}.
#[plugin_fn]
pub fn call(Json(input): Json<Value>) -> FnResult<Json<Value>> {
    let text = input["inputs"]["text"].as_str().unwrap_or_default();
    Ok(Json(json!({"result": text.trim().to_lowercase()})))
}
`,
	"tool": `
/// Input: {"inputs": {...}}. Output: {"output": ...}.
#[plugin_fn]
pub fn invoke(Json(input): Json<Value>) -> FnResult<Json<Value>> {
    Ok(Json(json!({"output": {"echo": input["inputs"]}})))
}
`,
}

func wasmScaffold(kind, name string) (map[string]string, error) {
	body, ok := wasmBodies[kind]
	if !ok {
		return nil, fmt.Errorf("wasm scaffolds exist for provider, scraper, function and tool (not %s)", kind)
	}
	input, extra := "domain", ""
	switch kind {
	case "provider":
		extra = "capability: company_size\n"
	case "function":
		input = "text"
	case "tool":
		input = "query"
	}
	m := strings.NewReplacer("__KIND__", kind, "__INPUT__", input, "__EXTRA__", extra).Replace(wasmManifest)
	files := map[string]string{
		"plugin.yaml": m,
		"Cargo.toml":  wasmCargo,
		"build.sh":    wasmBuild,
		"src/lib.rs":  wasmPrelude + body,
		".gitignore":  "target/\n",
	}
	switch kind {
	case "provider":
		files["fixtures/basic/case.yaml"] = `description: Build the module first (./build.sh), then run opengtm plugin test.
input: { domain: acme.example }
http:
  - method: GET
    url: https://api.example.com/v1/lookup?domain=acme.example
    status: 200
    headers: { Content-Type: application/json }
    body: '{"data": {"employees": 120}}'
expect:
  match: exact
  fields: { company_size: 120 }
`
	case "function":
		files["fixtures/basic/case.yaml"] = "description: Pure function, no HTTP.\ninput: { text: \"  Hello \" }\nhttp: []\nexpect:\n  fields: { result: hello }\n"
	case "tool":
		files["fixtures/basic/case.yaml"] = "description: Echo tool, no HTTP.\ninput: { query: ping }\nhttp: []\nexpect:\n  fields: { output: { echo: { query: ping } } }\n"
	}
	return files, nil
}

func pluginNew(_ context.Context, args []string) error {
	fs := newFlags("new")
	runtime := fs.String("runtime", "declarative", "declarative, wasm or process (Python)")
	dir := fs.String("dir", "", "target directory (default ./<name>)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 2 {
		return errUsage
	}
	kind, name := pos[0], pos[1]
	if !scaffoldNameRE.MatchString(name) {
		return fmt.Errorf("plugin name %q must match ^[a-z][a-z0-9_]{2,63}$", name)
	}
	var files map[string]string
	switch *runtime {
	case "declarative":
		switch kind {
		case "provider":
			files = declarativeProvider
		case "scraper":
			files = declarativeScraper
		default:
			return fmt.Errorf("declarative plugins are providers or scrapers; use --runtime wasm for a %s", kind)
		}
	case "wasm":
		if files, err = wasmScaffold(kind, name); err != nil {
			return err
		}
	case "process":
		if files, err = processScaffold(kind); err != nil {
			return err
		}
	default:
		return fmt.Errorf("--runtime must be declarative, wasm or process")
	}
	target := *dir
	if target == "" {
		target = name
	}
	if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s exists and is not empty", target)
	}
	repl := strings.NewReplacer("__NAME__", name, "__ENV__", strings.ToUpper(name)+"_API_KEY", "__CRATE__", strings.ReplaceAll(name, "-", "_"))
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		full := filepath.Join(target, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(repl.Replace(files[rel])), mode); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "  created %s\n", full)
	}
	next := "opengtm plugin test " + target
	switch {
	case *runtime == "wasm":
		next = "cd " + target + " && ./build.sh && cd - && " + next
	case *runtime == "process":
		next = "pip install opengtm-sdk   # or: export OPENGTM_PLUGIN_PYTHONPATH=<repo>/packages/sdk-python/src\n      " + next
	case kind == "scraper":
		next += "   (scraper extraction needs the Rust kernel build)"
	}
	fmt.Fprintf(stdout, "\nnext: %s\n", next)
	return nil
}
