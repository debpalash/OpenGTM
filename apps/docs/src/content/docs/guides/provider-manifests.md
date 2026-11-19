---
title: Provider manifests (contributor guide)
description: The YAML manifest format for BYOK data providers, a worked example, how to test one, the security rules, and the pull request checklist.
sidebar:
  order: 12
---

A provider manifest is one YAML file. It describes one HTTP call to a vendor
API: how to authenticate, what to send, and which response fields to keep.
Once the file is in the repository, the provider works in enrichment
waterfalls with the user's own key. No Go code is needed.

This page is for contributors who add a manifest. For the list of providers
and how to set keys, see [Enrichment providers](/guides/providers/).

## What a manifest can and cannot do

A manifest can make **one synchronous HTTP request** and read **flat values**
from the JSON answer.

It cannot:

- poll or wait for a webhook (vendors with async results are out of scope);
- send a request in more than one step;
- send form-encoded or multipart bodies (JSON bodies and query strings only);
- join or loop over arrays (`items[]` takes the first non-empty item);
- use two credentials in the Settings screen (a second key needs
  `auth.extra_env` and is not covered by the Settings key field).

If the vendor needs one of these, do not stretch the manifest. Open an issue
and name the vendor.

## Where files go

```text
apps/backend-go/internal/manifest/builtin/manifests/<capability>/<name>.yaml
```

Use the primary capability as the folder name (`email`, `email_verify`,
`phone`, `linkedin_url`, `company_size`, and so on). The folder is only for
tidiness. The file is picked up by `go:embed`; there is no list to edit.

## Field reference

This table comes from `internal/manifest/types.go`, `registry.go` and
`execute.go`. The checks in the last column run in CI.

### Top level

| Field | Required | Default | Meaning |
|---|---|---|---|
| `manifest_version` | no | `"1"` | Must be the string `"1"`. |
| `name` | yes | | Provider id. Lowercase letters, digits and `_`, 3 to 64 characters, starts with a letter. Must be unique. This is the name users put in a waterfall. |
| `display_name` | no | title-cased `name` | Label for the catalog. |
| `author` | no | `community` | Your GitHub handle or `OpenGTM`. |
| `homepage` | yes, by policy | | The vendor's official API documentation URL for this endpoint. A test fails if it is empty. |
| `license` | no | `Apache-2.0` | Licence of the manifest file. |
| `tags` | no | | Free labels, for example `[email, enrichment, byok]`. |
| `capability` | yes | | The primary field this provider fills, for example `email`. |
| `capabilities` | no | | All fields it can fill. Include `capability`. |
| `description` | no | | One line: endpoint, input, output. |
| `default_confidence` | no | `0.7` | A number from 0 to 1 attached to every hit. |
| `cost_per_lookup` | no | `0` | USD per call. Use the vendor's published price. If it is not published as a per-call price, leave `0` and say so in a comment. `0` means unknown, not free. |
| `input_fields` | no | | Informational list of lead fields used. It is not enforced. |

### `auth`

| Field | Default | Meaning |
|---|---|---|
| `type` | `none` | `header`, `bearer`, `query` or `none`. Anything else fails to load. |
| `param` | `Authorization` for `header`, `api_key` for `query` | Header name or query parameter name that carries the key. Ignored for `bearer`. |
| `env_var` | | Name of the credential, for example `ACME_API_KEY`. Required unless `type` is `none`. The value is read from the workspace secret, then Settings, then the environment. |
| `value` | | Optional template for the credential, for example `Basic ${env:ACME_API_KEY}`. It must contain `${env:NAME}`. A literal secret is refused. |
| `extra_env` | | More credential names the manifest may use as `${env:NAME}`. Avoid; see the limits above. Connectors that declare it are not run by the worker; use `secondary` instead. |
| `secondary` | | A second credential, for vendors that need a key and a secret. See below. |

How the key is placed:

- `bearer`: header `Authorization: Bearer <key>`.
- `header`: header `<param>: <key>`.
- `query`: query parameter `<param>=<key>`.

If the credential is missing, no request is made and the result is
`<name>: missing <ENV_VAR>`.

#### `auth.secondary`

Some vendors need two values, for example Tomba (`X-Tomba-Key` and
`X-Tomba-Secret`). Declare the second one explicitly:

```yaml
auth:
  type: header
  param: X-Tomba-Key
  env_var: TOMBA_API_KEY
  secondary:
    type: header            # header or query only
    param: X-Tomba-Secret
    env_var: TOMBA_SECRET_KEY
```

Rules:

