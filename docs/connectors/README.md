# OpenGTM Connector SDK

Add an enrichment provider without writing Go. A connector is one YAML
manifest conforming to manifest version `1`; OpenGTM loads it into the same
provider interface used by workbook waterfalls.

## Build a connector

Follow the contributor guide: `apps/docs/src/content/docs/guides/provider-manifests.md`
(published as `/guides/provider-manifests/` on the docs site). It has the field
reference, a worked example, the fixture-based test recipe, the security rules
and the pull request checklist.

In short:

1. Copy [`connector.template.yaml`](connector.template.yaml) to
   `apps/backend-go/internal/manifest/builtin/manifests/<capability>/<name>.yaml`.
2. Fill it from the vendor's official API documentation only.
3. Add a fixture directory under
   `apps/backend-go/internal/manifest/builtin/testdata/vendors/<name>/`.
4. Add the vendor catalog, workbook registry and settings registry entries.
   The tests name any entry that is missing.

At runtime, `GET /api/connectors/catalog` lists connectors and whether their
required workspace credential is configured. Secret names and values are never
returned. `GET /api/connectors/compatibility` returns the validation report
used in CI.

## Signatures and trust

Connector files may carry a detached `<manifest>.sig` envelope containing an
Ed25519 signature and SHA-256 digest of the canonicalized YAML. The Go
binaries verify signatures against `trusted-publishers.json`; this repository
does not contain a signing or packaging command. Private keys are never stored
in a manifest, a signature envelope or the trust store.

The default policy, `CONNECTOR_SIGNATURE_POLICY=optional`, loads unsigned
manifests and rejects a present signature that is invalid or from an untrusted
key. Managed catalogs should set `CONNECTOR_SIGNATURE_POLICY=required`; then
unsigned manifests are not loaded. `OPENGTM_CONNECTOR_MANIFESTS_DIR` and
`OPENGTM_CONNECTOR_TRUST_STORE` replace the built-in manifests and trust store.

Catalog entries stay `beta` until a controlled-live certificate with
`subject_id: connector:<manifest-name>` is signed and deployed through the
integration certification process.

## Security contract

- Remote endpoints must use HTTPS. The connector validator rejects anything else.
- URL templates pass through the SSRF guard at execution time. A manifest that
  carries a credential may not template the host.
- Credentials resolve from workspace secrets, then Settings, then the
  environment, and only for names in `auth.env_var` and `auth.extra_env`.
  They are injected only into the outbound request and scrubbed from error text.
- Responses are projected onto the fields declared in `response.mappings`; the
  raw vendor payload is not stored by the manifest runtime.

See [`connector-manifest.schema.json`](connector-manifest.schema.json) for a
machine-readable outline of the format. The Go loader in
`apps/backend-go/internal/manifest` is the authority where the two differ.
