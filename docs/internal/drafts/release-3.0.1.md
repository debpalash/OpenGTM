# Release 3.0.1 (draft, nothing tagged)

A patch release for the data-loss and entity fixes. It has no database migration.

## Checklist

1. Commit the fixes with sign-off and merge them to main.
2. Change 3.0.0 to 3.0.1 in `pyproject.toml`, in `apps/api/main.py` (three places), in `apps/mcp/server.py`, and in the image tag in `README.md` and `apps/docs/src/content/docs/getting-started/quickstart.md`.
3. Relock, because `uv.lock` records the project version.
4. Regenerate the API reference, whose version follows `apps/api/main.py`.
5. Replace the Unreleased heading in `CHANGELOG.md` as shown below.
6. Run the release preflight.
7. Tag and push the tag to the public remote. The release workflow builds the image and creates a draft release.
8. Paste the notes below into the draft release, review it, and publish.
9. Run the manual docs deploy so the quickstart shows 3.0.1.

```bash
uv lock
uv run python scripts/export_openapi.py
scripts/release/preflight.sh
git tag v3.0.1 && git push opengtm v3.0.1
```

## CHANGELOG.md

Put a fresh empty `## [Unreleased]` above this heading and keep the existing
Changed and Fixed entries beneath it.

```markdown
## [3.0.1] - 2026-MM-DD
```

## Release notes

```markdown
Container image: `ghcr.io/debpalash/opengtm:3.0.1`. No database migration.

### Fixed

- **Leads are no longer overwritten.** Creating a lead for an existing company and city keeps what is stored. Fields you leave out, including status, score and notes, are preserved, so a collection re-run or a partial `POST /api/lead` cannot blank enrichment.
- **One contact per lead is enforced.** A request that names a different contact is rejected with 409 and changes nothing, and the MCP `create_lead` tool returns the same error. Bulk paths keep the stored contact.
- **Watches no longer lose events.** A watch whose target matches no lead holds its cursor and reports `no_matching_lead`, then delivers the events once a lead matches.
- **Merging a company with linked people works.** People and their employment history move to the kept company.
- **Undoing a company merge is exact.** It removes only what the merge added, keeps evidence collected since, and moves back only the links the merge moved.
- **A shared email no longer joins different people.** A profile with a different name becomes its own person and both records are flagged for review. A changed profile slug with a matching name is still the same person.

### Upgrade notes

- If an integration, such as an n8n flow, adds a second contact for the same company and city, it now receives 409. Use `PUT /api/lead/{id}` to replace the contact, or keep several people in a workbook.
- Upserts no longer clear fields. To clear a field, send an explicit update with `PUT /api/lead/{id}`.
- Watches deliver only to a lead. Pin one with `lead_id`, which feed watches need in practice. Without a match the watch now says so in its last error instead of dropping events.

Thanks to @tartakovsky for the reproductions and root causes.
```
