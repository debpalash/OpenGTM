# OpenGTM Connector SDK

Add an enrichment provider without writing Python. A connector is one YAML
manifest conforming to manifest version `1`; OpenGTM compiles it into the same
provider interface used by workbook waterfalls.

## Build a connector

1. Copy [`connector.template.yaml`](connector.template.yaml) into a capability
   folder under `apps/api/services/leadgen/enrichment/declarative/manifests/`.
2. Give it a globally unique, stable `name`. Never put credentials in YAML;
   reference a workspace secret with `auth.env_var`.
3. Map provider response paths to OpenGTM fields in `response.mappings`.
4. Validate locally with `uv run python -m apps.api.cli connectors path/to/manifests`.
5. Add a deterministic mocked-response test and open a pull request.

The compatibility command rejects duplicate IDs, non-HTTPS endpoints, unknown
auth modes, missing credential references, invalid schema versions, unsupported
HTTP methods, and empty response mappings. Use `--json` for CI output.

At runtime, `GET /api/connectors/catalog` lists connectors and whether their
required workspace credential is configured. Secret names and values are never
returned. `GET /api/connectors/compatibility` exposes the same validation report
used in CI.

## Sign and distribute a connector

Connector packages use a detached `<manifest>.sig` envelope containing an
Ed25519 signature and SHA-256 digest of canonicalized YAML. Generate and guard
an Ed25519 PEM private key outside the repository, publish only its raw public
key in `trusted-publishers.json`, then run:

```bash
uv run python -m apps.api.cli connector-sign path/provider.yaml \
  --private-key /secure/publisher-ed25519.pem --key-id publisher-2026
uv run python -m apps.api.cli connectors path --signature-policy required \
  --trust-store docs/connectors/trusted-publishers.json
```

The default `optional` policy keeps local unsigned connectors usable, while
still rejecting a present signature that is invalid or from an untrusted key.
Managed catalogs should set `CONNECTOR_SIGNATURE_POLICY=required`; then unsigned
packages are neither loaded nor accepted by review automation. Private keys are
never stored in a manifest, signature envelope, or trust store.

## Security contract

- Remote endpoints must use HTTPS.
- Lead-controlled URL templates still pass through OpenGTM's DNS-resolving SSRF
  guard at execution time.
- Credentials resolve from encrypted workspace secrets and are injected only
  for the outbound request.
- Connector responses are projected onto explicitly declared fields; raw vendor
  payloads are not persisted by the manifest runtime.
- A connector is beta until its mocked contract test and controlled-live vendor
  check both pass.

See [`connector-manifest.schema.json`](connector-manifest.schema.json) for the
machine-readable contract.
