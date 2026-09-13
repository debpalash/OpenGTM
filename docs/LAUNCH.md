# OpenGTM launch kit

This is the copy-and-asset pack for a coordinated public rollout. Keep every
claim grounded in the current README and product; update screenshots by running
`uv run python scripts/capture_launch_assets.py` against a seeded local stack.

## Positioning

**Category:** open-source, self-hostable Clay alternative.

**One line:** Source, enrich, research, and route leads on your infrastructure —
with your own keys and the bill shown before every run.

**Launch wedge:** cost transparency and control. Avoid leading with a giant
feature-parity checklist; show one complete workflow and its spend estimate.

## Show HN

**Title**

> Show HN: OpenGTM – an open-source, self-hosted Clay alternative

**Post**

> I built OpenGTM because enrichment tools make it too easy to spend money
> without understanding the waterfall behind each cell.
>
> OpenGTM is a spreadsheet-shaped GTM engine you can self-host. It sources
> companies, runs cost-ordered enrichment waterfalls, researches hard cells
> with bounded agents, and pushes results to CRMs, Sheets, or webhooks. Before a
> run, it shows the worst-case bill and lets you set a hard spend ceiling.
>
> It is AGPLv3, BYOK, and the REST API/webhooks/MCP tools are not plan-gated.
> `docker compose up` starts the full stack and seeds a populated demo workbook
> that uses zero-key providers, so you can see it work before adding API keys.
>
> Repo: https://github.com/debpalash/opengtm
>
> I would especially value feedback on the provider waterfall UX, the
> self-hosting path, and which integrations should be next.

## Reddit

### r/selfhosted

**Title:** I built a self-hosted, open-source alternative to Clay – full stack in Docker Compose

Lead with: Postgres + worker + Redis included, data and keys stay on the user's
machine, zero-key demo on first boot, honest limitations in the README.

### GTM and sales communities

**Title:** I built an enrichment workbook that shows the bill before it runs

Lead with: cost-ordered waterfalls, hard spend ceiling, BYOK, provenance per
cell, CRM/Sheets/webhook outputs. Do not assume the audience knows Clay.

## Product Hunt

**Tagline:** Open-source GTM agents, on your infrastructure

**Description:** Build lead lists, enrich them through provider waterfalls,
research hard-to-find facts with AI, and route the results to your stack.
OpenGTM is self-hostable, BYOK, and shows the estimated bill before a run.

**Gallery order:**

1. Social preview: the category and core promise.
2. Workbook GIF: the complete product loop.
3. Cost-control screenshot: the differentiated wedge.
4. Login/self-host screenshot: ownership and trust.

## Assets

- `docs/assets/opengtm-social-preview.png` – 1280×640 launch card.
- `docs/assets/opengtm-demo.gif` – short GitHub-friendly real-product tour.
- `docs/assets/opengtm-demo-light.gif` – light-mode version of the product tour.
- `docs/assets/opengtm-workbook.png` – seeded workbook.
- `docs/assets/opengtm-cost-control.png` – spend controls.
- `docs/assets/opengtm-source-engine.png` – sourcing and living-workbook panel.
- `docs/assets/opengtm-login.png` – self-hosted login.

GitHub repository social previews are configured in **Settings → General →
Social preview**; upload `opengtm-social-preview.png` there.

## Rollout sequence

1. Verify a clean-machine `docker compose up` and time first useful screen.
2. Set the GitHub description to: `Open-source, self-hosted Clay alternative — source, enrich, research, and route leads with your own keys.`
3. Add GitHub topics: `clay-alternative`, `lead-enrichment`, `sales-automation`,
   `gtm`, `self-hosted`, `ai-agents`, `fastapi`, `react`, and `mcp`.
4. Upload `docs/assets/opengtm-social-preview.png` as the GitHub social image.
5. Publish a tagged release with accurate release notes.
6. Post Show HN first; stay available to answer technical questions.
7. Adapt, rather than duplicate, the post for r/selfhosted and GTM communities.
8. Launch on Product Hunt after the first wave yields quotes and objections.
9. Turn repeated questions into docs and link back to the exact answer.

## Launch-day checks

- [ ] Fresh clone reaches the demo without undocumented steps.
- [ ] `admin` / `admin` warning is prominent and correct.
- [ ] GIF and screenshots match the tagged release.
- [ ] No real customer, prospect, or credential data appears in media.
- [ ] README links and issue templates work.
- [ ] Known limitations are visible.
- [ ] Someone is watching issues and discussions for the first 24 hours.
