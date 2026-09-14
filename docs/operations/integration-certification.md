# Integration certification and support maturity

OpenGTM exposes activation integrations as `beta` unless a current,
controlled-live certification is present and correctly attested. This keeps
code availability separate from a support claim.

A certificate JSON object requires `subject_id` (legacy destination records may
use `integration_id`), `status: "supported"`, `mode: "controlled_live"`,
`validated_at`, `expires_at`, `build_sha`, `validation_run_id`, an HTTPS
`evidence_url`, the artifact's `evidence_sha256`, and a SHA-256
`external_system_id_hash` (never the raw account identifier). It also requires
subject-specific `checks`, each shaped as
`{"passed": true, "evidence": "artifact.json#/check"}`. Destinations require
authentication, external write, idempotency, retry recovery, and tenant
isolation; bidirectional CRMs additionally require inbound reconciliation.
Signal sources, agents, and community connectors have corresponding read,
provenance/grounding, normalization, budget, recovery, and isolation gates.
The attestation command refuses incomplete evidence before signing:

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

`GET /api/audience-destinations/types`, `GET /api/signals/sources`, and
`GET /api/research-playbooks/capabilities` report the effective maturity and
non-secret certification metadata. Missing files, malformed JSON, missing
keys, expired records, unknown integrations, metadata-only claims, missing or
failed required checks, invalid evidence digests, HTTP evidence links, wrong
keys, and post-signing edits all fail closed to `beta`. Rotate the key to revoke all
current certifications immediately.

Installed declarative connectors use `subject_id: connector:<manifest-name>`.
Their Ed25519 publisher signature and controlled-live HMAC certification are
independent gates: the former proves provenance, while the latter proves the
specific package has current operational evidence.
