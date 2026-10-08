# Drafts

Drafted 2026-10-02 after the review of the Companies and People proposal
(issue 20). Nothing here has been published, committed or sent. This folder is
maintainer-only, because `docs/internal/` is excluded from the public snapshot.
Move a file out when it is ready to share.

| File | What it is | Where it goes |
|---|---|---|
| `reply-issue-20.md` | Reply to the contributor | A comment on issue 20, sent by you |
| `company-people-model-design.md` | Decisions, migration phases and open questions | `docs/plans/`, then linked from the tracking issue |
| `github-issues.md` | Five issues: tracking, CI, person merge, read-only screens, follow-ups | GitHub, created by you |
| `ci.yml` | Backend tests on pull requests, SQLite and PostgreSQL 16 | `.github/workflows/ci.yml` |
| `release-3.0.1.md` | Checklist, changelog edit and release notes for the patch release | `CHANGELOG.md` and the draft GitHub release |

## Suggested order

1. Create the CI and tracking issues, then the other three.
2. Send the reply with the issue links filled in.
3. Open the CI workflow as its own pull request, so later pull requests get checks.
4. Commit the fixes with sign-off, one commit per fix, and open the pull requests.
5. Release 3.0.1.

## Decisions that are yours

- **Licensing.** Sign-off alone means contributions stay AGPL. If you might want a commercial exception for OpenGTM later, as you plan for OmniVoice, decide on a CLA before taking a large contribution.
- **Contributor access.** Add CODEOWNERS lines for the entity and people paths only after the contributor agrees to the working agreement. Hold off on required reviews until you have worked together.
- **Discussions.** The issue template links to Discussions, which is switched off, so that link is dead today. Turning it on fixes it and gives design threads a home.
- **Stop point.** Pick a date by which phases 1 and 2 must be done, or the work pauses.

## Draft CODEOWNERS lines

For `.github/CODEOWNERS`, added only after the contributor agrees to the working
agreement. The last matching pattern wins, so this puts the contributor beside
you on the entity and people code and leaves `migrations/` and tenancy to you.

```text
apps/api/services/entities/         @debpalash @tartakovsky
```
