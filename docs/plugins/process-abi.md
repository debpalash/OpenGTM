# Process plugin ABI (protocol version 1)

A plugin with `runtime: process` is an ordinary operating-system process that
the Go host starts, talks to over one socket, and stops. This page specifies
that conversation, so a plugin can be written in any language. Python authors
should read the [Python plugin guide](python.md) instead; the SDK implements
everything below.

The machine-readable contract is
[`packages/contracts/plugin-process.v1.schema.json`](../../packages/contracts/plugin-process.v1.schema.json).
The Go tests run the real Python SDK against the real supervisor and validate
every frame in both directions against it.

## Why length-prefixed JSON and not gRPC

The RFC names "Python worker pool and SDK over gRPC". This implementation uses
a smaller transport with the same shape (versioned messages, input and output
schemas, progress, cancellation, structured errors and evidence), for these
reasons:

- **Dependencies.** gRPC needs `grpcio` (a native, per-platform wheel of about
  10 MB) in every plugin's environment, and `google.golang.org/grpc` in the Go
  module, which today has no gRPC. AI and browser plugins already pull SDKs
  that pin their own `protobuf` and `grpcio`; a second, host-imposed pin is a
  recurring source of unsolvable environments. The SDK has no dependencies.
- **Fit.** A run is one connection carrying a handful of message types, with
  at most a few concurrent host calls. HTTP/2 multiplexing, deadlines and
  service discovery buy nothing here; cancellation and deadlines are messages.
- **Debuggability and portability.** A frame is readable in `strace` or a
  hex dump, and a client is about thirty lines in any language (see the end of
  this page). Generated stubs are not needed, and nothing is committed that
  can drift from a `.proto`.

The messages are plain JSON objects described by JSON Schema, which is what the
manifest contract already uses, so the contract has one source of truth. If
workers later run on other hosts (RFC milestone M8), a gRPC transport can carry
the same messages unchanged: version 1 would map one-to-one onto a stream of
`oneof` messages.

## Transport

The host creates a `socketpair(AF_UNIX, SOCK_STREAM)` per run and passes one end
to the child as **file descriptor 3** (also named in `OPENGTM_PLUGIN_FD`). There
is no listening socket: only the process the host started can talk to the host,
and the connection ends with the process. The child's standard input is
`/dev/null`; standard output and error are captured by the host as log lines, so
`print()` can never corrupt the protocol.

A **frame** is a 4-byte big-endian length `N` followed by `N` bytes of UTF-8
JSON holding one object with a string `"type"`. `N` must be between 1 and the
frame limit (16 MiB by default; the `init` frame states it in
`limits.max_frame_bytes`). Either side that reads a malformed or oversized frame
ends the run. The host treats that as a permanent `protocol_error` and kills the
process tree.

Receivers **ignore unknown properties**, and the SDK ignores unknown host
message types. The protocol grows by adding properties and message types; a
breaking change increments the protocol version.

## Conversation

```text
plugin                                   host
  |------------- hello ------------------->|   protocols it speaks, SDK identity
  |<------------ init ---------------------|   run id, inputs, config, secrets, limits, deadline, proxy
  |--- fetch / progress / log / records -->|   any number, any order
  |<--------- fetch_result ----------------|   one per fetch, same id
  |<------------ cancel -------------------|   at most once, when the run must stop
  |------- result  |  failure ------------>|   exactly one, then the plugin exits
```

### Versioning

`hello.protocols` lists every version the plugin implements. The host picks the
highest it also speaks and states it in `init.protocol`. With no common version
the host ends the run with a permanent `protocol_error` that names both sides'
versions. This host speaks `[1]`.

### Messages from the plugin

| `type` | Fields | Meaning |
| --- | --- | --- |
| `hello` | `protocols`, `sdk{name,version,language}`, `kinds?` | First frame. |
| `fetch` | `id`, `url`, `method?`, `headers?`, `body?` or `body_base64?` | Ask the host for one HTTP request. |
| `progress` | `pages?`, `records?`, `fraction?`, `message?` | Best effort; the host forwards at most four a second. |
| `log` | `message`, `level?` | At most 200 lines per run, 4 KiB each, secrets redacted. |
| `records` | `records: [{fields, evidence?}]` | A batch of output rows sent before the result, so output is not bounded by one frame. |
| `result` | `records?`, `cost_usd?`, `pages?`, `stopped?`, `provider_error?` | Terminal: success. |
| `failure` | `code`, `message`, `retryable?`, `details?` | Terminal: failure. |