- `secondary.type` is `header` or `query`. Both values go only into a header
  or a query parameter, never into the URL path, a body or a log line.
- If either value is missing, no request is made and the result is
  `<name>: missing <ENV_VAR>`.
- The credential allow-list is exactly `auth.env_var` plus
  `auth.secondary.env_var`. A third `${env:NAME}` does not resolve.
- Both names need an `enrichment_providers` card and a `secret_app_keys` entry
  in `settings/registry.json`, one card per name. Neither may be a platform
  secret on the deny-list; the validator refuses the manifest if it is.
- Both values are removed from error bodies and transport errors.

### `request`

| Field | Default | Meaning |
|---|---|---|
| `method` | `POST` | `GET`, `POST`, `PUT` or `PATCH`. |
| `url` | required | Must start with `https://`. May contain `{{input.x}}` after the host. |
| `headers` | | Map of header name to template. |
| `query` | | Map of parameter name to template. Added to any query already in `url`. |
| `body` (or `body_template`) | | JSON body. Strings anywhere inside are templates. Sent with `Content-Type: application/json` unless you set that header. |
| `timeout` | `20` | Seconds. Must be above 0 and at most 120. |
| `omit_empty` | none | List of header names, query parameter names and top-level body keys. Each one is left out of the request when its rendered value is the empty string. Without it an empty input is sent as `""`. Credentials added by `auth` are never removed. |

The response body is read up to 1 MiB and must be JSON.

### `response`

| Field | Meaning |
|---|---|
| `mappings` | Required, not empty. Map of OpenGTM field name to a path in the response. |
| `error_path` | Optional path. If it holds a truthy value (non-empty string, `true`, non-zero), the call is a failure even though the HTTP status was 2xx. |
| `error_message_path` | Optional path of the text to report. Cut to 120 characters. |
| `value_maps` | Optional. Translates the value of a mapped field. See below. |
| `success_when` | Optional condition on the (final) response. If it does not match, the call is a failure. See below. |

#### `success_when` and conditions

Some vendors answer HTTP 200 with a body that says the lookup failed or found
nothing usable. A condition decides. It is used by `success_when`,
`poll.done_when` and `poll.failed_when`:

| Key | Meaning |
|---|---|
| `path` | A path in the JSON body, for example `$.status`. |
| `equals` | Matches when the value at `path` equals this scalar. Text, numbers and booleans are compared as text (`true`, `42`). |
| `in` | Matches when the value is any one of this list. |
| `status` | A list of HTTP status codes. Matches when the status is one of them. |

A condition needs `path` with `equals` or `in`, or `status`, or both kinds
(then both must match). A missing, null or empty value never matches. When
`success_when` fails, the result is the text of `error_message_path` (cut and
scrubbed) or `no_data`.

```yaml
response:
  success_when: {path: $.status, equals: success}
```

#### `value_maps`

Use it when the vendor's words differ from the words OpenGTM uses for the
field. The key is a field name from `mappings`.

```yaml
response:
  mappings:
    email_verify: "$.status"
  value_maps:
    email_verify:
      values:
        deliverable: valid
        undeliverable: invalid
        risky: risky
      default: unknown
```

- `values` maps the vendor's value (matched exactly, as text, so the number
  `1` matches the key `"1"`) to the value to store. Quote keys that YAML
  could read as a boolean (`"yes"`, `"no"`, `"on"`, `"off"`).
- `default` is stored for any vendor value that is not listed. If you leave it
  out, an unlisted value is stored as it is. State the fallback in the
  manifest; there is no built-in one.
- A missing or empty vendor value is never mapped, so the result is still
  `no_data`. A vendor error is reported before any mapping.
- Both sides are plain text. They are never treated as templates, so a map
  cannot pull in a lead value or a credential.
- Every key must also be in `mappings` (the connector validator checks it).

**`email_verify` rule.** Downstream code reads these words: `valid`,
`invalid`, `catch_all`, `risky`, `unknown`. A manifest that maps
`email_verify` must carry a `value_maps` entry with `default: unknown` whose
values are only those five words. A test enforces it. Follow the cascade:
`ok`, `deliverable`, `safe` become `valid`; `undeliverable`, `rejected`,
`bad` and `disposable` become `invalid`; `accept_all` and `catch_all` become
`catch_all`.

Path syntax in `mappings`, `error_path` and `error_message_path`:

| Path | Result |
|---|---|
| `$.a.b` | Value at `a` then `b`. |
| `$.a[]` | First non-empty item of the array `a`. |
| `$.a[2]` | Third item of `a`. |
| `$.a[].b` | `b` of the first item that has a non-empty `b`. |
| `plain text` | A literal, no `$.` in it. |
| `Prefix $.a` | The prefix, then the value as text. |

