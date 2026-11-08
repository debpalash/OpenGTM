# Closed-source blockers

This audit lists what would stop Yupcha from shipping OpenGTM, or any part of
it, under a closed or commercial licence. It was taken on 2026-11-08, against
`main` at `36648dcc`. This is an engineering audit, not legal advice: have a
lawyer confirm it before selling a closed edition.

To re-run the contributor part:
`GH_TOKEN=... python3 scripts/cla_audit.py` (after `git fetch origin cla-signatures`).

## 1. Contributor code without a CLA

Contributors who never signed the CLA licensed their code under the AGPL only.
For each one, either get a signature on #134 or remove or rewrite their code.

| Contributor | Lines at HEAD | Where | Action |
|---|---:|---|---|
| @rudycelekli | 163 | `apps/web` (workbook clipboard, tests), `docs/adr` | Ask to sign #134, or rewrite |
| @bipul0525 | 182 | `reference/legacy/enrich*.py` (old reference code) | Delete `reference/legacy/`, or ask to sign |
| @musabgultekin | 1 | `.env.example` | Ask to sign, or rewrite the line |
| @googio (serply) | 1 | `.env.example` | Ask to sign, or rewrite the line |

Maintainer identities that the CLA check does not exempt yet. These need confirming as Yupcha-owned work:

| Identity | Lines at HEAD | Action |
|---|---:|---|
| @velixio (Shimul Debnath, account ID 270455167) | 1,558,753 | Add to `MAINTAINER_IDS` in `.github/scripts/cla_check.py`, or sign #134 |
| `user@users-MacBook-Air.local` (2 commits, 2026-07-18) | 1,923 | Confirm who wrote `gtm/*.md` and the `ambitionbox` fix |

The audit reviewed two maintainer commits that fold in other work, `fc0a93e3`
and `a348bbd6`. Both are maintainer work with an AI co-author, so neither is a
blocker.

## 2. Copyleft dependencies

| Component | Licence | Where | Why it matters | Options |
|---|---|---|---|---|
| `holehe` | GPL-3.0 | `apps/browser-service` (`scrapers/holehe_check.py`), imported in process | A closed browser service that imports it would breach the GPL | Replace or remove it, run it as a separate open process, or keep the browser service AGPL |
| `reacherhq/backend` | AGPL-3.0 (commercial licence sold by Reacher) | `docker-compose.yml` email-verification service | OpenGTM only calls it over HTTP, so OpenGTM's own code is unaffected. Shipping or hosting the image still carries AGPL duties for Reacher itself | Keep offering Reacher's source, buy Reacher's commercial licence, or make it optional |

## 3. Checked and clear

- **Go backend:** all 25 third-party modules are MIT, BSD, Apache-2.0 or ISC.
- **Web app** (`apps/web`, 635 npm packages): the only licences outside MIT, BSD, Apache and ISC are:
  - OFL-1.1 for the fonts;
  - MPL-2.0 for `lightningcss` (a build tool) and `axe-core` (a test tool).

  OFL fonts and MPL code that isn't modified are fine in a closed product.
- **Browser service, other packages:** permissive. `staffspy` is WTFPL.
- **`scripts/vendor/twenty-ui`:** MIT. Keep its copyright notice.
- **Released versions:** anything already published under the AGPL stays AGPL for its recipients. That doesn't block future closed versions.
