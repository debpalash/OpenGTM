# GTM GOAL — Yupcha portfolio

> **Written:** 2026-07-16 · **Owner:** Palash Debnath (solo) · **Status:** live
> Every number here was verified against a primary source on 2026-07-16. Claims
> that could not be verified are marked. Re-verify before acting on anything
> older than a month — see [Freshness](#freshness).
>
> **Owner update — 2026-08-28:** the Clay alternative is now branded OpenGTM,
> but the repository intentionally remains private while release gates and UX
> are polished. Do not change repository visibility as part of routine launch
> preparation; publication requires a separate owner decision.

---

## 0. The one-line goal

**Convert existing attention into revenue and existing capability into
distribution — before manufacturing any new attention.**

Not "get more traffic." Not "reach more customers." Those are the answers to a
problem we do not have.

---

## 1. The finding that reorders everything

The brief that started this work assumed we need agents to go out and sell. The
evidence says otherwise. Here is the portfolio, measured:

| Product | Wedge | Distribution | Monetization |
|---|---|---|---|
| **OmniVoice-Studio** | Strong | **8,509★ · 1,362 forks · 136,707 downloads** | **None — nothing to buy** |
| **lead-data** (Clay alt) | **Excellent** | **Zero — repo is private** | None |
| **ResuBird** | Decent | Modest, live | **Broken — `/pricing` 404s, PRO is ₹0** |

**The product with world-class distribution has no way to pay us. The product
with a world-class wedge cannot be seen. The product that is live and polished
has a broken checkout.**

Customers are not missing. They are already walking past three closed doors.
An outreach swarm aimed at this portfolio would be pouring water into a bucket
with no bottom — and would burn the founder's name doing it.

**Therefore: the swarm's first job is to open the doors, not to knock on
others'.**

This is not a new conclusion. `docs/council/yupcha-council-strategy-2026-06-25.md`
reached it independently three weeks ago: *"distribution is the bottleneck."*
This document agrees and makes it specific.

---

## 2. Verified ground truth

Do not re-litigate these. Do re-verify them (§Freshness).

### OmniVoice-Studio — the only real distribution asset

- **8,509 stars, 1,362 forks, 42 watchers.** Created **2026-04-09** — ~8.5k stars
  in ~14 weeks. `gh api repos/debpalash/OmniVoice-Studio`
- **136,707 downloads** — sum of `download_count` across **563 release assets**.
  Now surfaced as a live badge (PR #1168). Reproduce:
  ```bash
  gh api --paginate repos/debpalash/OmniVoice-Studio/releases \
    --jq '.[] | .assets[] | .download_count' | awk '{s+=$1} END {print s}'
  ```
- **Star velocity is decelerating**: ~100/day (early June) → **~50–60/day** now.
  Still ~1,500/month. The launch spike is over; this is the cruise phase.
- **Downloads are concentrated in three moments**: v0.2.7 (May 3) 38,867 ·
  v0.3.5 (Jun 3) 28,508 · v0.3.7 (Jun 20) 24,152 = ~91k of 137k. Across **32
  releases**, the rest fragment. We ship faster than the audience re-downloads.
- **No revenue path exists.** Free, AGPL, no tiers, hosted version rejected on
  philosophy (correctly). The commercial licence for closed-source embedding
  says pricing **"coming soon."** *8,509 stars are walking past a door that is
  not open.*
- **Zero Hacker News presence.** HN Algolia returns no stories for it. 8.5k stars
  arrived via GitHub trending + SEO + aggregators. **The single largest untapped
  channel is a Show HN that has never happened.**
- **We do not own the name.** Upstream `k2-fsa/OmniVoice` (Apache-2.0, 8,271★)
  is the TTS model; Studio launched 9 days later. **318 public repos** carry
  "omnivoice." We have out-starred the upstream model and still cannot own the
  term. Any commercial GTM built on this name is built on sand.
- **Earned press, unprompted**: MarkTechPost, kiadev.net, Digital Solution
  Centre, bymar.co. Nobody asked them to.
- Real project hygiene: Discord, Ko-fi, PayPal, Trendshift badge, signed
  macOS/Windows/Linux releases, Chinese README, an MCP server, `.claude/skills/`.

### lead-data — the wedge nobody can see

- **Private repo. 0 stars.** `visibility: PRIVATE`, remote is
  `debpalash/lead-data`.
- **The README's own quickstart 404s.** It says
  `git clone https://github.com/yupcha-internal/lead-data.git` — that returns
  **404 anonymously**. Anyone who ever followed our instructions failed on line
  one. (It resolves for us only because `gh` is authenticated and GitHub
  redirects renamed repos.)
- **The name is already spent.** `yupcha.com` is *"the agentic AI hiring platform
  … autonomous AI interviews, AI voice & phone screening, resume screening"* —
  a different, live product with its own SEO, pricing, and comparison pages.
  **The Clay alternative needs its own name and domain before it can be
  marketed at all.** This is the single most consequential blocker in the
  portfolio, and it is not a technical one.
- **The product is real.** Verified by driving it in a browser, not by reading
  code: Chat · Leads · Workbooks · Templates · Search · Tasks · Outreach ·
  Automations · Watches · Signals · Sources · Analytics. A working Clay-style
  grid, live spend counter, agentic chat front door.
- **The first-run demo was dead and is now fixed.** `seed_demo.py` enqueued the
  first-run job with no `workspace_id`; `handle_run_workbook` fails loud without
  it, so every new user got 175 cells that never filled. Now fills to ~31%+ with
  real data at **$0.000** spend. *The activation moment was one dict key from
  never working.*
- **Honest gaps, self-declared**: treat as single-tenant, shared integration
  credentials, no billing.

### ResuBird — live, polished, and not selling

- **B2C, India-first.** Job feed is Bengaluru/Hyderabad/Noida/Gurugram, salaries
  in LPA, pricing in ₹. Completely different motion from the other two.
- **Pricing and checkout work. Corrected 2026-07-16 — the earlier claim here was
  wrong.** Verified in a real browser: pricing lives at **`/#pricing`** (an
  anchor section on the homepage) and renders four live tiers — **FREE ·
  STUDENT ₹49/mo · JOB SEEKER ₹149/mo · ENTERPRISE Custom** — with "Get Started"
  wired to a real billing console at
  `console.yupcha.com/org/work?tab=plans&product=resubird` (HTTP 200).
  **This document previously claimed `/pricing` 404s "while linked from the top
  nav and the footer," and that there was no checkout. Both were false**, taken
  from a research agent and repeated without checking. **None of the homepage's
  64 links point at `/pricing`** — the nav points at `/#pricing`. The
  "PRO ₹2,499 / launch ₹0" figure was also wrong or stale.
- **The only real defect is minor:** the bare `/pricing` path is a **soft 404**
  (HTTP 200 with a "Page Not Found" body, because it's an SPA). Nothing links
  to it, so it matters only if it's indexed or guessed — a soft 404 is worse
  than a hard one for crawl budget, but this is a nit, not a blocker. Worth a
  redirect to `/#pricing`.
- **The review markup is a liability.** schema.org claims `4.8/1250` in one block
  and `4.8/12` in another, with no independent review corpus supporting either.
  Google penalizes unearned review markup, and — see RULES §6 — seeded
  testimonials plus efficacy claims is precisely the FTC *TruHeight* fact
  pattern. **Either earn the numbers or delete the markup.**
- **Blocks bot UAs (403)** — on a product whose entire channel should be organic
  search and AI answer engines.

### What Clay told us about our own wedge

Clay published an internal pricing memo. It says, in their words:

- **60–70% of value comes from orchestration, not data.**
- Their own docs claim BYOK saves users **50–80%** — *an implicit admission of
  ~2–5× markup on default routing, from the incumbent.*
- **"Pro customers [were] unprofitable for years."**
- They are mid-pivot from data broker to orchestration tax: Actions have ~zero
  marginal COGS and meter everything, including operations where Clay resells no
  data at all.

The waterfall we thought was the moat is **~25 lines**, and **Clay's provider
ordering is manual** — the optimization burden is on the user. Optimal ordering
is a bandit problem over (cost, hit-rate | segment). *That is a beatable thing,
and it is the accuracy-moat the council doc already identified.*

**The moat is procurement, not engineering.** Build orchestration; rent data.

---

## 3. The bridge (why this is one portfolio, not three)

OmniVoice's pitch: *"Your voice is the most personal data you have. So why rent
it back from a cloud?"*

lead-data's pitch: *"Own your GTM data stack. BYOK. See the bill before you
run."*

**That is the same thesis** — anti-cloud, own-your-data, no meter running. Which
means OmniVoice's 8,509 stars are made of privacy-conscious, self-hosting
developers, and **that is lead-data's ICP almost exactly.**

This is the portfolio's one structural advantage: we already have an audience of
the right people. We have never once spoken to them about the other product.

ResuBird does not share this thesis (B2C, hosted, job seekers). Treat it as a
separate business with a separate motion. Do not force the narrative.

---

## 4. Objectives, ranked

Ranked by (value ÷ effort), each with a falsifiable done-condition. **Ship in
order. Do not parallelize the top three — solo founder.**

### O1 — Open OmniVoice's revenue door · `[highest value / S]`
8,509 stars and 137k downloads with **nothing to buy**. Demand exists; there is
no transaction.
- **Done when:** the commercial-licence page has a **price**, not "coming soon,"
  and a way to pay. Dual-licence (AGPL + commercial exception), sponsorware, and
  priority support. **Never a hosted tier** — it contradicts the local-first
  promise that earned the stars.
- **Metric:** first commercial-licence enquiry; first paid exception.
- **Blocker cleared:** licence detection (PR #1168) — corporate scanners read
  `NOASSERTION` today, which deters the exact buyer this objective targets.

### O2 — The Show HN that never happened · `[high / S]`
Zero HN stories for a repo with 8.5k stars. Show HN front page ≈ **10,000–30,000
visitors/24h** (and analytics *understate* it ~1.5×, since much of HN blocks GA).
- **Done when:** posted, by the author, with something people can run in one
  command.
- **Gate:** Show HN is one shot and ungameable. It requires a genuinely runnable
  artifact. Do **not** post a waitlist or a landing page — that's off-topic and
  will be flagged.
- **Do not** solicit upvotes. See RULES §5 — that is bannable, and the trigger is
  pattern-based.

### O3 — ~~Un-404 ResuBird's checkout~~ → **fix the review markup** · `[medium / XS]`
**Downgraded from `high` on 2026-07-16 — the premise was wrong.** Pricing and
checkout work (see §2). There is no revenue blocker here; ResuBird is the one
product of the three whose door is *open*.

What remains is smaller but still worth doing:
- **Substantiate or delete the review markup.** `4.8/1250` in one schema.org
  block vs `4.8/12` in another, with no independent review corpus behind either.
  Google penalizes unearned review markup, and *FTC v. TruHeight* is exactly
  this shape — unsubstantiated efficacy claims plus seeded testimonials, $4M
  suspended to $750k. **Done when:** one number, and it's true.
- **Redirect `/pricing` → `/#pricing`.** Currently a soft 404 nothing links to.
- **Serve AI crawlers.** It 403s bot UAs, on a product whose channel is organic
  search.
- **Metric:** first ₹ collected. *This is now measurable — go measure it before
  planning anything else here.*

### O4 — Rename and publish the Clay alternative · `[high / M]`
It cannot be marketed under a name that belongs to a live hiring product, and it
cannot do OSS-native growth while private.
- **Current state (2026-08-28):** renamed and branded as OpenGTM; publication is
  deliberately deferred for a private hardening phase.
- **Done when:** new name, domain, public repo, working `git clone` in the README.
- **Gate — launch-blocking, non-negotiable:** the council doc's security floor
  (SSRF `resolve=True` on every fetch path, fail-closed `SECRET_KEY`, enforced
  per-workspace rate limiter) ships **before** the repo goes public. We are
  courting a security-conscious audience; one SSRF post nukes the control/audit
  wedge that is the entire pitch.
- **Then:** comparison SEO ("Clay alternative", "Clay pricing"), the specific
  subreddits and HN threads where data-ops people already argue about tooling.
- **Wedge copy, in Clay's own words:** see §2. We do not have to assert the
  markup — the incumbent documented it.

### O5 — Introduce the audience we already have to the product they'd want · `[medium / S]`
The bridge in §3. One honest mention, from the author, where it is genuinely
relevant. **Not** a cross-promotion campaign; not a mailing list blast to
stargazers (see RULES — that's scraping + CAN-SPAM + reputation suicide).
- **Done when:** the two products know about each other in public, tastefully.
- **Blocked by:** O4. There is nothing to point at yet.

### O6 — Fix time-to-first-enriched-row · `[medium / M]`
The demo now fills, but **12 cells errored and Stripe's row is entirely empty —
31% fill, not 100%.** The zero-key providers are lossy. First impressions are
the viral motion.
- **Done when:** first-run demo fills ≥80% of cells, or the UI honestly explains
  why it doesn't.

---

## 5. Explicit non-goals

Each of these is a thing the brief asked for, or implied, that the evidence
says not to do. **Reasons, not preferences — see RULES-OF-ENGAGEMENT.md.**

- **❌ Cold email at scale as the wedge.** Most regulated, most
  reputation-destroying surface. $53,088/email strict liability. Germany bans it
  outright even B2B. And our own compliance posture (data-broker registration in
  CA/TX/OR/VT, GDPR Art. 14 notice, the CNIL/Kaspr precedent) is a harder
  problem than the GTM.
  **Note carefully *why* we reject it.** There is **no credible measured
  evidence that AI cold email underperforms human cold email** — the vendor
  claims were never tested and the skeptics' counter-claims are *invented*
  (see RULES §8: Lavender's stat has no AI-vs-human comparison; Artisan's "95%
  churn" traces to one pseudonymous Reddit user; the Forrester and Gartner
  reports cited against it **do not exist**). We reject this on **legal
  exposure, our own compliance posture, and reputational cost** — never on
  efficacy folklore we cannot source. Rejecting a thing for a fake reason is
  how you get talked back into it by a better fake.
  **But the revealed preference is worth knowing:** ICONIQ GTM 2026 (n=149) —
  "AI for Outbound/Prospecting" fell **58%→55%, the only one of 18 GTM AI use
  cases to decline.** And across 22,988 job posts, SDR postings fell 21%
  overall **while AI-native companies more than doubled SDR headcount.** *The
  firms selling the automation are buying humans.*
- **❌ Any cold outreach for OmniVoice or ResuBird.** OmniVoice is a free local
  tool — there is nobody to cold-email. ResuBird's users are Indian job seekers:
  DPDP is consent-or-nothing, TRAI/DND makes cold SMS/calls forbidden, and they
  are a vulnerable cohort that draws regulators.
- **❌ AI voice cold outbound.** Telemarketing + artificial voice + mobile
  requires **prior express *written* consent** — a signed writing naming the
  number, before the first call. There is no cold path to that. (This binds
  *callers*. It does **not** bind OmniVoice, which places no calls — see
  RULES §0.)
- **❌ Chasing stars.** HTTPie accidentally went private, permanently lost
  **54,000 stars**, and had zero revenue impact: *"our GitHub stars turned to
  dust; HTTPie has never been doing better."* Redpoint: median 2,850 stars at
  seed, **49 of 69 seed companies had zero revenue** — *"stars are a resume,
  not a check."* We have the stars. They are not the goal; they are the asset
  we are trying to convert.
- **❌ `llms.txt`.** Google, June 2026: *"Google Search itself doesn't use
  them."* Mueller compares it to the keywords meta tag. 28% of domains ship one
  anyway. Cargo cult.
- **❌ Schema markup for AI citation.** Causally tested — 1,885 pages adding
  JSON-LD vs 4,000 controls: AI Overviews −4.6%, AI Mode +2.4%, ChatGPT +2.2%.
  All statistically zero. Cited pages *correlate* with schema ~3×; adding it
  causes nothing.
- **❌ GEO / "answer-engine optimization" tactics.** The famous "+40%" fails
  replication: **C-SEO Bench** (NeurIPS D&B 2025) finds *"only three [of 54]
  where the ranking improvements are statistically significant,"* and the
  Statistics method *"decreases rankings in 19 out of 24 evaluated settings…
  in contrary to prior beliefs."* The metric is **zero-sum by construction** —
  impressions normalize to 1, so "+40%" is one source taking a slice from the
  other four; GEO's own §5.2 shows that when everyone optimizes, rank-1 loses
  30%. And the benchmark abstracts away retrieval entirely — restore it and the
  same rewrites *"often degrade performance."* **Worst: the top-3 methods
  instruct fabrication** — the repo's own prompts read *"Add more quotes in the
  source, even though fake and artificial"* and *"You may invent these
  sources."* Write well because buyers read it. ([arXiv:2506.11097](https://arxiv.org/abs/2506.11097))
- **❌ Betting on AI referral traffic (yet).** Similarweb, June 2025, top 1,000
  sites: AI referrals **1.13B, +357% YoY**; Google Search **191B**. That's
  **~169:1**. A 357% growth rate on ~0.6% of Google's base is a trend, not a
  channel. Both halves are true; vendors quote one.
- **❌ Product Hunt (probably).** Of 500 PH SaaS launches: **97.4% under $1,000
  MRR, 91.2% under 100 active users.** Head-to-head, PH drove 4× the traffic but
  1/5 the durable signal vs HN. Prefer Show HN.
- **❌ MCP directory listings as an acquisition channel.** Zero documented cases
  of a directory listing driving real acquisition for a small product. Build an
  MCP server because it serves users — do not budget acquisition against it.
- **❌ Buying followers/stars/reviews/upvotes.** Not merely ToS: **16 CFR
  §465.8** makes buying *and selling* fake indicators of social-media influence
  a federal rule violation with civil penalties. See *TruHeight* (§RULES 6).
- **❌ 92 markdown agent personas.** The 334★ repo did this; its median "agent"
  is 1.2 KB of platitudes and it has no runtime. The only real harness in the
  field has ~7 executable task types. **Prefer verbs the machine can run and
  measure over an org chart.**

---

## 6. Metrics that mean something

Ranked by how hard they are to fake. **We optimize the top; we report the
bottom; we never confuse them.**

| Tier | Metric | Why |
|---|---|---|
| **Real** | Revenue; commercial-licence enquiries; ₹ collected | Cannot be faked |
| **Real** | Time-to-first-enriched-row; first-run fill % | The activation moment |
| **Real** | Maintainer first-response latency | Median 8.5h → **71% higher odds** a newcomer resolves an issue. Human attention; cannot be delegated |
| **Directional** | Downloads, release adoption | Real demand, but counts assets not people (bots, CI, mirrors, re-downloads) |
| **Directional** | Show HN front page → durable signups | Traffic is real; conversion is the question |
| **Vanity** | Stars | See HTTPie. Report, never chase |
| **Noise** | Open rates | Apple MPP pre-fetches; Apple Mail ≈58% of opens. Optimize replies only |

**Data gaps are not zeros.** If a metric has no measured value, leave it
untouched and mark it a gap. Never write a 0 where we simply didn't look — a
zeroed KPI silently reads as failure and corrupts every downstream decision.
(Borrowed from `kai-cmo-harness`, which learned it the hard way.)

---

## 7. What the swarm is actually for

Given §5, the honest scope. The machine is a **research, drafting, and
instrumentation engine** — not a mouth.

**The swarm does:** research (ICP, competitors, keywords, channels) · draft
(docs, comparison pages, release notes, READMEs) · audit (broken links, dead
pricing pages, licence detection, watermark coverage, first-run health) ·
instrument (measure what actually happened) · prepare (listings, submissions,
launch artifacts — up to the send button).

**A human does:** anything that speaks to a stranger in the founder's voice.

That is not timidity. It is where the evidence points: the channels that work
for a solo OSS founder — Show HN, the right subreddit, real maintainer
responsiveness, genuine community standing — are **precisely the ones a swarm
cannot do for you.** Automating them is how you lose them.

See `SWARM.md` for the architecture and `RULES-OF-ENGAGEMENT.md` for the gates.

---

## 8. Freshness

Every claim in §2 was verified 2026-07-16 and will rot. Before acting:

```bash
scripts/gtm-verify.sh          # re-checks every number in §2, prints a diff
```

Re-verify on sight if older than **30 days**. Specific decay risks:
- Star/download counts move daily.
- `resubird.com/pricing` may be fixed (check before repeating the claim).
- PR #1168 / issue #1169 may be merged/closed.
- The EU AI Act Art. 50(2) date (**2026-08-02**) is fixed; our compliance state
  is not.

**No primary-source URL, no claim.** See RULES §8.
