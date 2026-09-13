# Integration certification and support maturity

OpenGTM exposes activation integrations as `beta` unless a current,
controlled-live certification is present and correctly attested. This keeps
code availability separate from a support claim.

A certificate JSON object requires `integration_id`, `status: "supported"`,
`validated_at`, `expires_at`, `build_sha`, `validation_run_id`, and an HTTPS
`evidence_url`. Sign it using a secret held by the release environment:

```bash
uv run python scripts/attest_integration_certification.py \
  --input artifacts/hubspot-certification.json \
  --output artifacts/hubspot-certification.attested.json
```

Deploy a JSON array of attested certificates and configure:

```text
OPENGTM_INTEGRATION_CERTIFICATIONS=/run/opengtm/integration-certifications.json
OPENGTM_INTEGRATION_CERTIFICATION_KEY=<secret-manager reference>
```

`GET /api/audience-destinations/types` reports the effective maturity and
non-secret certification metadata. Missing files, malformed JSON, missing
keys, expired records, unknown integrations, HTTP evidence links, wrong keys,
and post-signing edits all fail closed to `beta`. Rotate the key to revoke all
current certifications immediately.