### Messages from the host

| `type` | Fields | Meaning |
| --- | --- | --- |
| `init` | `protocol`, `run_id`, `plugin{name,version,kind}`, `inputs`, `config?`, `secrets`, `limits`, `deadline_unix_ms`, `proxy?` | Everything the run needs. `secrets` holds only secrets named in the manifest's `capabilities.secrets`. |
| `fetch_result` | `id`, then either `status`, `headers`, `body` or `body_base64`, `evidence` or `error{code,message}` | The answer to a `fetch`. |
| `cancel` | `reason` (`cancelled`, `timeout`, `shutdown`), `grace_ms` | Stop now. After `grace_ms` the host kills the plugin's process group. |

`inputs` have already been validated against the manifest's `inputs` schema.
Records are validated against the `outputs` schema when the manifest declares one.

### Results

A **provider** returns zero or one record; no record plus `provider_error` is its
"no result" answer and the run still succeeds. A **scraper** returns any number
of records (at most 50,000 and 32 MiB per run). **Functions** and **tools**
return one record whose fields are `{"result": value}` and `{"output": value}`,
as in the [WebAssembly ABI](wasm-abi.md). `cost_usd` replaces the manifest's
`cost_per_lookup`; without it a provider that returned a record is charged
`cost_per_lookup`.

The host adds the evidence it observed itself to every record: the fetches it
performed (URL with secrets redacted, status, time, content type, size, SHA-256
of the body) and, for tunnels, bytes per host. What the plugin reports in
`evidence` is kept next to it under `plugin`, and is informational only:

```json
{"runtime": "process", "fetches": [ ... ], "tunnels": [ ... ], "plugin": { ... }}
```

### Errors

A `failure` carries a `code`. The host decides whether to retry from
`retryable` when present, and otherwise from the code:

| Code | Retried | Meaning |
| --- | --- | --- |
| `upstream_error`, `rate_limited`, `timeout` | yes | The data source failed or throttled. |
| `plugin_crashed` | yes | Host-generated: the process died, was killed by a signal, exited without a result, or never said hello. |
| `pool_busy` | yes | Host-generated: no process slot became free within 30 s. |
| `invalid_input`, `auth_failed` | no | The inputs cannot work, or credentials are missing or rejected. |
| `plugin_exception` | no | An unhandled exception in plugin code (the SDK adds a traceback in `details`). |
| `protocol_error` | no | Host-generated: a malformed frame, an unknown message, no common protocol version. |
| `invalid_output` | no | Host-generated: records do not match the manifest, or a provider returned several. |
| `output_too_large` | no | Host-generated: too many records or bytes. |
| `launch_failed` | no | Host-generated: the command cannot be started. |
| anything else | no | Reported as given. |

