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
- leave out a parameter when the lead has no value for it (the parameter is
  sent as an empty string);
- translate a value (a vendor word such as `deliverable` is stored as it is);
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
| `extra_env` | | More credential names the manifest may use as `${env:NAME}`. Avoid; see the limits above. |

How the key is placed:

- `bearer`: header `Authorization: Bearer <key>`.
- `header`: header `<param>: <key>`.
- `query`: query parameter `<param>=<key>`.

If the credential is missing, no request is made and the result is
`<name>: missing <ENV_VAR>`.

### `request`

| Field | Default | Meaning |
|---|---|---|
| `method` | `POST` | `GET`, `POST`, `PUT` or `PATCH`. |
| `url` | required | Must start with `https://`. May contain `{{input.x}}` after the host. |
| `headers` | | Map of header name to template. |
| `query` | | Map of parameter name to template. Added to any query already in `url`. |
| `body` (or `body_template`) | | JSON body. Strings anywhere inside are templates. Sent with `Content-Type: application/json` unless you set that header. |
| `timeout` | `20` | Seconds. Must be above 0 and at most 120. |

The response body is read up to 1 MiB and must be JSON.

### `response`

| Field | Meaning |
|---|---|
| `mappings` | Required, not empty. Map of OpenGTM field name to a path in the response. |
| `error_path` | Optional path. If it holds a truthy value (non-empty string, `true`, non-zero), the call is a failure even though the HTTP status was 2xx. |
| `error_message_path` | Optional path of the text to report. Cut to 120 characters. |

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

### Templates

- `{{input.name}}` is replaced by a lead value. Missing values become an
  empty string. `{{input.name | default: x}}` uses `x` instead.
- Available inputs: `company`, `company_name`, `website`, `domain` (host of
  `website`, without scheme and `www.`), `email`, `linkedin_url`, `city`,
  `contact_person`, `full_name` (same as `contact_person`), `first_name`,
  `last_name`, `phone`.
- `${env:NAME}` is replaced by a credential. It works only in text you
  wrote in the manifest, never in a lead value, and only for `auth.env_var`
  and `auth.extra_env`.

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
| `timeout` | The call took longer than `request.timeout`. |

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
  one), and one vendor error. For a vendor that reports errors with HTTP
  status 200 and an error field, add a case for it and set `error_path`.
- `"error": "no_data"` means a successful call with nothing to map.
  `"error": "http_402"` means an HTTP error status.
- `body` is `null` for a `GET`.

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
- [ ] `capability` is an existing one (email, email_verify, phone, mobile_phone, linkedin_url, company_size, ...).
- [ ] Fixture directory with a hit, an empty answer and a vendor error, copied from the docs.
- [ ] vendorcatalog, wbregistry (and the two meta fixtures), settings registry
      (and the two settings fixtures) updated; counts raised in the three count tests.
- [ ] `.env.example`, configuration page and provider table updated.
- [ ] `go vet` and `go test` pass for manifest, vendorcatalog, providerrun,
      credentials, wbregistry and settings.
- [ ] Anything I could not confirm is listed in the PR body.
```
