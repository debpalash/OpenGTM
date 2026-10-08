# Companies and People as shared records: design draft

Status: draft for discussion, not agreed. Written 2026-10-02 from issue 20 and a
review of the code on main. Move it to `docs/plans/` when it is ready to share
with the contributor.

## Goal

From issue 20. A company and a person have separate identities, records and
lifecycles. Several people can belong to one company without replacing each
other. A person keeps their identity and history across jobs. Workbooks,
audiences, watches and outreach reference the same records, and a company alone
is a valid subject.

## Where the code is today

| Area | State |
|---|---|
| Lead | One row with a company and one contact. 56 columns. Unique on workspace, company and city. |
| Entities | Company and person tables with employment history, row-level security and per-field evidence. Company attributes beyond name, domain, phone, email and city still live in lead columns. |
| Merge and undo | Companies only. No person merge, split or alias decision. |
| People saved | Only by people search and chat research, as entities and workbook rows. No person endpoints and no screens. |
| Lead ID dependents | 13 tables, over 350 code lines, 77 modules, 14 web files, the MCP tools and the n8n node. |
| Suppression | Keyed by email, so it survives a migration. Enrollment and delivery records are keyed by lead ID and need a mapping. |

## Decisions

### D1. The subject of audiences, outreach, destinations and watches

- **Options.** Person only. Company only. A typed subject: company, person, or person at a company.
- **Recommendation.** A typed subject. Person at company, meaning the person and their current employment, is the unit for outreach, CRM contact sync and contact audiences. Company is the unit for watches, account audiences and account signals. Store two nullable references with a check that at least one is set, not one polymorphic ID, so foreign keys and row-level security keep working.
- **Why.** A lead is exactly that pair today, so the mapping is the least surprising. It also makes a company-only subject valid without inventing a contact.

### D2. Accepted values and precedence

- **Options.** Keep values only as observation lists, as today. Materialize accepted values as typed columns. Use one attribute table.
- **Recommendation.** Keep observations as evidence. Materialize accepted values for the fields that grids, filters and segments use, as typed and indexable columns on the company and person tables. Keep the long tail in JSON and add expression indexes when queries need them. Precedence is manual edit, then verified provider value, then the most agreed observation. Every change records who made it and why.
- **Why.** Grids and segments need queryable values, and observation lists in JSON are not.

### D3. Identity rules

- **Companies.** Exact domain first, then fuzzy name and city with a review queue. Both exist. Add: a company without a domain is allowed but marked unverified, and the grey band is never auto-merged. Several leads for different cities can map to one company.
- **People.** LinkedIn, then legacy id, then email. An email match needs name agreement, which is now enforced. A person with only a name needs a company-scoped key.
- **New work.** Person merge, split and alias decisions, with undo that records exactly what moved, applying what the company merge fixes taught. A queryable review table for people, because notes inside JSON cannot be listed at scale.

### D4. Workbook edits: write through or local override

- **Recommendation.** A workbook column mapped to a shared field writes through by default, with manual precedence and a note of which workbook made the change. A per-column setting keeps a local value on the row instead. Enrichment results reach the shared record through the same operation, so every writer goes through one record layer.
- **Consequences.** An edit in one workbook shows in the others. Deleting a workbook row never deletes the shared record.

### D5. Tenant isolation

Every new table gets forced row-level security in the same migration revision, is
covered by the readiness check, and is tested under the non-superuser role, as
the existing PostgreSQL tests do.

### D6. Erasure and suppression

Add person-level erasure across entities, workbook rows, audiences, outreach and
delivery history. Suppression stays keyed by normalized email. Map enrollment and
delivery idempotency keys from lead ID to person and company before outreach
switches, so nobody is emailed twice or after an unsubscribe.

### D7. Compatibility window

The lead ID stays as an alias. REST lead endpoints and MCP lead tools keep
working for at least two minor releases, with deprecation notices in responses
and docs. Company and person endpoints and tools are added beside them.

## Migration plan

Each phase ships alone, behind a flag, and can be reversed. No column is dropped
in the release that switches reads.

| Phase | Work | Exit criteria |
|---|---|---|
| 0 | Data-loss and entity fixes in a patch release | Released |
| 1 | Expand: nullable company and person links on leads, a backfill job, read-only Companies and People screens | Backfill is idempotent, resumable and tenant-scoped. A dry run reports leads, new companies, ambiguous matches and new people. Ambiguous matches go to review and are never auto-merged. |
| 2 | One record layer and dual write | REST, import, collection, connectors, enrichment writeback, inbound callbacks and MCP all write through it, and the lead and entity change in one transaction. |
| 3 | Shadow read | Old and new reads are compared in logs for an agreed period with no unexplained mismatch. |
| 4 | Consumers by risk: watches, workbook writeback, audiences, destinations, outreach last | For each, add references, write both IDs, switch reads behind a flag, and remove the lead dependency a release later. |
| 5 | Editable Companies and People screens and independent enrichment workflows | |
| 6 | Contract: deprecate lead-ID endpoints and tools, then drop old columns a release later | |

Rollback is a flag per consumer. The lead tables stay authoritative for a
consumer until its phase 4 step has soaked.

Order inside phase 4 matters. Watches come first because they match leads by
fuzzy company name today. Outreach comes last because a mistake there sends
duplicate or unwanted email.

## Test plan

- Backfill: idempotent, resumable, tenant-scoped, and correct for leads with no domain and for branches of one company.
- Dual write: a randomized comparison that lead and entity agree after many writes.
- A PostgreSQL job under row-level security in CI, which needs the CI draft.
- Concurrency tests, like the contact race test, for every consumer that serializes writers.
- An upgrade test from a database created by 3.0.0.

## Not in the first milestone

- Delivering company-only watch events to the signal feed without a lead. Today they are held. Decide after phase 4.
- Sharing records across workspaces.

## Questions for the contributor

1. How big is your pipeline: companies, people per company, import formats?
2. Which company and person enrichment workflows do you need first?
3. Are your audiences of people, companies or both?
4. Would you change anything in D1 to D7?

## Decision log

| Date | Decision | Agreed by |
|---|---|---|