A mapping that gives nothing, or an empty string, is dropped. If every
mapping is dropped the result is `no_data`. The fields `phone` and
`mobile_phone` are normalised to E.164 when possible.

### `poll` (asynchronous vendors)

Some vendors accept a lookup, return an id, and finish later. A `poll` block
adds a second request that is repeated until the vendor says it is done. One
lookup is still one `Execute` call; `Result.Requests` shows the HTTP requests
that were made.

```yaml
poll:
  request:
    method: GET
    url: https://api.example.com/v1/enrich/{{response.request_id}}
    timeout: 20
  interval: 5          # seconds waited before every poll request, 0.1 to 60
  max_attempts: 24     # poll requests, 1 to 60
  max_total: 120       # seconds for the whole sequence, at most 120
  done_when: {path: $.status, equals: terminated}
  failed_when: {path: $.status, equals: on_hold}   # optional
```

- `poll.request` has the same fields as `request`. Write `body_template`
  there, not `body`: the `body` shorthand works only at the top level.
- `{{response.path}}` reads a value from the first response. It is escaped
  like `{{input.x}}`, so an id cannot add path segments or a host. If the
  value is missing, no poll request is made (`poll_missing_reference`).
- The poll URL host must be written literally and must equal the request host
  or be listed in `poll.allowed_hosts` (plain host names). Redirects are not
  followed. The same credentials, egress guard and https rule apply.
- `failed_when` is checked before `done_when`. Both may test a body field and
  the HTTP status.
- Poll errors: `poll_failed`, `poll_max_attempts`, `poll_timeout`,
  `poll_missing_reference`.
- A vendor error on the first response (`error_path`) ends the lookup without
  polling.

**Timeouts.** One rule, in `Manifest.CallTimeout`:

- An ordinary manifest (no `poll`) is bounded by the workspace's
  `provider_timeout` (default 10 s, 3 to 120).
- A manifest with a `poll` block is bounded by its own budget: the first
  request's `timeout` plus `poll.max_total` (at most 120 s). The workspace
  `provider_timeout` does not apply to it, so a shipped polling vendor works
  with default settings.
- Cancelling the run (job cancelled, caller context done) ends either kind at
  once; that is an infrastructure error, not a timeout.
- A poll that uses up `max_total` is recorded as one `timeout` for the
  provider (it counts once toward the timeout cooldown). A poll that is still
  running records nothing.

**Worker slots.** A polling lookup holds one provider worker for its whole
duration, up to `max_total`. Provider workers default to 8 per run, so at most
8 polling lookups run at once. A 25-row run whose rows all reach a polling
provider therefore takes about `ceil(25 / 8) = 4` rounds of up to `max_total`
each (up to 4 x 120 s for Dropcontact) while those workers are busy, and other
providers in the same run wait for a free worker. Put polling providers last in
a waterfall: faster providers then fill most rows first and only the leftover
rows pay the polling wait.

### Templates

- `{{input.name}}` is replaced by a lead value. Missing values become an
  empty string. `{{input.name | default: x}}` uses `x` instead.
- Available inputs: `company`, `company_name`, `website`, `domain` (host of
  `website`, without scheme and `www.`), `email`, `linkedin_url`, `city`,
  `contact_person`, `full_name` (same as `contact_person`), `first_name`,
  `last_name`, `phone`.
- Values substituted into `request.url` are escaped. In the path, `/`, `?`,
  `#`, `%` and spaces are percent-encoded and a value of `.` or `..` cannot
  act as a path element. After a `?` the value is query-escaped. A lead value
  therefore cannot add path segments, parameters or a fragment. Defaults and
  your own text are written as you typed them.
- `${env:NAME}` is replaced by a credential. It works only in text you
  wrote in the manifest, never in a lead value, and only for `auth.env_var`
  and `auth.extra_env`. `auth.secondary.env_var` is also allowed.

### Results and errors

| Result | Meaning |
|---|---|
| hit | At least one mapped field has a value. |
| `no_data` | The call worked and every mapped value was empty. |
| `http_<status>` | HTTP status 400 or above. The body is not read. |
| `non_json_response` | The body was not JSON, or was larger than 1 MiB. |
| text from `error_message_path` | `error_path` was truthy. |
| `<name>: missing <ENV_VAR>` | No credential. No request was made. |
| `blocked_url: ...` | The URL was refused by the egress guard. |
| `timeout` | The call took longer than `request.timeout`, or a poll ran past the caller's deadline. |
| `poll_failed`, `poll_max_attempts`, `poll_timeout`, `poll_missing_reference` | See `poll` above. |