Retried means the queue runs the attempt again with its usual backoff (three
retries by default) and the run shows `Retrying: <code>: <message>` meanwhile.
When retries are exhausted, or the code is permanent, the run is `failed` with
`<code>: <message>`. A cancelled run is never retried and a late result is never
committed (see [Running plugins on a server](README.md#running-plugins-on-a-server)).

Messages, details and everything else the plugin returns are scanned for the
values of its declared secrets, which are replaced by `REDACTED`.

### fetch

`fetch` is the plugin's way to the network. The host checks the URL (and every
redirect hop) against `capabilities.network`, applies the destination guard and
DNS pinning, honours `robots.txt` for `scraper` and `signal` plugins, applies the
per-domain rate limit and the `limits.max_response_bytes` cap, sets the
identifying `User-Agent`, and counts the call against `limits.max_pages` (the
fetch budget of the run). HTTP error statuses are normal responses. Failures
come back as `error.code`: `capability_denied`, `blocked_url`,
`robots_disallowed`, `body_too_large`, `rate_limited`, `timeout`,
`too_many_fetches`, `bad_request`, `network_unavailable` or `fetch_failed`.
Several fetches may be in flight (the host runs up to four at once); answers
arrive in any order and are matched by `id`.

## What the host guarantees

- **One process per run, never reused.** Nothing a plugin keeps in memory or on
  disk reaches another run or workspace.
- **Bounded capacity.** At most `OPENGTM_PLUGIN_MAX_PROCESSES` plugin processes
  exist at once (default: the smaller of 4 and the CPU count), and at most half
  that many for one plugin, so a busy plugin cannot starve the others. A run
  waits up to 30 s for a slot and then fails with the retryable `pool_busy`.
- **A scrubbed environment.** The child gets `PATH`, `HOME` and `TMPDIR` (both a
  private, empty, `0700` directory deleted afterwards), `LANG`, `LC_ALL`,
  `PYTHON*` and `MALLOC_ARENA_MAX` settings, `SSL_CERT_FILE`, `SSL_CERT_DIR`,
  `TZ`, the proxy variables below and `OPENGTM_PLUGIN_FD`, `_PROTOCOL`, `_DIR`
  and `_RUN`. Nothing else is inherited: not `DATABASE_URL`, not the worker's
  other secrets. Declared secrets are delivered in `init` only, never in the
  environment, argv or a file. Descriptors other than 0-3 are closed.
- **Resource limits** where the operating system has them (Linux, macOS), set
  before the plugin starts: address space `limits.memory_mb` plus 48 MiB of
  interpreter headroom (allocations fail instead of growing), CPU time
  `limits.timeout_seconds` plus 6 s, 256 open files, no core dumps. The
  wall-clock `limits.timeout_seconds` is enforced by the host regardless.
- **Cancellation and timeouts.** On timeout, on a cancelled or lost job lease,
  and on worker shutdown the host sends `cancel`, waits one second, then kills
  the process group with `SIGKILL`. It also kills every descendant it can find by
  parent links and, on Linux, any process still carrying the run's
  `OPENGTM_PLUGIN_RUN` marker, which catches children that called `setsid()` or
  were re-parented to init. This is also done after a *successful* run, so a
  plugin cannot leave daemons behind.
- **Crash detection.** A process that exits, is killed by a signal, closes the
  socket or never sends `hello` without a final message is reported as
  `plugin_crashed` with its exit status or signal and the last 4 KiB of its
  output (redacted).
- **Network.** `fetch` is enforced. Libraries that open their own TLS connections
  get an optional per-run CONNECT proxy (next section); code that ignores it
  can only be stopped by the [sandbox launcher](python.md#isolation-what-it-does-and-does-not-do).

### The tunnel proxy

When the plugin declares `capabilities.network`, the host runs a loopback HTTP
proxy and offers it in `init.proxy.url` and as `HTTPS_PROXY`, `HTTP_PROXY` (and
the lower-case spellings). It accepts only `CONNECT host:port` with the run's
credentials, only for hosts the manifest declares as `https://` patterns without
a path prefix, dials through the guarded egress client (so `OPENGTM_EGRESS_PROXY`
applies and private, loopback and metadata addresses are refused even when
declared), allows 16 tunnels per run, records bytes per host in the evidence,
and revokes everything when the run ends. Plain HTTP requests are refused with
405. Tunnels carry opaque TLS, so there is no URL to check, no `robots.txt` and
no rate limit; therefore `scraper` and `signal` plugins get a proxy only when
they declare `capabilities.browser: true`, and are then responsible for
respecting the sites they visit.

## Crash recovery

Each supervisor records its runs under `OPENGTM_PLUGIN_STATE_DIR` (default
`$TMPDIR/opengtm-plugins-<uid>`, mode `0700`, refused if it is a symlink or
owned by someone else). When a worker is killed:

1. The kernel kills the plugin it started directly (`PR_SET_PDEATHSIG`; every
   launch happens from one dedicated OS thread so that this is reliable).
2. Background processes the plugin started survive that. The next supervisor
   to start (a restarted worker, or `opengtm plugin test`) sweeps the dead
   supervisor's state: it kills what still carries a recorded run marker, and
   signals a process group only if the recorded leader is still the same
   process (matching start time), so a recycled PID can never be hit.
3. The queue's lease recovery reaps the stale job, the failure reconciler marks
   the run `pending` ("Retrying: Heartbeat Timeout"), and another worker runs it
   again. The old attempt's results cannot be committed: they are fenced by the
   job lease.

## A client in thirty lines

```python
import json, os, socket, struct

sock = socket.socket(fileno=int(os.environ["OPENGTM_PLUGIN_FD"]))

def send(msg):
    body = json.dumps(msg).encode()
    sock.sendall(struct.pack(">I", len(body)) + body)

def recv():
    n = struct.unpack(">I", sock.recv(4, socket.MSG_WAITALL))[0]
    return json.loads(sock.recv(n, socket.MSG_WAITALL))

send({"type": "hello", "protocols": [1], "sdk": {"name": "mine", "version": "0", "language": "python"}})
init = recv()
send({"type": "result", "records": [{"fields": {"echo": init["inputs"]}}]})
```
