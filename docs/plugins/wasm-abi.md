# WebAssembly plugin ABI (v1)

OpenGTM runs WebAssembly plugins with the [Extism](https://extism.org) Go SDK
on wazero (pure Go, no cgo). Any language with an Extism PDK can implement a
plugin; the reference example is Rust with `extism-pdk`
([`plugins/examples/wasm-echo-provider`](../../plugins/examples/wasm-echo-provider)).
Host implementation: `apps/server/internal/plugin/wasmhost`.

## Module and manifest

```yaml
runtime: wasm
wasm:
  module: plugin.wasm   # relative to plugin.yaml
  sha256: "<64 hex>"     # required; the host refuses any other module
limits:
  memory_mb: 32          # linear memory limit for every module in the sandbox
  timeout_seconds: 10    # wall clock per call; also stops infinite loops
  max_pages: 2           # opengtm_fetch budget per call
```

Build for `wasm32-unknown-unknown` (or `wasm32-wasip1`). WASI is available
without filesystem preopens, environment variables or arguments. Extism's
built-in `http_request` is disabled (no allowed hosts); use `opengtm_fetch`.

The host compiles each module once and keeps a bounded pool of instances
(4 per plugin by default). Instances are reused between calls; an instance that
traps or times out is discarded. Plugins must not rely on state surviving
between calls.

## Exports

The exported function depends on the plugin kind. Every export takes JSON input
and returns JSON output through the Extism input/output buffers.

| Kind | Export | Output |
| --- | --- | --- |
| `provider` | `enrich` | `{"fields": {...}, "confidence": 0.9, "cost_usd": 0.0, "evidence": [...], "error": "..."}` |
| `scraper` | `scrape` | `{"records": [{"fields": {...}, "evidence": {...}}]}` |
| `function` | `call` | `{"result": <any JSON>}` |
| `tool` | `invoke` | `{"output": <any JSON>}` |

`signal` and `destination` exports are not defined yet.

Input for every export:

```json
{"inputs": {"domain": "acme.example"}, "config": {"region": "eu"}}
```

`inputs` has already been validated against the manifest's `inputs` schema and
`config` against its `config` schema. Only `fields`/`records`/`result`/`output`
are required in the output; everything else is optional. Output larger than
1 MiB fails the call. Return an Extism error (for example `Err(...)` from a
Rust `#[plugin_fn]`) for unexpected failures; report expected "no data"
outcomes as an `error` string in a provider's output.

The host records its own evidence for every fetch (URL, status, time, content
type, size, SHA-256) and returns it alongside the plugin output; plugin-supplied
evidence is informational.

## Host functions

Both live in the Extism user namespace `extism:host/user`, which is the default
import module for `#[host_fn] extern "ExtismHost"` in `extism-pdk`. Arguments
and results are Extism memory handles (i64 offsets).

### `opengtm_fetch(request: json) -> json`

Request:

```json
{"method": "GET", "url": "https://api.example.com/v1/x", "headers": {"Accept": "application/json"},
 "body": "optional UTF-8 body", "body_base64": "optional binary body"}
```

Successful response:

```json
{"status": 200, "headers": {"content-type": "application/json"},
 "body": "<UTF-8 text>", "body_base64": "<set instead of body for binary content>",
 "evidence": {"url": "...", "status": 200, "fetched_at": "...", "content_type": "...", "bytes": 123, "sha256": "..."}}
```

Failures are returned, not trapped, so the plugin can decide what to do:

```json
{"error": {"code": "capability_denied", "message": "..."}}
```

| Code | Meaning |
| --- | --- |
| `capability_denied` | URL (or a redirect hop) is outside `capabilities.network` |
| `blocked_url` | SSRF guard refused the destination (private, metadata, bad scheme) |
| `robots_disallowed` | `robots.txt` disallows the URL (scrapers and signals only) |
| `rate_limited` | the per-domain slot is later than the call deadline |
| `body_too_large` | response exceeds `limits.max_response_bytes` |
| `timeout` | the call deadline passed |
| `too_many_fetches` | more than `limits.max_pages` fetches in one call |
| `bad_request` | malformed request JSON or base64 |
| `fetch_failed` | other network failure |

Requests go through the host egress client: DNS pinning, validated redirects
(plugin headers are dropped when a redirect changes origin), the identifying
User-Agent (plugin `User-Agent` headers are ignored), per-domain rate limits
and response caps. `robots.txt` applies to `scraper` and `signal` plugins.

### `opengtm_log(level: string, message: string)`

Writes to the host's structured log with `plugin=<name>` and `source=wasm`.
Levels: `debug`/`trace`, `info`, `warn`/`warning`, `error`. Messages are cut at
4 KiB, at most 200 lines are kept per call, and declared secret values are
replaced by `REDACTED`.

## Secrets and config

Declared secrets (`capabilities.secrets`) are resolved for the calling workspace
at call time and exposed only as Extism config values, read with
`extism_pdk::config::get("NAME")`. Undeclared names are never present, and the
config is cleared after every call. Workspace `config` values arrive in the
call input, not in Extism config.

## Minimal Rust provider

```rust
use extism_pdk::*;
use serde_json::{json, Value};

#[host_fn]
extern "ExtismHost" {
    fn opengtm_fetch(request: Json<Value>) -> Json<Value>;
    fn opengtm_log(level: String, message: String);
}

#[plugin_fn]
pub fn enrich(Json(input): Json<Value>) -> FnResult<Json<Value>> {
    let domain = input["inputs"]["domain"].as_str().unwrap_or_default();
    let key = config::get("EXAMPLE_API_KEY")?.unwrap_or_default();
    let Json(resp) = unsafe {
        opengtm_fetch(Json(json!({
            "url": format!("https://api.example.com/v1/lookup?domain={domain}"),
            "headers": {"Authorization": format!("Bearer {key}")}
        })))?
    };
    if let Some(err) = resp.get("error") {
        return Ok(Json(json!({"fields": {}, "error": err["code"]})));
    }
    let data: Value = serde_json::from_str(resp["body"].as_str().unwrap_or("{}"))?;
    Ok(Json(json!({"fields": {"company_size": data["employees"]}})))
}
```

`opengtm plugin new provider my_plugin --runtime wasm` scaffolds this project
with a build script and a fixture case.
