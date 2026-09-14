# Controlled-live gauntlet attestations

Release eligibility requires ten consecutive controlled-live G1–G7 passes.
Each history record must be signed with the release environment's HMAC key;
unsigned records, records signed by another key, and records edited after
signing fail closed and do not contribute to the streak.
The history must be strictly chronological and every counted record must match
the newest record's exact immutable `build_sha`; reordered records, duplicate
timestamps, malformed timestamps, and mixed-build histories terminate the
candidate streak.

Keep `OPENGTM_GAUNTLET_ATTESTATION_KEY` in the release secret manager. Never
store it in an artifact, repository, CI log, or command-line argument. After a
dedicated-workspace run produces its unsigned history record, attest it:

```bash
uv run python scripts/attest_gtm_validation.py \
  --input artifacts/run-001.json \
  --output artifacts/run-001.attested.json
```

Append the attested record to `production_run_history`, ensure the artifact's
top-level `run_id` and `build_sha` match the newest record, and evaluate it:

```bash
uv run python scripts/run_gtm_gauntlet.py \
  --input artifacts/controlled-live.json \
  --output artifacts/controlled-live-report.json \
  --require-release
```

Rotating the key intentionally starts a new trusted streak. Retain previous
reports as immutable release evidence, but do not mix signatures from multiple
keys in one candidate streak.

Set `OPENGTM_GAUNTLET_ARTIFACT` to the assembled controlled-live artifact.
Platform administrators can then query `GET /admin/operations/release-readiness`
for one fail-closed view of the gauntlet, every required first-party maturity
subject, and separately reported community-connector certification coverage.
Missing or malformed artifacts are never release eligible.
The readiness endpoint also requires the gauntlet's top-level `build_sha` to
exactly match `OPENGTM_BUILD_SHA`, the same deployed-build identity used for
integration certifications. A valid streak from another build fails closed
with `build_mismatch`.
