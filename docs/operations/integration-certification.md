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
Paid-media destinations additionally require consent enforcement, identifier
hashing, add and remove reconciliation, and partial-failure accounting.
Streaming warehouse activation additionally requires streaming-upload,
manifest-checksum, and bounded-memory evidence.
CRM certificates require conflict-policy evidence; sequencers require campaign
enrollment; Google Sheets requires idempotent upsert; Airtable requires atomic
upsert; and Slack requires notification-delivery evidence. These checks keep a
generic successful HTTP request from overstating the supported workflow.
Signal sources, agents, and community connectors have corresponding read,
provenance/grounding, normalization, budget, recovery, and isolation gates.
Signal certification also requires source-specific proof: JobSpy employment
normalization, SEC CIK resolution and filing cursors, website content-change
detection, technology fingerprinting, and news source attribution.
Agent subjects require capability-specific proof: citations and provenance for
grounded research; context propagation and prompt versioning for chains;
bounded traversal and durable results for audience runs; cancellation, in-place
retry, and completed-work preservation for recovery; and single-flight restart
recovery for schedules.
OIDC SSO and SCIM directory subjects additionally require operation-specific
identity binding, access enforcement, provisioning/lifecycle, revocation,
pagination, and tenant-isolation evidence. Their maturity is available from
`GET /api/governance/capabilities`.
The attestation command refuses incomplete evidence before signing:

```bash
uv run python scripts/attest_integration_certification.py \
  --input artifacts/hubspot-certification.json \
  --evidence artifacts/hubspot-controlled-live-evidence.json \
  --output artifacts/hubspot-certification.attested.json
```

The signer streams and hashes `--evidence` and refuses to issue a certificate
unless those exact bytes match `evidence_sha256`. Upload the same immutable
artifact bytes at `evidence_url`; do not regenerate or reformat them after
attestation. Every passed check's `evidence` value must contain an RFC 6901
JSON pointer (for example `artifact.json#/authentication`) that resolves in
that artifact; missing or malformed references also prevent issuance.

Deploy a JSON array of attested certificates and configure:

```text
OPENGTM_INTEGRATION_CERTIFICATIONS=/run/opengtm/integration-certifications.json
OPENGTM_INTEGRATION_CERTIFICATION_KEY=<secret-manager reference>
OPENGTM_BUILD_SHA=<exact immutable deployed revision>
```

The running build identity must exactly match the signed certificate's
`build_sha`. Missing or mismatched build identity fails closed, preventing live
evidence collected against an older binary from certifying a newer deployment.

`GET /api/audience-destinations/types`, `GET /api/signals/sources`, and
`GET /api/research-playbooks/capabilities` report the effective maturity and
non-secret certification metadata. Missing files, malformed JSON, missing
keys, expired records, unknown integrations, metadata-only claims, missing or
failed required checks, invalid evidence digests, HTTP evidence links, wrong
keys, missing or mismatched build identity, and post-signing edits all fail
closed to `beta`. Rotate the key to revoke all current certifications
immediately.

Subject identity must also be unambiguous. If both `subject_id` and the legacy
`integration_id` are present, they must match. A certification file containing
more than one currently valid certificate for the same subject demotes that
subject to `beta` rather than choosing a record based on file order.
Issuance also fails closed for malformed validation-run IDs, blank or
whitespace-containing build identities, invalid or inverted timestamps,
credential-bearing evidence URLs, and validity windows longer than 92 days.
Revalidate against the deployed build instead of issuing permanent support
claims.

Installed declarative connectors use `subject_id: connector:<manifest-name>`.
They must also include `subject_build_sha256`, the canonical installed manifest
digest exposed as `package_sha256` by `GET /api/connectors/catalog`.
Their Ed25519 publisher signature and controlled-live HMAC certification are
independent gates: the former proves provenance, while the latter proves the
specific package has current operational evidence. Any manifest replacement
changes the digest and immediately demotes the connector to `beta` until the
new package is validated and attested.
