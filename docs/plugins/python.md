# Writing Python plugins

Use a Python plugin (`runtime: process`) when YAML and WebAssembly are not
enough: AI and model SDKs, browser automation, heavy dependency stacks, or code
you would rather write than configure. The Go host starts your plugin in its own
process, gives it its inputs and only the secrets it declared, answers its
HTTP requests through the same guarded client every other runtime uses, and
kills the whole process tree if it times out, is cancelled or crashes.

This guide covers the SDK ([`packages/sdk-python`](../../packages/sdk-python),
import name `opengtm_sdk`). The wire protocol underneath is specified in
[process-abi.md](process-abi.md); manifest fields are in the
[plugin guide](README.md#manifest-reference).

- [A provider in about twenty lines](#a-provider-in-about-twenty-lines)
- [Handlers by kind](#handlers-by-kind)
- [The context](#the-context)
- [Errors, retries and "no result"](#errors-retries-and-no-result)
- [Network access](#network-access)
- [Testing with fixtures](#testing-with-fixtures)
- [Dependencies and the interpreter](#dependencies-and-the-interpreter)
- [Running on a server](#running-on-a-server)
- [Isolation: what it does and does not do](#isolation-what-it-does-and-does-not-do)
- [Troubleshooting](#troubleshooting)

## A provider in about twenty lines

```bash
opengtm plugin new provider acme_firmographics --runtime process
pip install opengtm-sdk     # in a checkout: pip install -e packages/sdk-python
opengtm plugin test acme_firmographics
```

`plugin.yaml` is a normal v2 manifest whose `process.command` starts your code:

```yaml
manifest_version: "2"
name: acme_firmographics
kind: provider
runtime: process
version: 0.1.0
capabilities:
  network: ["https://api.acme-data.example/v1"]   # ctx.fetch may only reach these
  secrets: [ACME_DATA_API_KEY]                    # ctx.secret may only read these
limits: { timeout_seconds: 15, max_pages: 2, memory_mb: 128 }
inputs: { domain: string }
outputs: company
capability: company_size
process:
  command: [python3, main.py]
```

`main.py`:

```python
from opengtm_sdk import Retryable, provider, run


@provider
def enrich(ctx, domain: str):
    r = ctx.fetch(
        "https://api.acme-data.example/v1/companies",
        params={"domain": domain},
        headers={"X-Api-Key": ctx.secret("ACME_DATA_API_KEY")},
    )
    if r.status == 404:
        return None  # the API does not know this domain: a normal "no result"
    body = r.json()
    if body.get("error"):
        raise Retryable(body["error_message"])  # transient: the queue will retry
    return {"company_size": body["company"]["employees"]}


if __name__ == "__main__":
    run()
```

A complete, tested version with fixtures is
[`plugins/examples/python-provider`](../../plugins/examples/python-provider).

Inputs arrive as keyword arguments named after the manifest's `inputs`
(parameters you do not declare are simply not passed; a missing required one
fails the run with the permanent `invalid_input`). Take `**inputs` to receive
all of them.

## Handlers by kind

The manifest `kind` selects which registered handler runs. A file may register
several (the SDK's own tests do), but a plugin has one kind.

| Decorator | Returns | Becomes |
| --- | --- | --- |
| `@provider` | a `dict` of fields, a `Record`, or `None` | One record. `None`, an empty dict or `raise NoResult("not_found")` is the run's `provider_error` ("no result"), not a failure. |
| `@scraper` | an iterable, generator or async generator of `dict` or `Record` | One record per item, streamed to the host in batches of 200, so output is not bounded by one message. At most 50,000 records and 32 MiB per run. |
| `@function` | any JSON value | One record `{"result": value}`. |
| `@tool` | any JSON value | One record `{"output": value}`. |

Handlers may be `async def`. A `dict` is always the record's *fields*; use
`Record(fields, evidence=ctx.evidence(url))` to attach your own evidence. Fields
must match the manifest's `outputs` schema when it is a JSON Schema (an entity
name such as `company` is not validated). Datetimes, `Decimal`, dataclasses,
sets and bytes (base64) are serialized for you; anything else that is not JSON
fails the run as `plugin_exception`.

```python
@scraper
async def team(ctx, domain: str):
    page = await ctx.afetch(f"https://{domain}/team")      # robots.txt applies
    for name, role in parse(page.text):
        yield Record({"full_name": name, "title": role}, evidence=ctx.evidence(f"https://{domain}/team"))
    ctx.progress(records=n)
```

The host adds the evidence it observed itself (every fetch with URL, status,
time, size and SHA-256) to each record, so waterfalls and cell traces have
provenance even if your code reports none.

## The context

| | |
| --- | --- |
| `ctx.inputs`, `ctx.config` | Run inputs; the workspace configuration for the plugin. |
| `ctx.secret(name)` | A declared secret, or `AuthFailed` (permanent) when the workspace has none. Secrets arrive over the control socket, never in the environment. |
| `ctx.fetch(url, method=, params=, headers=, body=, json=)` | One HTTP request through the host. Returns a `Response` (`status`, `headers`, `body`, `text`, `json()`, `ok`, `evidence`). HTTP error statuses are responses; host refusals raise `FetchError`. `await ctx.afetch(...)` in async code. |
| `ctx.log(message, level=)` | Log line (at most 200 per run, secrets redacted). `print()` is captured too. |
| `ctx.progress(pages=, records=, fraction=, message=)` | Progress on the events stream, at most four updates a second. |
| `ctx.add_cost(usd)` | Cost of the run; replaces the manifest's `cost_per_lookup`. |
| `ctx.pages`, `ctx.stopped` | Optional counters reported with the result (`stopped` explains early termination). |
| `ctx.cancelled`, `ctx.check_cancelled()`, `ctx.time_left()` | Cancellation and the host deadline. Long loops should call `check_cancelled()`. |
| `ctx.limits` | The manifest limits in force. |
| `ctx.proxy_url` | The tunnel proxy for libraries with their own sockets, or `None` ([below](#network-access)). |
| `ctx.evidence(url, **extra)` | A source-and-time evidence dict for a `Record`. |

When the host cancels a run (the user cancelled it, the timeout passed, the
worker is shutting down) the SDK raises `Cancelled` inside your handler: it wakes
`time.sleep` and blocking socket calls, stops pure-Python loops and cancels
async tasks. `Cancelled` derives from `BaseException`, so `except Exception`
does not swallow it. If your code does not stop within one second, the host
kills the process group.

## Errors, retries and "no result"

Raise one of these to report a structured failure; anything else that escapes is
a permanent `plugin_exception` with a traceback in the details.

| Raise | Code | Retried by the queue |
| --- | --- | --- |
| `Retryable(msg)` | `upstream_error` | yes |
| `RateLimited(msg)` | `rate_limited` | yes |
| `InvalidInput(msg)` | `invalid_input` | no |
| `AuthFailed(msg)` | `auth_failed` | no |
| `Permanent(msg, code="...")` | `plugin_exception` or your code | no |
| `Retryable(msg, code="...")` | your code | yes |
| `NoResult(reason)` / return `None` (providers) | not a failure | n/a |
| `FetchError` escaping from `ctx.fetch` | the host's code | transient ones (`timeout`, `rate_limited`, `fetch_failed`, `network_unavailable`) yes; `capability_denied`, `blocked_url`, `robots_disallowed`, `too_many_fetches`, `body_too_large` no |

Anything the host detects is separate: a crash, a kill by signal, an exit
without a result, or no hello is `plugin_crashed` and **is retried**; exceeding
`limits.timeout_seconds` is `timeout` and **is retried**; a plugin that sends an
invalid frame or output that breaks the manifest fails permanently. After the
queue's retries (three by default) the run is `failed` with
`<code>: <message>`. Use `retryable=True` or `False` to override a code's
default. A cancelled run is never retried.

## Network access

Everything goes through the host:

- **`ctx.fetch`** is the sanctioned way. The host checks the URL and every
  redirect hop against `capabilities.network`, refuses private, loopback and
  metadata addresses (even if declared), pins DNS, applies `robots.txt` for
  scrapers, the per-domain rate limit, the response size cap and the fetch
  budget (`limits.max_pages` calls per run), uses an identifying `User-Agent`
  you cannot change, and honours `OPENGTM_EGRESS_PROXY`.
- **Libraries with their own sockets** (an LLM vendor SDK, `httpx`, `requests`,
  a Playwright browser) use the **tunnel proxy**: while the plugin declares
  `capabilities.network`, the process gets `HTTPS_PROXY` and `ctx.proxy_url`,
  and can reach exactly the declared hosts through the guarded egress dialer.
  Tunnels carry opaque TLS, so patterns with a path prefix never match them, plain
  `http://` is refused, and there is no robots.txt or rate limiting. For that
  reason scrapers get the proxy only if they declare `capabilities.browser:
  true`, and then must respect robots.txt themselves. The host records bytes per
  host in the evidence.

  ```python
  import anthropic, httpx
  client = anthropic.Anthropic(api_key=ctx.secret("ANTHROPIC_API_KEY"))   # honours HTTPS_PROXY
  ```

  With Playwright, pass `proxy={"server": "http://host:port", "username": ..., "password": ...}`
  built from `urllib.parse.urlparse(ctx.proxy_url)`. The tunnel is tested with a
  raw client; Playwright itself, and a shared browser pool, are not part of this
  repository's tests.

The default launcher cannot stop code that ignores the proxy settings from
opening its own sockets. See [Isolation](#isolation-what-it-does-and-does-not-do).

## Testing with fixtures

Fixtures are the tests, exactly as for the other runtimes:
`opengtm plugin test <dir>` replays `fixtures/<case>/case.yaml` offline. Every
`ctx.fetch` goes through the real egress client over the recorded responses, so
capabilities, robots.txt and size caps are checked as in production, and a
request with no recorded response fails the case.

```yaml
input: { domain: acme.example }
secrets: { ACME_DATA_API_KEY: test-key }        # fake values for declared secrets
http:
  - { method: GET, url: "https://api.acme-data.example/v1/companies?domain=acme.example",
      status: 200, headers: { Content-Type: application/json }, body_file: company.json }
expect:
  fields: { company_size: 250 }                  # providers, functions, tools
  # records: [ { full_name: Ada } ]              # scrapers
  # error: no_data                               # a provider's "no result"
  # error: "upstream_error: monthly quota exceeded"   # a failure the plugin reported
```

For a failure the *plugin* reported, `expect.error` is `"<code>: <message>"`.
Faults of the machinery (crash, timeout, protocol violation, invalid output,
launch) fail the case instead, so a broken plugin cannot pass by "expecting" its
own crash. `opengtm plugin record` runs the plugin against the live network and
writes the fixtures; `opengtm plugin dev` reruns the tests on every save.

`opengtm plugin test` finds the SDK through the interpreter: either
`pip install opengtm-sdk` into it, or point at a checkout:

```bash
export OPENGTM_PLUGIN_PYTHONPATH=$PWD/packages/sdk-python/src   # development
export OPENGTM_PLUGIN_PYTHON=$HOME/venvs/plugins/bin/python      # a venv with your dependencies
```

The SDK's own tests run with
`uv run --no-project --with pytest --with jsonschema pytest packages/sdk-python/tests`.

## Dependencies and the interpreter

`process.command` is an argv list. A bare `python3` (or `python`) is looked up on
the host's `PATH`, or replaced by `OPENGTM_PLUGIN_PYTHON`; an element with a
slash is relative to the plugin directory. The plugin runs with the plugin
directory as its working directory and a scrubbed environment, with `HOME` and
`TMPDIR` pointing at a private, empty, per-run directory. Packages installed with
`pip install --user` are therefore not visible: install your dependencies, and
`opengtm-sdk`, into the interpreter named by `OPENGTM_PLUGIN_PYTHON`.

`limits.memory_mb` is applied as an address-space limit (`RLIMIT_AS`) plus 48 MiB
of interpreter headroom. It counts reserved virtual memory, so declare a
realistic number for your dependencies (a bare interpreter fits in 32; `anthropic`
or `pandas` want 512 or more). Exceeding it raises `MemoryError` rather than
killing the process. The SDK needs Python 3.10 or newer and has no dependencies.

## Running on a server

Process plugins need a Linux or macOS worker with Python. The default server
image is distroless (no shell, no Python), so runs of process plugins in it fail
permanently with `launch_failed`; build a worker image on a Python base that
contains the `opengtm` binary, the plugins, the SDK and your dependencies, and
set the variables below. (This repository does not ship that image.) Windows is
not supported: the API lists process plugins as not runnable there.

| Variable | Default | |
| --- | --- | --- |
| `OPENGTM_PLUGIN_MAX_PROCESSES` | smaller of 4 and the CPU count | Plugin processes alive at once, across all plugins. |
| `OPENGTM_PLUGIN_MAX_PER_PLUGIN` | half of the above, at least 1 | Processes of one plugin. |
| `OPENGTM_PLUGIN_PYTHON` | `python3` on `PATH` | Interpreter used when a command starts with `python` or `python3`. |
| `OPENGTM_PLUGIN_PYTHONPATH` | none | Path list added to the plugin's `PYTHONPATH` (an uninstalled SDK checkout). |
| `OPENGTM_PLUGIN_STATE_DIR` | `$TMPDIR/opengtm-plugins-<uid>` | Run directories and the crash-recovery registry. Mode `0700`; refused if it is a symlink. |
| `OPENGTM_PLUGIN_SANDBOX` | `exec` | `bwrap` selects the [bubblewrap launcher](#isolation-what-it-does-and-does-not-do). |
| `OPENGTM_PLUGIN_SANDBOX_RO` | none | Extra paths bound read-only into the sandbox (a venv outside `/usr`). |
| `OPENGTM_EGRESS_PROXY` | none | Upstream proxy for `ctx.fetch` and for tunnels. |

Capacity is bounded twice: the queue's `WORKER_CONCURRENCY` limits jobs, and the
process pool limits plugin processes. A run that cannot get a slot within 30 s
fails with the retryable `pool_busy` (and the queue's backoff) instead of
holding a worker forever. Sizing: each process may use up to `memory_mb`, so the
pool bounds the worst-case memory of plugins at
`OPENGTM_PLUGIN_MAX_PROCESSES x memory_mb`.

If a worker is killed, its plugin processes die with it (or are swept when the
next worker starts), its job lease expires, and another worker runs the job
again; the first attempt can never commit results.
[process-abi.md](process-abi.md#crash-recovery) has the details.

## Isolation: what it does and does not do

The default launcher (`exec`) runs the plugin as a child of the worker, as the
same OS user, with:

| Does | Does not |
| --- | --- |
| Scrub the environment: no `DATABASE_URL`, no other plugin's or workspace's secrets. | Hide the filesystem. A plugin can read any file its OS user can, including the plugin directory and `~/.ssh`. |
| Deliver only declared secrets, over the control socket, never in env, argv or files. | Stop plugin code from opening sockets that ignore `HTTPS_PROXY`. |
| Give each run a private, empty, `0700` HOME and TMPDIR, deleted afterwards, and close every descriptor except the control socket. | Prevent a plugin from writing to its own (installed) directory. |
| Make the worker non-dumpable (`PR_SET_DUMPABLE`) so a plugin cannot read the worker's `/proc/<pid>/environ` or `mem` or ptrace it; and the SDK does the same to itself so concurrent plugins cannot read each other's. | Protect a plugin that does not use the SDK from other plugins' `/proc/<pid>/environ` (Yama's `ptrace_scope` 1, the default on most distributions, still blocks reading its memory). |
| Apply address-space, CPU-time, open-file and core limits; enforce a wall-clock timeout; kill the whole process tree. | Limit processes (`RLIMIT_NPROC` is per user, not per plugin), disk use or network bandwidth. |

**Platforms.** Everything above is tested on Linux. On macOS the plugin runs and
is killed by process group, but there is no `/proc`: children that leave their
process group are not found, a killed worker's leftovers are not swept, and the
parent-death kill and `/proc` hardening do not exist. Use Linux workers in
production. Resource limits are applied through the shell's `ulimit`; a limit the
system refuses (for example address space on macOS) is skipped, and the
wall-clock timeout is the backstop.

Treat it as protection against **mistakes and honest plugins**, plus a barrier
against secret leakage by omission: it is not a sandbox for hostile code. Run
plugins you do not trust under the bubblewrap launcher, in their own container
(a worker image per trust level), or as a separate OS user.

`OPENGTM_PLUGIN_SANDBOX=bwrap` runs each plugin inside
[bubblewrap](https://github.com/containers/bubblewrap): a mount namespace with
only `/usr`, `/bin`, `/lib*`, certificates and the plugin directory (read-only),
a private `/tmp` and `/proc`, new PID, IPC and UTS namespaces, **no network**, and
death with the worker. Tests show that a sandboxed plugin cannot read a host
file, cannot open a socket to a local listener, and cannot see the worker
process, while the same plugin under `exec` can do the first two. The only way
out is `ctx.fetch`, so libraries that need their own sockets (vendor SDKs, a
browser) do not work under it, and no tunnel proxy is offered. It needs
unprivileged user namespaces (`kernel.unprivileged_userns_clone`) and `bwrap` on
the worker; paths outside `/usr` (a venv) must be listed in
`OPENGTM_PLUGIN_SANDBOX_RO`. Launchers are an interface (`process.Launcher`), so
nsjail, a container runtime or gVisor can be added without touching the
supervisor; none besides `exec` and `bwrap` is included.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `plugin_crashed: ... ModuleNotFoundError: No module named 'opengtm_sdk'` | The interpreter cannot import the SDK. `pip install opengtm-sdk` into it, or set `OPENGTM_PLUGIN_PYTHONPATH` (and `OPENGTM_PLUGIN_PYTHON`). |
| `launch_failed: process.command[0] "python3" not found` | No interpreter on the worker's `PATH`; set `OPENGTM_PLUGIN_PYTHON`. |
| `plugin_crashed: plugin was killed by signal ...` | A native crash, an out-of-memory kill, or `RLIMIT_CPU`. Look at the last output in the message. |
| `MemoryError` or `plugin_exception: MemoryError` | `limits.memory_mb` is an address-space limit; raise it. |
| `timeout: plugin exceeded limits.timeout_seconds` | The run was killed (limit at most 300 s). Report progress, page through results across runs, or raise the limit. |
| `capability_denied` from `ctx.fetch` | The URL is outside `capabilities.network`: check scheme, host, port and path prefix. |
| `pool_busy` | All `OPENGTM_PLUGIN_MAX_PROCESSES` slots stayed busy for 30 s. |
| `protocol_error` | The plugin wrote to file descriptor 3, or a non-SDK client sent a bad frame. |
| Fixture case fails with `unrecorded request` | `ctx.fetch` asked for a URL the case does not record (including query order-insensitively and `robots.txt`, which defaults to 404). |
