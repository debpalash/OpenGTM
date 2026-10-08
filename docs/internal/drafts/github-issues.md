# GitHub issue drafts (not created)

Five issues. Create the CI and tracking issues first so the others can link to
them. Labels use the repo's existing set. Replace `#NN` once numbers exist.

---

## 1. Companies and People as shared records: plan and working agreement

**Labels:** enhancement

Follows up issue 20. The design draft is linked below. Nothing in it is agreed
until it appears in the decision log.

**Plan**

- [ ] Phase 0: data-loss and entity fixes, released as 3.0.1
- [ ] CI runs tests on pull requests (#NN)
- [ ] Person merge, split and alias decisions (#NN)
- [ ] Design agreed: subject model, field ownership, identity rules, compatibility window
- [ ] Phase 1: link columns, backfill, read-only Companies and People screens (#NN)
- [ ] Phase 2: one record layer and dual write
- [ ] Phase 3: shadow read
- [ ] Phase 4: consumers by risk, watches first and outreach last
- [ ] Phase 5: editable screens and independent enrichment workflows
- [ ] Phase 6: deprecate lead-ID contracts after the compatibility window

**Working agreement**

- Design before schema. No migration lands before the design is agreed.
- Decisions are recorded in the design document's decision log.
- The maintainer owns releases, migrations and tenancy, as in CODEOWNERS. The contributor reviews and proposes on the entity and people paths.
- Every phase ships alone, behind a flag, and can be reversed.
- Commits are signed off under the DCO, and contributions are AGPL-3.0, as in CONTRIBUTING.

Bugs 15 to 19 were reported by @tartakovsky.

---

## 2. Run backend tests on every pull request

**Labels:** enhancement, help wanted

No workflow runs the tests on pull requests. The docs workflow is manual and
only builds the docs site. The release workflow runs on tags and builds the
image. Pull request #14 has no checks, and Dependabot pull requests cannot be
verified. A change as large as the Companies and People model should not rely on
someone remembering to run the suite.

**Proposal.** Add a workflow with two jobs, drafted in `ci.yml`:

- SQLite: the default suite, plus a check that the generated API reference is current.
- PostgreSQL 16, the version in the compose file: the full suite with `TEST_DATABASE_URL` set, which also runs the row-level-security tests the default run skips.

Both jobs use the locked dependencies. Each takes about a minute locally.

**Acceptance**

- [ ] The workflow runs on pull requests and on pushes to main.
- [ ] A failing test or a stale API reference fails the check.
- [ ] The PostgreSQL job runs the gated tests, not skips them.
- [ ] The jobs are marked required for main once they have been stable for a week.

**Later**

- A frontend job running lint and the production build, as the pull request template asks.
- A scheduled run against the latest PostgreSQL major.

---

## 3. Person merge, split and alias decisions

**Labels:** enhancement

Resolving a person on a shared or reused email now creates a separate person and
records the shared email under `identity_conflicts` on both. Nothing can resolve
that yet. Only companies have merge and undo.

**Proposal**

- Merge two people in one workspace: move identifiers, employment history, workbook row references and sources to the kept person, and drop the conflict notes between them.
- Where both people have an employment at the same company, combine the rows: earliest first seen, latest last seen, union of titles.
- Record exactly what moved, so undo restores only that and leaves anything collected afterwards. This is the lesson of the company merge fixes.
- Split restores the merged person from the snapshot.
- Accepting an alias, for example the same person under a new profile with a different name, is a merge with a reason.
- Expose `GET /api/entities/person` and `GET /api/entities/person/{id}` using the existing profile, plus merge and split under the same prefix as the company endpoints.
- Make conflicts listable. Notes inside JSON cannot be queried at scale, so add a review table and keep the notes as evidence.

**Acceptance**

- [ ] Merge and undo are tested under foreign keys and under forced row-level security.
- [ ] Merging never crosses workspaces.
- [ ] Current and historical employment follow a merge, and undo moves back only what moved.
- [ ] A merge resolves the matching conflict notes and an undo restores them.

---

## 4. Read-only Companies and People screens

**Labels:** enhancement, help wanted

People search and chat research already save people as entities, and the source
engine saves companies. No screen shows them. Read-only screens let us look at
real data before any consumer changes, and are a safe first deliverable.

**Scope**

- Navigation items and routes for Companies and People next to Leads, Workbooks and Audiences.
- Companies list: name, domain, city, sources, corroboration, number of people, last seen. Search, sort and keyset pagination.
- Company detail: per-field evidence and linked people.
- People list: name, current company and title, LinkedIn, email status, sources, and a flag when there are identity conflicts.
- Person detail: employment history, identifiers and conflicts.

**API work.** The company list takes only a limit today. Add search, sort and a
cursor, keeping the current parameters working. Add the person list and detail
endpoints.

**Out of scope.** Editing, enrichment and deleting.

**Acceptance**

- [ ] Every query is scoped to the workspace.
- [ ] Lists stay responsive at one hundred thousand rows.
- [ ] Empty, loading and error states are handled.
- [ ] The lint and production build pass.

---

## 5. Follow-ups to the lead and watch fixes

**Labels:** enhancement

Small items found while fixing issues 15 to 19.

- The watches page shows the raw text `Last poll error: no_matching_lead`. Explain it and link to pinning a lead or adding one.
- Show `identity_conflicts` on the person detail once the screens exist.
- `POST /api/lead` without a source resets an existing lead's source to `manual`, because the request default counts as a provided value. Decide whether source applies only when sent, or whether the first source is kept.
- A nickname that does not match the full name, such as Bob and Robert, is split into two people when the email is shared and the profile is new. Both are flagged. An alias decision from the person merge work resolves it.
- Feed watches need a pinned lead. Before the fix they dropped events silently, and now they report that no lead matches. Decide whether feed and company watches should be able to deliver signals without a lead.