## Worked example

Findymail "Find from name"
(docs: <https://app.findymail.com/docs/>). This is the shipped file
`manifests/email/findymail_email.yaml`.

```yaml
# Findymail "Find from name" -- declarative provider manifest.
# Docs: https://app.findymail.com/docs/ (POST /api/search/name).
# Auth: Authorization: Bearer <key>. Synchronous unless webhook_url is sent,
# and this manifest never sends it.
# Billing: 1 finder credit when an email is found, free otherwise. The credit
# price depends on the plan, so cost_per_lookup is 0 (unknown).
manifest_version: "1"
name: findymail_email
display_name: Findymail Email Finder
author: OpenGTM
homepage: https://app.findymail.com/docs/
tags: [email, enrichment, byok]
capability: email
capabilities: [email]
description: Findymail /api/search/name - full name + company domain -> verified work email.
default_confidence: 0.8
cost_per_lookup: 0
auth:
  type: bearer
  env_var: FINDYMAIL_API_KEY
request:
  method: POST
  url: https://app.findymail.com/api/search/name
  headers:
    Content-Type: application/json
  body:
    name: "{{input.full_name}}"
    domain: "{{input.domain}}"
  timeout: 30
response:
  error_path: error
  error_message_path: error
  mappings:
    email: "$.contact.email"
```

The vendor documents this answer:

```json
{ "contact": { "name": "John Doe", "domain": "website.com", "email": "john.doe@website.com" } }
```

and this failure, with HTTP status 402:

```json
{ "error": "Not enough credits" }
```

## Add a manifest, step by step

1. **Read the vendor's official API documentation.** Confirm the method, URL,
   auth scheme, parameters, and a sample response. If you cannot confirm a
   field from the vendor's own documentation, leave it out. Do not guess an
   endpoint.
2. **Check it fits.** One synchronous call, JSON in and out. If not, stop.
3. **Write the manifest** under `manifests/<capability>/<name>.yaml`. Put the
   docs URL in `homepage` and in a comment, with the billing facts.
4. **Add the fixture directory** `internal/manifest/builtin/testdata/vendors/<name>/`
   (see below).
5. **Wire it into the app.** A manifest works from the UI only when four
   places agree. A test (`TestPackManifestsAreWiredIntoTheApp`) fails with a
   message that names the missing one:
   - `internal/vendorcatalog/catalog.go`: add an entry with the same
     capabilities and the same cost as `cost_per_lookup`.
   - `internal/wbregistry/registry.json`: add the provider (capabilities,
     confidence, `"maturity": "beta"`), and add the same object to
     `testdata/contract/meta/providers.json` and
     `testdata/contract/meta/providers_no_ws_header.json`.
   - `internal/settings/registry.json`: add an `enrichment_providers` entry
     for the vendor (one per key, not per manifest) and add the key to
     `secret_app_keys` so it is stored encrypted. Add the card to
     `testdata/contract/settings/enrichment_providers_before.json` and
     `..._after.json`.
   - Raise the counts in `manifest/builtin/builtin_test.go`,
     `internal/api/connectorsources_test.go` and
     `internal/api/connectorsroutes_test.go`.
6. **Add the key to `.env.example`** and to the key list in
   [Configuration](/self-hosting/configuration/).
7. **Add a row** to the provider table in [Enrichment providers](/guides/providers/).

The page `docs/connectors/README.md` in the repository root has the same
pointer for people who start from the connector folder.

## Testing recipe

One test, `TestVendorManifestsAgainstDocsSamples` in
`internal/manifest/builtin/vendors_test.go`, covers every manifest. You add
data, not code. For each directory under `testdata/vendors/<name>/` it:

1. loads `<name>` from the embedded manifests (so the YAML must load and
   validate);
2. starts a local TLS server and sends the manifest's real `https://` URL to
   it;
3. runs the manifest once per entry in `responses`;
4. checks the method, host, path, query, headers and JSON body of the request;
5. checks that the credential appears only where `auth` puts it (a header or
   a query parameter), never in the body;
6. checks the mapped fields on a hit, or the exact error on a miss;
7. checks that a missing key makes no request.

The directory holds `case.json` and one file per vendor response:

```json
{
  "docs": ["https://app.findymail.com/docs/ (Find from name)"],
  "lead": { "website": "https://www.acme.com/about", "contact_person": "Ada Lovelace" },
  "credential": "test-key-123",
  "request": {
    "method": "POST",
    "host": "app.findymail.com",
    "path": "/api/search/name",
    "query": {},
    "headers": { "Authorization": "Bearer test-key-123", "Content-Type": "application/json" },
    "body": { "name": "Ada Lovelace", "domain": "acme.com" }
  },
  "responses": [
    { "name": "hit", "status": 200, "file": "hit.json", "fields": { "email": "john.doe@website.com" } },
    { "name": "vendor_error", "status": 402, "file": "error.json", "error": "http_402" },
    { "name": "vendor_error_in_200_body", "status": 200, "file": "error.json", "error": "Not enough credits" }
  ]
}
```

Rules for the fixtures:

- Copy the response bodies from the vendor's documentation. If the docs show a
  placeholder such as `<email>` or an obfuscated address, replace only that
  value with a realistic one and say so in `docs`.
- Do not add a field the docs do not show.
- Include at least one hit, one answer with no data (when the vendor documents
  one), and one vendor error. For a verifier, add one case per vendor word you
  translate. For a vendor that reports errors with HTTP
  status 200 and an error field, add a case for it and set `error_path`.
- `"error": "no_data"` means a successful call with nothing to map.
  `"error": "http_402"` means an HTTP error status.
- `body` is `null` for a `GET`.
- The fixture lead is fixed per manifest. A parameter listed in `omit_empty`
  is tested on the absent side by leaving that lead field out of `lead`; the
  engine tests in `internal/manifest/extensions_test.go` cover the present
  side.

Run it from `apps/backend-go`:

```bash
go test ./internal/manifest/... ./internal/vendorcatalog/... \
  ./internal/providerrun/... ./internal/credentials/... \
  ./internal/wbregistry/... ./internal/settings/...
```

## Security rules

- **HTTPS only.** `request.url` must start with `https://`. The connector
  validator and a test reject anything else.
- **No secrets in the file.** Never write a key, token or password in a
  manifest, in a comment, or in a fixture other than the fake
  `test-key-123`. The key comes from `auth.env_var`. A literal `auth.value`
  is refused at run time (`literal_auth_value`).
- **Environment allow-list.** A manifest can read only the names in
  `auth.env_var` and `auth.extra_env`. Platform secrets such as
  `DATABASE_URL`, `SECRET_KEY`, `REDIS_URL`, `STRIPE_*` and `*_MASTER_KEY` are
  always refused. A name written as `${env:NAME}` in a template is not
  granted by that alone.
- **Lead data never reaches the secret.** `${env:...}` is expanded only in
  manifest text. A lead field that contains `${env:X}` is sent as plain text.
- **No templated host.** When a manifest carries a credential, `{{...}}` in
  the host part of the URL is refused (`templated_authority`). Template only
  the path or query.
- **Egress guard.** Every call passes the server-side request guard, which
  blocks private and link-local addresses and checks the address again when
  it connects.
- **Query-string keys.** A key sent as a query parameter can show up in the
  vendor's logs. Prefer a header or bearer scheme when the vendor offers one.
- **Error text is scrubbed** of the credential before it is stored.
- **Signing.** The default policy (`CONNECTOR_SIGNATURE_POLICY=optional`) loads
  unsigned manifests and rejects a present but invalid signature. With
  `required`, only manifests signed by a key in the trust store load. Built-in
  manifests are unsigned and are meant for the default policy. Setting
  `OPENGTM_CONNECTOR_MANIFESTS_DIR` replaces the built-in folder with your
  own directory.

## Pull request checklist

Copy this into the PR description.

```markdown
- [ ] Vendor docs URL is in `homepage` and in a comment, and I read the page.
- [ ] Method, URL, auth and every mapped field are shown in the vendor docs.
- [ ] The call is synchronous and needs one request.
- [ ] `request.url` is https; the key is in `auth.env_var`; no secret in any file.
- [ ] `cost_per_lookup` is the published price, or 0 with a comment saying it is unknown.
- [ ] Verifiers write only valid, invalid, catch_all, risky, unknown (value_maps with `default: unknown`).
- [ ] `capability` is an existing one (email, email_verify, phone, mobile_phone, linkedin_url, company_size, ...).
- [ ] Fixture directory with a hit, an empty answer and a vendor error, copied from the docs.
- [ ] vendorcatalog, wbregistry (and the two meta fixtures), settings registry
      (and the two settings fixtures) updated; counts raised in the three count tests.
- [ ] `.env.example`, configuration page and provider table updated.
- [ ] `go vet` and `go test` pass for manifest, vendorcatalog, providerrun,
      credentials, wbregistry and settings.
- [ ] Anything I could not confirm is listed in the PR body.
```
