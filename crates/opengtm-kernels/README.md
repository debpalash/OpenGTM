# opengtm-kernels

Rust data-plane kernels for OpenGTM: normalizers with exact parity to the
Python app, and declarative HTML/JSON extraction. The crate builds to one
WebAssembly module that the Go server embeds and runs in-process
(`apps/server/internal/kernels`), and that is also a standard
[Extism](https://extism.org) plugin.

```
src/api.rs        JSON in / JSON out entry points; the error envelope
src/normalize.rs  domain, email, phone, person-name normalizers
src/pystr.rs      CPython str semantics (whitespace, \d, lower) used for parity
src/pyurl.rs      port of CPython urllib.parse urlsplit/hostname validation
src/extract.rs    css:/json:/re: extraction
src/plugin.rs     Extism exports (wasm32 only)
src/direct.rs     direct-memory exports used by the Go host (wasm32 only)
parity/           Python fixture generator, fixtures.json, Python benchmark
testdata/         extract fixtures (HTML, JSON, cases) and the benchmark corpus
```

## Build, test, embed

```bash
cd crates
cargo test                                  # native unit, parity and extract tests
cargo clippy --all-targets -- -D warnings
cargo fmt --check
./build-kernels.sh                          # release wasm -> apps/server/internal/kernels/opengtm_kernels.wasm
cd ../apps/server && go test -race ./internal/kernels/...
```

The target is `wasm32-unknown-unknown` (the plan's default is `wasm32-wasip1`;
the kernels need no WASI, so the module imports nothing but the Extism host
environment). The release profile is `opt-level="s"`, LTO, one codegen unit,
stripped, `panic="abort"`. `build-kernels.sh` remaps local paths so the
artifact does not depend on where the repository lives, runs `wasm-opt -Oz`
only if it is installed, and prints the size and sha256. The `.wasm` is
committed so `go build` works without Rust; rebuild and commit it whenever
`crates/` changes.

## Interface

Every kernel takes a JSON request and returns JSON. Failures use one envelope
for every function and never panic:

```json
{"error": {"code": "invalid_selector", "message": "...", "field": "name"}}
```

`field` appears only when the error belongs to one extract field. Codes:
`invalid_request`, `request_too_large`, `invalid_selector`,
`invalid_document`, `document_too_large`, `output_too_large`. A trap (for
example the host's memory limit) is a runtime error, not an envelope. Go
surfaces envelopes as `*kernels.Error`.

| Function | Request | Response |
| --- | --- | --- |
| `normalize_domain` | `{"value": str}` | `{"value": str\|null}` |
| `normalize_email` | `{"value": str}` | `{"value": str\|null}` |
| `normalize_phone` | `{"value": str, "default_region"?: str}` | `{"value": str\|null}` |
| `normalize_person_name` | `{"value": str}` | `{"full": str, "first": str, "last": str}` |
| `normalize_batch` | `{"kind": "domain\|email\|phone\|person_name", "values": [str], "default_region"?: str}` | `{"values": [...]}` in input order; objects for `person_name` |
| `extract` | `ExtractRequest` in `apps/server/internal/kernels/types.go` | `ExtractResult` |

`null` is accepted wherever a field may be missing (Go encodes nil maps and
slices as `null`).

Two export sets wrap the same functions:

- **Extism exports** (`normalize_domain`, ..., `extract`) for the plugin
  platform and any Extism host.
- **Direct exports** (`og_alloc`, `og_free`, `og_<function>`) for the Go
  server: the host copies the request into guest memory once and reads the
  response once. The Extism I/O protocol costs about 25 µs of fixed host-side
  overhead per call in the Go SDK, ten times the whole direct call for a
  single domain (see BENCHMARKS.md). Protocol: `ptr = og_alloc(len)`, write
  the request, `packed = og_<function>(ptr, len)` (the guest frees the
  request), read `packed & 0xffffffff` bytes at `packed >> 32`, then
  `og_free(out_ptr, out_len)`.

## Normalizer parity with Python

Each normalizer ports one Python function exactly. Python's `""` (no value)
becomes `null`. `parity/generate.py` runs about 5,800 inputs (a hand-written
corpus of IDN, unicode whitespace, schemes, ports, userinfo, brackets, plus
addressing, invalid emails, international and non-Latin-digit phones,
honorifics, suffixes and particles, plus a seeded random corpus built from
delimiter-heavy fragments) through the Python functions and writes
`parity/fixtures.json`. Rust (`tests/parity.rs`) and Go
(`TestParitySingle`, `TestParityBatch`) assert exact equality on every case;
`TestExtismProtocolMatchesDirectABI` repeats 200 domain cases through the
Extism exports.

```bash
uv run python crates/opengtm-kernels/parity/generate.py   # regenerate; output is deterministic
```

### Chosen references

| Kernel | Python reference | Why this one |
| --- | --- | --- |
| domain | `apps/api/services/dedup.py::normalize_domain` | The company entity graph (`services/entities/graph.py`: `identity_domain`, blocking keys, merge/split evidence) and `LeadDeduplicator` use it; it is the identity key for companies. |
| email | `apps/api/services/entities/people.py::_email_key` | The person-entity identifier: trimmed, lowercased, and `""` unless it has `@` and a `.` after the last `@`. |
| phone | `apps/api/services/dedup.py::normalize_phone` | Company entity blocking keys and stored phone evidence: Unicode decimal digits only (Python `\d`), last 10 kept. |
| person_name | `apps/api/services/workbook/people_search.py` (`name.strip()` + `_split_name`) | The only code that produces full/first/last together; `PersonEntity.full_name` is the same `name.strip()`. |

The port reproduces CPython behavior that a "natural" Rust implementation
would get wrong: `urlsplit`'s removal of tab/CR/LF, its `ValueError` cases
(unbalanced brackets, invalid bracketed IPv6/IPvFuture hosts using the
`ipaddress` rules, netlocs whose NFKC form introduces `/?#@:`) and the
`except` fallback that then returns `url.lower().replace("www.", "").strip("/")`
(so `"[::1"` normalizes to `"http://[::1"`); `str.strip`/`str.split`
whitespace (which includes U+001C..U+001F); `\d` as Unicode `Nd` (so Arabic-Indic
digits survive phone normalization unconverted); and `str.lower` including
final sigma. No IDNA is applied, because Python applies none.

### Python variants that intentionally differ

These are not ported; callers that need them should not switch to the kernel
without a decision:

- **Domain:** `services/leadgen/dedup.py::normalize_domain` returns the
  registrable domain (eTLD+1 via a small multi-part suffix list), strips
  diacritics and tolerates emails; it is used by ingest, workbooks, account
  discovery and people contacts, and is the most likely next kernel kind.
  `leadgen/collection_intent.py::normalize_domain` validates with a regex and
  returns `""` for non-domains. `enrichment/declarative/compiler.py::_domain_of`
  only strips prefixes (keeps port, userinfo, subdomains). `signals/tracking.py::_canonical_domain`
  also strips a trailing dot. Several provider-local `_extract_domain`
  helpers keep the port.
- **Email:** `services/dedup.py::normalize_email` lowercases without
  validation; `leadgen/dedup.py::normalize_email` also strips diacritics
  (NFKD); `outreach/normalize.py::normalize_email` applies NFKC and IDNA to
  the domain for suppression keys.
- **Phone:** `leadgen/dedup.py::normalize_phone` returns `""` below 7 digits;
  `enrichment/normalize.py::normalize_phone` formats E.164 when the input has
  `+` or `00`. No Python code uses `phonenumbers`, so `default_region` is
  accepted and ignored.
- **Name:** `enrichment/email_finder.py::_split_name` lowercases first/last;
  entity name comparison uses `casefold` with collapsed whitespace;
  `poller/keys.py::normalize_exec_name` lowercases and collapses. None of
  them strip honorifics or suffixes: `"Dr. Jane Doe PhD"` splits into first
  `"Dr."` and last `"PhD"`, in Python and here.

### Divergences from Python

The fixtures pass with zero differences. Known cases where exact parity is
not guaranteed:

1. **Unicode version.** Parity targets CPython 3.13 (Unicode 15.1), the
   interpreter `uv` resolves for this repository. Whitespace, `Nd` digits and
   the NFKC delimiter set are copied from it. Lowercasing uses Rust's tables
   (Unicode 17) except for the 55 uppercase letters added after 15.1, which
   are kept unchanged like Python does. Case properties used by the
   final-sigma rule (cased/case-ignorable) for characters added after 15.1
   are not overridden; a `Σ` next to such a character could lowercase
   differently. No fixture exercises this.
2. **Python version.** `urllib.parse` validation changed across 3.11 to 3.13
   (bracketed-host checks arrived in 3.11.4/3.12). The project allows
   Python >= 3.11; on an older interpreter the Python side itself differs.
3. **Invalid Unicode.** Python strings can hold lone surrogates; Rust and Go
   strings cannot. Go's JSON encoder replaces invalid UTF-8 with U+FFFD before
   the kernel sees it, and the kernel rejects JSON with lone surrogate
   escapes as `invalid_request`.
4. **Non-string input.** The Python functions are called with `None` or
   numbers in places; the kernels take strings only (`null` behaves like
   `""`).

## Extraction semantics

Selector specs (see `types.go`):

- `css:<selector>[::text|::attr(name)|::html]`. `::text` (the default) is the
  element's text with whitespace collapsed and trimmed. Block-level element
  boundaries and `<br>` count as whitespace; descendant `script`, `style`,
  `noscript`, `template` and `head` text is skipped (selecting such an
  element directly, for example JSON-LD scripts, still returns its text).
  `::attr` values are trimmed; `href` and `src` resolve against `base_url`,
  adjusted by the document's `<base href>`, and stay as-is when no base
  applies. `::html` is the inner HTML. An empty selector (`css:::attr(href)`)
  addresses the record element itself; otherwise only descendants match,
  like BeautifulSoup's `select_one`.
- `json:<JSONPath>` uses RFC 9535 (`serde_json_path`), including filter
  functions such as `length()` and `match()`. Paths without a leading `$` get
  `$.` (or `$` before `[`). Strings are returned as-is, numbers and booleans as
  JSON text, objects and arrays as compact JSON (keys sorted).
- `re:<regex>` uses the Rust `regex` dialect (linear time; no lookaround or
  backreferences; `\d`, `\w`, `\s` are Unicode-aware) against the record's
  text: the collapsed text for HTML, the string itself or compact JSON for
  JSON records. It returns the first participating capture group of the
  first match, or the whole match when there are no groups.

A field takes the first non-empty value among its matches and is omitted
when there is none (no match, missing attribute, blank text, `null`, empty
array or object). `items` selects records (CSS for HTML, JSONPath for JSON;
the `css:`/`json:` prefix is optional); each match becomes a record, even if
none of its fields matched, so records stay aligned with items. Without
`items` the whole document is one record.

Parsed field specs are cached per instance (256 entries), because compiling a
Unicode-aware regex can cost more than extracting a page.

### Limits

| Limit | Value | On violation |
| --- | --- | --- |
| Document | 10 MiB | `document_too_large` |
| Request body | 21 MiB (JSON escaping) | `request_too_large` (checked in Go before copying) |
| Records | `limit` default 1000, maximum 10,000 | `invalid_request` above the maximum |
| Fields | 256 per request | `invalid_request` |
| Selector length | 4 KiB | `invalid_selector` |
| Regex program | 1 MiB compiled (2 MiB DFA cache) | `invalid_selector` |
| Extracted values | 16 MiB total | `output_too_large` |
| Batch values | 100,000 | `request_too_large` |
| Instance memory | 512 MiB by default (Go `Options.MemoryLimitBytes`) | trap; the instance is discarded |

CSS matching cost can grow with document depth times the number of items
(an 8,000-deep nested document with `items: div` takes seconds), so callers
must pass a context with a deadline. The Go extract pool compiles
interruption checks so a deadline stops a running call; see BENCHMARKS.md for
their cost.

## Sandbox

The module has no WASI imports and no host functions beyond the Extism
environment, which the Go host replaces with stubs that trap if called. It
has no filesystem, network, clock or randomness. Each instance has a memory
cap, instances are pooled (`Options.PoolSize`, default `GOMAXPROCS`, per
pool), and an instance is discarded after a trap, a cancellation or a request
over 1 MiB, since linear memory never shrinks.
