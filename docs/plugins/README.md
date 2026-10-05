# Writing OpenGTM plugins

A plugin adds a provider, a scraper, a workbook function or an agent tool
without forking OpenGTM. Every plugin is a directory with a `plugin.yaml`
manifest, recorded `fixtures/` that act as its tests, and (for WebAssembly
plugins) a compiled module. The Go host (`opengtm`) validates, tests, runs,
signs and installs plugins with one command, `opengtm plugin`.

This guide covers manifest v2 as implemented in `apps/server/internal/plugin`.
The design is in [the platform RFC](../plans/hybrid-platform-rewrite.md#plugin-architecture);
the machine-readable contract is
[`packages/contracts/plugin-manifest.v2.schema.json`](../../packages/contracts/plugin-manifest.v2.schema.json).
Existing [v1 connectors](../connectors/README.md) keep working unchanged.

- [A declarative scraper in five minutes](#a-declarative-scraper-in-five-minutes)
- [A declarative provider](#a-declarative-provider)
- [A WebAssembly plugin](#a-webassembly-plugin)
- [A Python plugin](#a-python-plugin)
- [Fixtures](#fixtures)
- [Signing, packaging and installing](#signing-packaging-and-installing)
- [Running plugins on a server](#running-plugins-on-a-server)
- [Capabilities and responsible scraping](#capabilities-and-responsible-scraping)
- [Manifest reference](#manifest-reference)
- [Compatibility with the Python connector SDK](#compatibility-with-the-python-connector-sdk)

Build the CLI once with `cd apps/server && go build -o ~/bin/opengtm ./cmd/opengtm`.

## A declarative scraper in five minutes

```bash
opengtm plugin new scraper acme_team_page          # scaffold manifest + fixtures
$EDITOR acme_team_page/plugin.yaml                 # point it at your site
opengtm plugin record acme_team_page --input domain=acme.example --case acme
opengtm plugin test acme_team_page                 # offline, deterministic
opengtm plugin dev acme_team_page                  # rerun tests on every save
```

The scaffold is a working scraper for a `/team` page:

```yaml
manifest_version: "2"
name: acme_team_page
kind: scraper
runtime: declarative
version: 0.1.0
capabilities:
  network: ["https://*.acme.example"]   # the only hosts it may fetch
limits:
  requests_per_second_per_domain: 1
  max_pages: 5
inputs: { domain: string }
outputs: person
scrape:
  start: "https://{{input.domain}}/team"
  items: "css:.team-member"             # one record per match
  fields:
    full_name: "css:.name::text"
    title: "css:.role::text"
    linkedin_url: "css:a.linkedin::attr(href)"
  paginate: { next: "css:a.next::attr(href)", max_pages: 3 }
```

Selectors use the extraction kernel's prefixes: `css:<selector>` (text of the
first match, or `::text`, `::attr(name)`, `::html`), `json:<JSONPath>` for JSON
documents (`format: json`) and `re:<regex>` (first capture group). Relative
`href`/`src` values resolve against the page URL. Extraction runs in the Rust
kernel (`internal/kernels`); a build without the kernel refuses to run or test
scrapers with an explicit error rather than producing fake results.

What the host does for every page: renders the start URL from the inputs,
checks the URL against `capabilities.network`, applies `robots.txt`, the
per-domain rate limit and size cap, extracts records, follows `paginate.next`
(deduplicating visited URLs, never leaving the declared network, stopping at
the smaller of `paginate.max_pages` and `limits.max_pages`), and attaches
evidence to each record: page URL, fetch time, status, page number, the items
and field selectors, and the SHA-256 of the page body.

See [`plugins/examples/declarative-scraper`](../../plugins/examples/declarative-scraper)
for a two-page example with recorded fixtures for a fictional site.

## A declarative provider

```bash
opengtm plugin new provider acme_firmographics
opengtm plugin test acme_firmographics
ACME_FIRMOGRAPHICS_API_KEY=... opengtm plugin run acme_firmographics \
  --input domain=acme.example --secret-env ACME_FIRMOGRAPHICS_API_KEY
```

A provider calls one HTTP API and maps the JSON response onto OpenGTM fields.
The `request`/`response`/`auth` blocks are exactly those of connector manifest
v1, so the same templates work:

```yaml
capabilities:
  network: ["https://api.acme-data.example/v1"]
  secrets: [ACME_DATA_API_KEY]
capability: company_size          # primary field (used by waterfalls)
default_confidence: 0.75
cost_per_lookup: 0.01             # charged only when fields are returned
auth: { type: header, param: X-Api-Key, env_var: ACME_DATA_API_KEY }
request:
  method: GET                     # GET | POST | PUT | PATCH
  url: https://api.acme-data.example/v1/companies
  query: { domain: "{{input.domain}}" }
  timeout: 10
response:
  error_path: error               # truthy => vendor error envelope
  error_message_path: error_message
  mappings:
    company_size: "$.company.employees"
    linkedin_url: "https://www.linkedin.com/company/$.company.linkedin_handle"
```

Templates: `{{input.x}}`, `{{input.x | default: y}}` and `${env:SECRET}` (only
declared secrets resolve). Mappings: `$.a.b`, `$.list[0].x`, `$.list[].x`
(first element) and a literal prefix before `$.`. Auth modes: `none`, `bearer`
(`Authorization: Bearer <secret>`), `header` (`param`, default
`Authorization`), `query` (`param`, default `api_key`); `value` may template the
secret (`"Token ${env:KEY}"`).

Results use the Python runtime's vocabulary: success with fields, or an error of
`http_<status>`, `non_json_response`, `no_data`, `timeout`,
`blocked_url: <reason>`, `<name>: missing <SECRET>` or the vendor's message.
`phone` and `mobile_phone` are normalized to E.164 when possible. Evidence
records the source URL (secrets replaced by `REDACTED`), status, fetch time,
content type, size, body SHA-256 and the expression behind every field.

Example: [`plugins/examples/declarative-provider`](../../plugins/examples/declarative-provider).

## A WebAssembly plugin

Use WebAssembly when YAML is not enough: custom logic, transforms, workbook
functions, or multi-step API calls.

```bash
opengtm plugin new provider my_enricher --runtime wasm   # Rust + extism-pdk
cd my_enricher && ./build.sh && cd -                     # compile, pin sha256
opengtm plugin test my_enricher
```

`build.sh` compiles to `wasm32-unknown-unknown` (`rustup target add
wasm32-unknown-unknown`), copies the module to `plugin.wasm` and writes its
SHA-256 into `wasm.sha256`. The host refuses to load a module whose hash does
not match the manifest. Any language with an Extism PDK works (Rust, Go,
TypeScript/JavaScript, C, Zig); the ABI is in [wasm-abi.md](wasm-abi.md).

Sandbox limits come from the manifest: `limits.memory_mb`, a wall-clock
`limits.timeout_seconds` that also stops infinite loops, a 1 MiB output cap,
and `limits.max_pages` fetches per call. The module has no filesystem, no
environment variables and no Extism HTTP; its only way out is the
`opengtm_fetch` host function, which enforces the network capability and uses
the same guarded client as everything else.

Example: [`plugins/examples/wasm-echo-provider`](../../plugins/examples/wasm-echo-provider).

## A Python plugin

Use a Python plugin (`runtime: process`) for AI and model SDKs, browser
automation, heavy dependencies, or code you would rather write than configure.
The host runs it in its own process, hands it its inputs and only the secrets it
declared, answers its HTTP requests through the same guarded client as every
other runtime, and kills the whole process tree on timeout, cancellation or
crash.

```bash
opengtm plugin new provider acme_enricher --runtime process
pip install opengtm-sdk
opengtm plugin test acme_enricher
```

```python
from opengtm_sdk import provider, run

@provider
def enrich(ctx, domain: str):
    r = ctx.fetch("https://api.acme-data.example/v1/companies", params={"domain": domain},
                  headers={"X-Api-Key": ctx.secret("ACME_DATA_API_KEY")})
    return None if r.status == 404 else {"company_size": r.json()["company"]["employees"]}

if __name__ == "__main__":
    run()
```

See the [Python plugin guide](python.md) for the SDK, retries, testing,
configuration and what the isolation does and does not cover, and
[process-abi.md](process-abi.md) for the wire protocol (versioned
length-prefixed JSON over an inherited socket; why not gRPC is explained
there). Example: [`plugins/examples/python-provider`](../../plugins/examples/python-provider).

## Fixtures

Fixtures are the tests. `opengtm plugin test` runs every
`fixtures/<case>/case.yaml` offline:

```yaml
description: Known company returns firmographics.
input: { domain: acme.example }          # plugin inputs
secrets: { ACME_DATA_API_KEY: test-key } # fake values for declared secrets
config: {}                                # wasm plugin config (optional)
http:                                     # recorded responses
  - method: GET
    url: https://api.acme-data.example/v1/companies?domain=acme.example
    status: 200
    headers: { Content-Type: application/json }
    body_file: company.json               # next to case.yaml, or inline `body:`
expect:
  match: exact                            # exact (default) or subset
  fields: { company_size: 250 }           # providers, functions, tools
  records: [ { full_name: Ada Lovelace } ] # scrapers (ordered)
  error: http_404                         # expected provider error, if any
```

Replay goes through the real egress client (guard, capabilities, robots, size
caps) over an in-process transport. Any request without a recorded response
fails the case; requests are matched by method and URL with query parameters
compared order-insensitively. The host's own `robots.txt` fetch is answered
with 404 unless the case records it. `subset` lets expected fields or records
be a subset of the actual ones.

`opengtm plugin record <path> --input k=v --case name` runs the plugin live and
writes a case with every exchange, including redirects and `robots.txt`.
Request headers are never stored; cookies are dropped; declared secret values
are replaced by `REDACTED` in URLs, headers, bodies and expectations, and the
case declares the secrets as `REDACTED` so replays match. Review recorded
bodies before committing them: they are whatever the site returned.

## Signing, packaging and installing

Plugins use the same Ed25519 envelope and trusted-publisher store as v1
connectors (byte-for-byte compatible with the Python CLI):

```bash
opengtm plugin keygen --key-id your-handle-2026 --out ~/keys/your-handle-2026.pem
#   add the printed entry to docs/connectors/trusted-publishers.json (or your own store)
opengtm plugin sign my_plugin --private-key ~/keys/your-handle-2026.pem --key-id your-handle-2026
opengtm plugin verify my_plugin --trust-store docs/connectors/trusted-publishers.json
opengtm plugin pack my_plugin                      # -> my_plugin-0.1.0.ogc
opengtm plugin install my_plugin-0.1.0.ogc --destination /var/lib/opengtm/plugins
```

- The signature (`plugin.yaml.sig`) covers the canonical form of the manifest:
  `json.dumps(yaml.safe_load(text), sort_keys=True, separators=(",", ":"),
  ensure_ascii=False)`. Comments and formatting do not matter; any value change
  does.
- WebAssembly modules are covered transitively: the signed manifest pins
  `wasm.sha256`, and pack, load and install all check it.
- `.ogc` bundles are deterministic ZIPs. v1 bundles hold exactly
  `connector.yaml` and `connector.yaml.sig` and install to
  `<destination>/<capability>/<name>.yaml`, like `connector-install`. v2
  bundles hold `plugin.yaml`, `plugin.yaml.sig`, the module and `fixtures/**`,
  and install to `<destination>/<name>/`. Fixtures are test data and are not
  covered by the signature.
- Installation verifies a trusted signature and the full manifest contract in a
  staging directory before anything is written, rejects unexpected or path
  traversal entries and oversized files, holds a per-plugin lock, and refuses to
  overwrite an installed plugin unless `--replace` is given.
- Signature policies: `optional` (default; unsigned local plugins work, but a
  present invalid or untrusted signature is rejected) and `required`
  (production; set `CONNECTOR_SIGNATURE_POLICY=required`). Unknown policy
  values fail closed to `required`.

## Running plugins on a server

`opengtm serve` and `opengtm worker` load plugins once at startup:

| Setting | Meaning |
| --- | --- |
| `OPENGTM_PLUGIN_DIRS` | Path list searched for directories holding `plugin.yaml`. Hidden, `fixtures`, `examples`, `target` and `node_modules` directories are skipped below a root; point a root at `plugins/examples` directly to load the examples. |
| `OPENGTM_CONNECTOR_DIRS` | v1 connector directories, validated exactly as the Python registry does. |
| `CONNECTOR_SIGNATURE_POLICY` | `optional` or `required`, shared with the Python app. |
| `OPENGTM_PLUGIN_TRUST_STORE` | Trusted-publisher JSON file. |
| `OPENGTM_EGRESS_PROXY` | Optional HTTP proxy for plugin traffic. |

The server image bundles `plugins/` and the v1 connectors and sets these
variables; mount another directory and add it to `OPENGTM_PLUGIN_DIRS` to
install more. Rejected plugins (bad manifest, failed signature policy,
duplicate name) are logged at startup and never run.

Each run is a durable `plugin_run` job for the caller's workspace:

```bash
curl -H "Authorization: Bearer $TOKEN" https://opengtm.example/api/v2/plugins
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"plugin":"acme_team_page","inputs":{"domain":"acme.example"}}' \
  https://opengtm.example/api/v2/plugin-runs
curl -H "Authorization: Bearer $TOKEN" https://opengtm.example/api/v2/plugin-runs/$RUN_ID/results
```

Editors, admins and owners can start and cancel runs; viewers can read them.
Inputs are validated against the manifest before the job is queued. Results
are stored with their evidence under workspace row-level security, and
progress is streamed on `/api/v2/events`. A run that is cancelled, or whose
job lease is lost to a timeout or another worker, never commits results.

Declared secrets currently resolve from the worker's environment. Per-workspace
secret resolution arrives when workspace secrets move from the legacy SQLite
control plane to PostgreSQL (RFC milestone M8).

`process` plugins (Python) need a Linux or macOS worker with Python and the SDK;
the default distroless image has neither. They run under a bounded process pool
(`OPENGTM_PLUGIN_MAX_PROCESSES`, `OPENGTM_PLUGIN_MAX_PER_PLUGIN`), with
`OPENGTM_PLUGIN_PYTHON`, `OPENGTM_PLUGIN_PYTHONPATH`, `OPENGTM_PLUGIN_STATE_DIR`
and `OPENGTM_PLUGIN_SANDBOX` as the other settings; all are described in the
[Python plugin guide](python.md#running-on-a-server). A crash or timeout is
retried by the queue, a plugin's permanent failure (bad input, bad credentials,
an exception in its code) fails the run at once, and results are committed under
the same job lease as every other runtime.

## Capabilities and responsible scraping

A plugin can only do what its manifest declares, and admins see these
capabilities before enabling it.

`capabilities.network` lists URL patterns `scheme://host[:port][/path]`:

| Pattern | Allows | Does not allow |
| --- | --- | --- |
| `https://api.example.com` | any path on that host, port 443 | `http://`, other hosts, other ports |
| `https://*.example.com` | `example.com` and every subdomain | `example.com.evil.net`, `notexample.com` |
| `https://api.example.com/v1` | `/v1` and `/v1/...` | `/v10`, `/v1/../admin`, encoded slashes |
| `http://127.0.0.1:*` | any port (local development only) | anything else |
| `https://*` | any HTTPS host (still subject to the SSRF guard) | `http://` |

An `http://` pattern also admits the `https://` upgrade, never the reverse.
Static request and start URLs are checked against the patterns at validation
time; templated hosts are checked on every request and redirect hop.

`capabilities.secrets` lists the workspace secrets a plugin may read. Only
these are resolved, per workspace, at call time; `${env:NAME}` for anything
else renders empty and fails validation in v2. Secret values are redacted from
evidence, errors, logs and recorded fixtures.

Every request, from any runtime, goes through the host's egress client:

- **Destination guard.** Only `http`/`https`; `localhost`, cloud metadata
  hosts and private, loopback, link-local, CGNAT, multicast and reserved
  ranges (IPv4 and IPv6, IPv4-mapped, NAT64 and 6to4 forms) are refused,
  including decimal, octal and hex host spellings.
- **DNS pinning.** Hostnames are resolved once, every address is checked, and
  the connection goes to a checked address, so DNS rebinding cannot redirect
  a request. Each redirect hop (at most 5) is validated the same way, and
  credentials are dropped when a redirect changes origin. With an egress proxy
  (`OPENGTM_EGRESS_PROXY`), the CONNECT tunnel targets the checked address.
- **robots.txt** for scrapers (and WebAssembly scrapers): fetched per origin
  and cached for an hour; the `OpenGTM` group applies, otherwise `*`;
  longest-match Allow/Disallow with `*` and `$`; `Crawl-delay` is honored as a
  minimum interval. A missing `robots.txt` (4xx) allows crawling; a 5xx or
  network failure disallows the request and is retried after a minute.
  Providers calling documented API endpoints do not consult `robots.txt`,
  matching the existing runtime.
- **Rate limits** per registrable domain (`requests_per_second_per_domain`,
  default 1), shared by all workers in the process, so subdomains cannot be
  used to multiply load. A wait that would outlast the request deadline fails
  fast.
- **Identifying User-Agent:** `OpenGTM/<version> (+https://github.com/debpalash/OpenGTM)`.
  Plugins cannot override it.
- **Size and time limits:** responses are capped (5 MiB by default,
  `limits.max_response_bytes`) after decompression, and every call has a
  deadline.

None of these can be switched off from a manifest. Plugin authors remain
responsible for respecting each site's terms of service and applicable law.

## Manifest reference

| Key | Notes |
| --- | --- |
| `manifest_version` | `"2"` (a string). v1 connectors omit it or use `"1"`. |
| `name` | `^[a-z][a-z0-9_]{2,63}$`, unique per installation. |
| `kind` | `provider`, `scraper`, `signal`, `destination`, `function`, `tool`. |
| `runtime` | `declarative` (providers and scrapers), `wasm`, `process` (Python or any language speaking the [process ABI](process-abi.md); kinds `provider`, `scraper`, `function`, `tool`). |
| `version` | Semantic version. |
| `author`, `license`, `description`, `homepage`, `display_name`, `tags` | Catalog metadata. |
| `capabilities` | `network` (patterns), `secrets` (names), `browser` (bool, for process plugins). Declarative plugins need at least one network pattern. |
| `limits` | `requests_per_second_per_domain` (default 1, max 100), `timeout_seconds` (20, max 300), `max_pages` (10, max 1000), `max_response_bytes` (5 MiB, max 50 MiB), `memory_mb` (64, max 4096). |
| `inputs` | JSON Schema with `type: object`, or the shorthand `{name: type}` where type is `string`, `number`, `integer`, `boolean`, `object` or `array`; append `?` for optional (`"integer?"`). Inputs are validated before every run. |
| `outputs` | An entity name (`person`, `company`, ...) or a JSON Schema object. |
| `config` | JSON Schema (`type: object`) for workspace settings, validated before wasm calls. |
| `capability`, `default_confidence`, `cost_per_lookup` | Providers. |
| `auth`, `request`, `response`, `input_fields` | Declarative providers (same as v1). Use `body` or `body_template`, not both. |
| `scrape` | Declarative scrapers: `start`, `items`, `fields`, `format` (`html`/`json`), `paginate` (`next`, `max_pages`). |
| `wasm` | `module` (relative path ending in `.wasm`) and `sha256`. |
| `process` | `command` (list of strings): a bare `python3` is looked up on the worker's `PATH` (or `OPENGTM_PLUGIN_PYTHON`), a path with a slash is relative to the plugin directory. |
| `x-*` | Reserved for extensions; ignored by the host. |

Unknown keys are rejected so typos surface early. A v1 connector loads as a v2
declarative provider whose `capabilities.network` is its endpoint origin (or
`https://*` when the host is templated, as the Python runtime allowed) and whose
`capabilities.secrets` is `[auth.env_var]`.

The schema and the Go validator are kept consistent by a test that runs both
over every fixture in `apps/server/internal/plugin/manifest/testdata/v2`.
Rules a JSON Schema cannot express are enforced by the host only: declared
secrets for `auth.env_var` and `${env:...}`, network coverage of static URLs,
and validity of embedded `inputs`/`outputs`/`config` schemas.

## Compatibility with the Python connector SDK

The Go host reproduces the Python implementation and is tested against it:

| Area | How parity is checked |
| --- | --- |
| YAML loading and canonical JSON (signing) | `pycompat/testdata/gen_yaml_parity.py` records PyYAML `safe_load` + `json.dumps` for YAML 1.1 edge cases (booleans, octal, sexagesimal, dates, merges, duplicate keys, floats). |
| Templates and mappings | `template/testdata/gen_template_parity.py` records `render_string`, `render_template`, `project_value`, `project_response`. |
| Manifest v1 validation | `manifest/testdata/gen_v1_parity.py` builds a 116-case corpus (valid, invalid and signed manifests) and records `validate_manifest_directory()`; Go must match decisions, messages, pydantic error locations, catalog entries and the `--json` report byte for byte. |
| Provider execution | `declarative/testdata/gen_provider_parity.py` records the requests and results of `DeclarativeProvider.enrich()` with a mocked transport. |
| URL guard | `egress/testdata/gen_guard_parity.py` records `check_url()` decisions. |
| Signing and bundles | `bundle/crosslang_test.go` signs with the Python CLI and verifies in Go and vice versa (envelopes are byte-identical), and installs each side's bundles with the other. Skipped when `uv` is unavailable. |

Regenerate fixtures from the repository root with
`uv run python <script>`; the Go tests then assert the recorded behavior.

Deliberate differences:

- The Go guard is stricter: it also blocks CGNAT (`100.64.0.0/10`), IPv6
  site-local (`fec0::/10`) and `inet_aton` short forms such as `127.1`, and it
  pins DNS at connect time, which the Python guard documents as a gap.
- In v2, `${env:NAME}` resolves only declared secrets; v1 connectors derive
  their single declared secret from `auth.env_var`. Like the Python runtime,
  `${env:...}` is expanded after `{{input...}}`, so an input containing
  `${env:KEY}` expands to a declared secret that is sent to the declared host.
- Attribute lookups on non-object template values (`{{input.name.upper}}`)
  render nothing instead of a Python method repr.
- Bundles with duplicate ZIP entries are rejected (Python would read the last).
- Go's deflate output differs from Python's, so `.ogc` bytes differ between
  implementations even though both are deterministic and interoperable.
