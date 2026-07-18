# RULES OF ENGAGEMENT — the GTM swarm

> **v1 · 2026-07-16 · Not legal advice.**
> Derived from ~40 primary-source verifications. Every rule carries its citation.
> If you are an agent: **these are not suggestions. A FORBIDDEN item has no
> human override.**

Governs three products with three different exposures:

- **(a) lead-data** (Clay alternative, unpublished) — **carries nearly all the
  legal risk in the portfolio.**
- **(b) OmniVoice-Studio** — local, offline TTS/voice-cloning desktop app.
  **Not a dialer.** Almost none of the outreach law binds it.
- **(c) ResuBird** — B2C, India-first, job seekers. **Stricter, not looser,
  because it's consumer.**

---

## 0. Tool-maker vs. caller — read this before anything about voice

**OmniVoice is not an outbound calling product, and the TCPA does not bind it.**

TCPA liability attaches to whoever **makes the call** — 47 U.S.C. §227(b)(1),
*"to make any call…"*. The FCC's Feb 2024 ruling (FCC 24-17) holds that
**callers** using AI voice are using an "artificial" voice. Every downstream
obligation — written consent, DNC scrubbing, identification, opt-out — is an
obligation **of the caller**. If a user generates audio in OmniVoice and dials
with it, **the user is the initiator and carries the entire exposure.**

The only route to vendor liability is vicarious (FCC's 2013 *Dish Network*
ruling: a **seller** liable for calls made *"on its behalf"* under common-law
agency). Users do not dial on OmniVoice's behalf. A shrink-wrapped, **offline,
local** app — no servers touched, no visibility, no control — is legally closer
to a microphone than to a dialer.

The strongest US signal on AI **tool-maker** liability points the same way: the
FTC asserted a "means and instrumentalities" theory against **Rytr** (an AI tool
whose users *might* generate fake reviews) and **vacated it on 2025-12-22** —
*"Treating as categorically illegal a generative AI tool merely because of the
possibility that someone might use it for fraud is inconsistent with our
precedents and common sense."*
([FTC](https://www.ftc.gov/news-events/news/press-releases/2025/12/ftc-reopens-sets-aside-rytr-final-order-response-trump-administrations-ai-action-plan))

### What actually binds OmniVoice

| Binds? | Regime | Why |
|---|---|---|
| ❌ | TCPA / FCC AI-voice ruling / PEWC / DNC | Bind the caller. We aren't one. |
| ❌ | CAN-SPAM / CASL / PECR | No messages sent. |
| 🔴 **YES** | **EU AI Act Art. 50(2)** | **Applies 2026-08-02. See below.** |
| ⚠️ | **ELVIS Act (TN)** | Only if primary purpose = a *particular identifiable individual*. |
| 👀 | **NO FAKES Act** | Passed Senate 2026-01-13; pending in House. Would federalize tool-maker liability. |
| ⚠️ | Right of publicity (*Midler v. Ford*, *Waits v. Frito-Lay*) | Binds our **marketing**, not our code. |

### 🔴 EU AI Act Article 50(2) — applies 2026-08-02

> *"Providers of AI systems, including general-purpose AI systems, generating
> synthetic audio, image, video or text content, shall ensure that the outputs
> of the AI system are marked in a machine-readable format and detectable as
> artificially generated or manipulated."*
> — [Art. 50](https://artificialintelligenceact.eu/article/50/)

**The AGPL licence does not exempt us.** Art. 2(12) exempts free and open-source
AI systems *"unless they are placed on the market or put into service as
high-risk AI systems or as an AI system that falls under Article 5 or **50**."*
**Article 50 is expressly carved out.** Every "we're open source, the AI Act
doesn't apply" take is wrong on this specific point.
([Art. 2](https://artificialintelligenceact.eu/article/2/))

**Current state (verified 2026-07-16, in code):** OmniVoice **already** embeds
AudioSeal invisible watermarks, **default-on** —
`resolve("watermark.invisible", default=True)` — applied in
`_finalize_generation`, covering `generation.py` (classic + streaming),
`dub_generate.py`, and `persona_bundle.py`. **This is substantially compliant
already.** Two open items:

1. **`/v1/audio/speech` (`openai_compat.py`) does not watermark** — confirmed
   gap, filed as issue #1169.
2. **Does a user-defeatable toggle satisfy "shall ensure"?** The obligation is
   the provider's. **Lawyer question, not an engineering one.**
3. **Does a non-commercial AGPL release "place on the market"?** Art. 3(9)–(10)
   ties this to *"the course of a commercial activity."* Colourable — but do
   **not** rely on it if we monetize at all (dual licence, sponsorship, pro
   tier). Arguing it puts our commercial future in tension with our compliance
   posture. **Watermark instead; it's cheaper than the argument.**

### 🔴 FORBIDDEN — OmniVoice marketing discipline

**Never market OmniVoice as a cold-calling or outbound tool.** Our legal posture
— *general-purpose, local, offline, open-source* — is genuinely strong, and
**our own marketing copy is the only thing that can destroy it.**

**Never ship pre-trained celebrity/identifiable-person voice packs, and never
demo with a famous voice.** The ELVIS Act reaches a tool whose *"primary purpose
… is the production of a **particular, identifiable individual's** … voice."*
Commentary: *"A voice-cloning app trained exclusively on one artist's recordings
would be squarely in the crosshairs; a general text-to-speech engine likely
would not."* That single act converts a general-purpose engine into the thing
the statute names.
([Latham](https://www.lw.com/admin/upload/SiteAttachments/The-ELVIS-Act-Tennessee-Shakes-Up-Its-Right-of-Publicity-Law-and-Takes-On-Generative-AI.pdf))

---

## 1. ALLOWED / GATED / FORBIDDEN

### 🔴 FORBIDDEN — no human override

| # | Rule | Why | Citation |
|---|---|---|---|
| 1 | AI voice → any mobile, telemarketing, without **prior express *written* consent** | §64.1200(a)(2): PEWC = a **signed** writing naming the **specific number**, disclosing the artificial voice, before the first call. **No cold path exists.** | [47 CFR 64.1200](https://www.ecfr.gov/current/title-47/chapter-I/subchapter-B/part-64/subpart-L/section-64.1200) |
| 2 | Cold email to **Germany/Austria** | UWG §7(2)(2): express consent **even B2B**. Enforced by competitor *Abmahnung* with cost-shifting, not just regulators. Block by TLD, registry, inferred location. | [SRD](https://www.srd-rechtsanwaelte.de/en/blog/email-marketing-without-consent) |
| 3 | Cold SMS (anywhere); any marketing SMS/call to **India** | TCPA PEWC + 10DLC. India: TRAI TCCCPR + DND. | [Infobip](https://www.infobip.com/blog/tcpa-compliance-sms) |
| 4 | Fake reviews / testimonials / followers / upvotes / stars | **Illegal, not just ToS.** 16 CFR §465.2, §465.4, **§465.8**; UK DMCCA Sch. 20 ¶13; EU UCPD ¶23b–c. | [16 CFR 465](https://www.ecfr.gov/current/title-16/chapter-I/subchapter-D/part-465) |
| 5 | Undisclosed AI personas in communities | Reddit banned the accounts **and sent legal demands** (U. Zurich, 2025). | [WaPo](https://www.washingtonpost.com/technology/2025/04/30/reddit-ai-bot-university-zurich/) |
| 6 | LinkedIn automation — **any** | UA §8.2 bans bots to *"add or download contacts, send or redirect messages … or otherwise drive inauthentic engagement."* | [LinkedIn UA](https://www.linkedin.com/legal/user-agreement) |
| 7 | Discord self-bots | Automating a user account outside the bot API *"can result in an account termination."* | [Discord](https://support.discord.com/hc/en-us/articles/115002192352-Automated-User-Accounts-Self-Bots) |
| 8 | Soliciting upvotes (HN / PH / Reddit) | HN: *"Don't solicit upvotes, comments, or submissions."* Show HN: *"Please don't ask friends to upvote."* | [HN](https://news.ycombinator.com/newsguidelines.html) |
| 9 | Sockpuppets / multi-account voting | Reddit vote-manipulation policy; §465.2 if reviews involved. | [Reddit](https://support.reddithelp.com/hc/en-us/articles/360043504051-Spam) |
| 10 | Scraping **behind a login**, or via **fake accounts** | Every company-ending case is here: hiQ ($500k + injunction + destruction of data), **Proxycurl shut down 2025 despite ~$10M ARR**, Ryanair CFAA verdict. | [Proskauer](https://www.proskauer.com/blog/hiq-and-linkedin-reach-proposed-settlement-in-landmark-scraping-case) |
| 11 | Promotional comments on others' GitHub issues | *"You may not advertise in other Users' Accounts, such as by posting monetized or excessive bulk content in issues."* | [GitHub AUP](https://docs.github.com/en/site-policy/acceptable-use-policies/github-acceptable-use-policies) |
| 12 | **Bulk cold email from Google Workspace** | AUP bans unsolicited bulk email **and** *"data mining any web property … to find email addresses."* Remedy: ***"we reserve the right to suspend the entire account and deny administrator access to all the Google Workspace services."*** **Our mail, docs, and calendar are the collateral.** | [Workspace AUP](https://workspace.google.com/terms/use_policy/) |
| 13 | **OmniVoice: identifiable-person voice packs / famous-voice demos** | Converts a general engine into an ELVIS Act "primary purpose" tool. | §0 |
| 14 | Emailing GitHub stargazers / scraping our own star list for outreach | Scraping + CAN-SPAM + the fastest way to convert 8,509 supporters into 8,509 detractors. | §2, §7 |

### 🟡 GATED — human approval, logged, basis recorded

| Rule | Why | Citation |
|---|---|---|
| **Any** cold email (US / UK-corporate only, entity verified) | Legal — but strict liability at **$53,088/email**. Never autonomous. | [FTC](https://www.ftc.gov/business-guidance/resources/can-spam-act-compliance-guide-business) |
| AI voice → **verified business landline** | §227(b)(1)(B) reaches only *"residential telephone line."* TSR + state law still apply. | [47 U.S.C. §227](https://www.law.cornell.edu/uscode/text/47/227) |
| Publishing anything claiming a customer's words | §465.2 has **no knowledge qualifier** for creators. | [16 CFR 465.2](https://www.ecfr.gov/current/title-16/chapter-I/subchapter-D/part-465) |
| First-touch in any new community | Enforcement is per-mod and unwritten. | — |
| Buying any lead data | You become an **independent controller** on purchase (Art. 4(7)). Vendor compliance ≠ your compliance. | [CNIL/Kaspr](https://www.cnil.fr/en/data-scraping-kaspr-fined-eu240000) |
| **Show HN launch** | One shot, ungameable, must be genuinely runnable. | [Show HN](https://news.ycombinator.com/showhn.html) |
| Any claim about ATS pass rates / efficacy (ResuBird) | Unsubstantiated efficacy = the *TruHeight* fact pattern. | §6 |
| Review solicitation / gating | **Not** federally banned — but bites via §465.7(b), UK ¶13(2), EU ¶23b, Google ToS. **Still don't.** | [FTC FRN](https://www.ftc.gov/system/files/ftc_gov/pdf/r311003consumerreviewstestimonialsfinalrulefrn.pdf) |

### 🟢 ALLOWED — autonomous

Content · docs · comparison pages · README/release notes · **logged-out**
scraping of public data **for research** (not EU outreach) · marketplace and
registry listings · answering questions **where we have genuine standing, with
affiliation disclosed** · warm/inbound follow-up · auditing our own properties ·
**drafting anything for human review.**

---

## 2. Cold outreach law — the short version

**🇺🇸 CAN-SPAM — cold email is LEGAL. Opt-out regime.** FTC, verbatim on both
commonly-misstated points: *"The CAN-SPAM Act doesn't require initiators of
commercial email to get recipients' consent"* and *"The law makes no exception
for business-to-business email."* Seven requirements: accurate headers ·
non-deceptive subject · identify as ad · valid physical postal address · clear
opt-out · opt-out live ≥30 days, honored **within 10 business days** · don't
sell opt-out addresses. **$53,088 per email.**
⚠️ Verify annually — it did **not** rise in 2026 (OMB Memo M-26-11 cancelled the
adjustment after the Oct 2025 shutdown blocked the CPI-U calculation). Most
sources print the stale $51,744.

**🇬🇧 UK — corporate subscribers only.** ICO: *"You can send unsolicited
electronic mail marketing to corporate subscribers without consent or a soft
opt-in."* **Two traps:** (1) **sole traders and ordinary partnerships are
"individual subscribers"** → consent required, and this is **not inferable from
an email address** — unverified entity type = **refuse**; (2) **PECR ≠ UK
GDPR** — you still need Art. 6(1)(f) + a documented LIA + Art. 13/14
transparency. **PECR maxima rose from £500,000 to £17.5m / 4% of global
turnover** (DUAA 2025, commenced **SI 2026/82, 2026-02-05**) — pre-2025 guidance
understates this ~35×.

**🇨🇦 CASL — consent-first, with expiry clocks.** Implied consent: **2 years**
from a business relationship, **6 months** from an inquiry — **both expire;
encode as timestamps with automatic suppression.** The killer: CRTC states it
*"does not permit the mining of email addresses from third-party directory
websites"* — **a scraped Apollo/ZoomInfo record does not qualify.** Sender bears
the burden of proving consent. **CAD $1M / $10M.** CASL expressly covers social
media messages — a cold LinkedIn DM to a Canadian is a CEM.

**🇮🇳 India — ResuBird's regime, and it's stricter because it's B2C.** DPDP Act
2023 + Rules 2025 (notified 2025-11-13; full compliance **2027-05-13**). Consent
must be *free, specific, informed, unconditional, unambiguous, with clear
affirmative action.* **No legitimate-interest basis for marketing — consent or
nothing.** Resumes are personal data; job seekers are a sensitive cohort.

**LinkedIn DMs:** CASL covers them; UK PECR likely does; **the US is a genuine
statutory vacuum.** Operationally irrelevant — LinkedIn UA §8.2 forbids the
automation regardless. **The constraint is contractual, not statutory.**

---

## 3. Deliverability — preconditions before ANY send

Hard gate. Any failure → **do not send.**

> ⚠️ **These rules do not discipline B2B cold email, and anyone who tells you
> otherwise (including an earlier draft of this file) has not read Google's
> FAQ.** Bulk status counts mail *"to **personal Gmail accounts**"* — mail to
> Google **Workspace** business tenants does not count and the guidelines do not
> apply to it. Microsoft's `550 5.7.515` enforcement is **consumer
> Outlook/Hotmail/Live only, not M365 tenants**. Yahoo **publishes no threshold
> and says so** (*"We will not specify a volume threshold"* — the 5,000 figure
> attributed to Yahoo is Google's, misapplied). **B2B cold outbound to
> Workspace/M365 tenants sits outside all four regimes.**
>
> **What actually disciplines cold email is spam filtering and reputation, not
> these published rules** — plus the Workspace AUP (FORBIDDEN #12), which is a
> contract, not a filter, and takes your whole domain. Note also that the
> many-domains × many-mailboxes playbook stays under the threshold *by
> construction* — which is precisely why the threshold is not the control.
>
> Two mechanics worth keeping: bulk status is **permanent** (*"Senders who meet
> the above criteria at least once are permanently considered bulk senders"* —
> one spike, forever), and it is counted **per primary domain, with subdomains
> rolling up** — so subdomain separation protects *reputation*, not *bulk
> status*.
> ([Google FAQ](https://support.google.com/a/answer/14229414))

**Google/Yahoo** — bulk = **5,000+/day to personal Gmail**, assessed at the
**primary domain**, **permanent once triggered**:
- SPF **and** DKIM **and** DMARC (**p=none** minimum), From-domain aligned
- **One-click unsubscribe** (RFC 8058 `List-Unsubscribe-Post`), honored ≤2 days
- Spam complaints **<0.10% target, 0.30% hard ceiling**
- Valid PTR / forward-confirmed reverse DNS; TLS

**Microsoft** (from 2025-05-05): SPF+DKIM+DMARC or hard rejection `550 5.7.515`.

**p=none remains the floor** at all three in 2026. The change is *severity*
(soft-defer → hard 550), not new rules. Also: SPF ≤10 DNS lookups · DKIM
2048-bit · **subdomain separation** — transactional / marketing / prospecting
must never share reputation.

**🔴 Get cold email off Google Workspace.** See FORBIDDEN #12.

**Scraped lists burn domains, and verification does not fix it.** **Pristine
spam traps** are seeded *precisely* into scraped sources and have no opt-in path
— hitting one is near-proof of scraping. NeverBounce/ZeroBounce verify *mailbox
existence*, not *provenance*; pristine traps are real, deliverable mailboxes
**by design**. ***"Verified" ≠ "safe."***

**Open rates are dead.** Apple MPP pre-fetches; Apple Mail ≈58% of opens.
**Optimize on replies only.**

⚠️ **"Cold email reply rates collapsed" is a denominator swap, not a finding.**
The famous 5.1% → 0.45% collapse is Belkins changing denominators —
replies÷openers → replies÷total sent. In their own words: *"A 5% reply rate
against openers and a 0.45% reply rate against total sends can describe the same
campaign; they're just measuring different things."* Apple MPP broke open
tracking mid-window, so the old denominator was corrupt too. **No clean series
exists — not even the direction is evidenced.** Counter-evidence against
interest: Validity (who sell deliverability tools) report 2025 global inbox
placement **recovering to 87.2%, +3.7%**, with volume declining.
([Belkins](https://belkins.io/blog/cold-email-response-rates))

---

## 4. Scraping — two layers, everyone gets one

**Layer 1 — CFAA: public scraping is fine.** *hiQ v. LinkedIn* (9th Cir. 2019,
reaffirmed 2022): scraping **public, unauthenticated** data isn't access
"without authorization." *Van Buren* (2021) gives the **gates-up-or-down** test.
A public page has no gate.

**Layer 2 — Contract: this is where hiQ actually lost.** Nov 2022 summary
judgment for LinkedIn on **breach of contract** (fake accounts via Mechanical
Turk); Dec 2022 consent judgment — **$500,000, permanent injunction, destruction
of derived code and data.** *hiQ won the famous CFAA point and lost the company.*

**2024–25 — logged-out vs. logged-in is the whole ballgame:**
- *Meta v. Bright Data* (2024): Meta **lost** — Terms *"do not bar logged-off
  scraping of public data."*
- *X Corp. v. Bright Data* (Alsup, 2024): X **lost**.
- *Ryanair v. Booking.com* (2024): first civil CFAA jury verdict — **login-gated**.
- *LinkedIn v. Proxycurl* (2025): **fake accounts** → injunction; **shut down**.

**Encode: logged-out + public + no account = low risk. Logged-in or fake accounts
= the bucket every company-ending case sits in.**

**robots.txt** (RFC 9309) is **not legally binding** — at most evidence of
knowledge/bad faith.

**GDPR — the real constraint for lead-data:**
- **Business contact data IS personal data.** No B2B exemption. Recital 47 says
  marketing *"may be regarded as"* a legitimate interest — a **starting point
  for balancing**, not a permission slip.
- **Art. 14 notice** within **1 month**, or at first communication.
- **"Too expensive to notify" is a losing argument with precedent against it.**
  Art. 14(5)(b) means **impossibility, not cost**. *Bisnode/Poland*: €220k for
  using a website notice instead of notifying ~6M people — upheld, and the
  **Supreme Administrative Court affirmed (Jan 2026)**.
- **CNIL v. Kaspr — €240,000** (SAN-2024-020, 2024-12-05) — directly on point:
  a Chrome extension surfacing LinkedIn contact details for prospecting.
  **Restricting profile visibility does not authorize third-party scraping.**
  CNIL **did not validate legitimate interest**.
  ⚠️ The EDPB publishes both €240,000 and €200,000 for this decision. **CNIL
  issued it and says €240,000. Cite CNIL.**
- **US state:** CCPA's B2B exemption **expired 2023-01-01**. **Delete Act /
  DROP**: brokers must process from **2026-08-01**, check every 45 days,
  **$200/day per request**. **Data-broker registration required in CA, TX, OR,
  VT** — if lead-data sells/licenses contact data on people we have no
  relationship with, **that is us**.

---

## 5. Platform rules — bannable vs. frowned upon

**Hacker News.** *"Don't solicit upvotes, comments, or submissions."* ·
*"Please don't use HN primarily for promotion."* **Bannable:** voting rings,
sockpuppets, **domain-level penalties** (applied quietly, rarely reversed).
⚠️ **The real trigger is pattern-based:** Aha!'s CEO lost posting privileges for
*"repeatedly posting from a single site"* — an automated flag, **no vote
manipulation at all.**

**Reddit — the 9:1 rule is FOLKLORE. Do not encode it.** Reddiquette is
informal and community-authored, explicitly distinct from the enforceable
Content Policy. **Actually bannable:** vote manipulation, ban evasion, same link
from different accounts, requesting upvotes **anywhere including off-platform**,
and — *"if your only Reddit activity is sharing links to your own website or
product"* — spam **regardless of content quality**. Domain-level shadowbans exist.

**Product Hunt.** *"Mass messaging users, asking for upvotes, using bots,
incentivizing upvotes … is not acceptable."* Graduated: *asking* → rank
suppression; *buying* → removal. You **may** invite people to look and give
honest feedback.

**GitHub — "can we comment on related repos?" → No.** Genuine technical
participation where we have standing (we hit the bug, we're fixing it),
affiliation disclosed = fine. Drive-by "check out our tool" = spam, bannable.
**An agent cannot reliably tell these apart → FORBIDDEN autonomously.**
⚠️ **Fake stars are detected:** ~6M suspected fake stars across 18,617 repos;
**90.42% of flagged repos deleted by Jan 2025.**
([arXiv:2412.13459](https://arxiv.org/abs/2412.13459))

**LinkedIn.** All automation violates §8.2. Routine consequence is temporary
restriction; permanent bans and litigation are reserved for scale/resale/fake
accounts. **A risk decision, not a grey area.**

**Discord.** Self-bots banned by name. **X.** Automated DMs require the
recipient to have **requested** contact — **following you is not consent.**
**Stack Overflow:** affiliation must be disclosed; AI-answer ban active.
**dev.to:** AI content **for promotion is banned even with disclosure.**

---

## 6. FTC fake-review rule — 16 CFR Part 465

Effective **2024-10-21**. **⚠️ The numbering is not sequential** — the FTC
dropped proposed §465.3 (review hijacking) and left it **[Reserved] without
renumbering**. Anyone assuming sequential numbering is off by one from §465.4 on.

| § | Title | Key detail |
|---|---|---|
| 465.2 | **Fake/False Reviews & Testimonials** | (a) creators/sellers — **no knowledge qualifier**. Covers **AI-generated** reviews |
| **465.3** | **[Reserved]** | Dropped |
| 465.4 | **Buying Positive/Negative Reviews** | Only **sentiment-conditioned** compensation. Disclosure does **not** cure it |
| 465.5 | Insider Reviews | Incl. soliciting from immediate relatives |
| 465.6 | Company-Controlled Review Sites | — |
| 465.7 | Review Suppression | Incl. misrepresenting that displayed reviews "represent most or all" submitted |
| **465.8** | **Misuse of Fake Indicators of Social Media Influence** | **Buying *and* selling** fake followers/views/likes |

**§465.8 — not §465.7 — is the clause a growth swarm will trip.**

**Penalty: up to $53,088 per violation**; each day of a continuing violation is
a separate violation. **No private right of action** — FTC only.

**Enforcement is real. *FTC v. Vanilla Chip LLC d/b/a TruHeight*** (complaint
Apr 2026, **final order July 2026**): reviews **written by their own employees
and vendors**; 5-star reviews incentivized with free product; **fake social
profiles masquerading as real users but run by bots**. **$4,000,000 judgment,
suspended to $750,000** on inability to pay.

**That fact pattern is exactly what an unsupervised growth swarm generates** —
§465.2 + §465.4 + §465.8 in one case. Note the recurring shape (Cure
Encapsulations $12.8M→$50k; Roomster $36.2M→$1.6M; TruHeight $4M→$750k):
**headline judgments get suspended to ability-to-pay — but a suspended judgment
still ends the company.**

**🔴 Directly relevant to ResuBird.** TruHeight was *unsubstantiated efficacy
claims + fake reviews*. **"Beat the ATS" claims + seeded testimonials is the
same case.** Our `4.8/1250` vs `4.8/12` markup discrepancy is the thread a
regulator pulls. **Substantiate or delete.**

**🇬🇧 UK:** DMCCA 2024, Sch. 20 ¶13, in force 2025-04-06. CMA has **direct
enforcement**, penalties to **10% of global turnover**. First investigations
opened **2026-03-27**.
**🇪🇺 EU:** UCPD Annex I ¶23b = an **affirmative duty** to verify reviewers are
genuine; ¶23c bans commissioning false reviews **including fake "likes."** To
**4% of turnover**.

---

## 7. Enforcement — where these rules actually live

**Prompts are not a control surface.** An agent that "knows" the rules will
violate them fluently under pressure. Every gate below is enforced by
machinery that does not read English.

**1. The capability token.** lead-data's MCP server already scopes writes by
capability (`apps/api/services/mcp/auth.py`):

```
leads:read · leads:write · sequences:enroll · workbooks:write
automations:write · outreach:send · leads:delete · workbooks:delete
```

**The swarm's MCP token is minted WITHOUT `outreach:send`, `leads:delete`, or
`workbooks:delete`.** The FORBIDDEN list is then not a policy the agent chooses
to follow — it is a capability it does not possess. `send_email` is documented
in-tree as *"Admin only — HIGHEST RISK."* It stays that way.

**2. The caps ledger.** Every MCP write reserves against the same
`trigger_cap_reservations` table the trigger engine uses
(`apps/api/services/mcp/caps.py`), with `UniqueConstraint(workspace_id,
idempotency_key)` as the idempotency ledger. **The swarm cannot exceed the
tenant's daily budget, and a retried write is a detected replay, not a
double-send.** Cost-bearing writes are bounded by `AUTOMATIONS_GLOBAL_DAILY_USD`
for free.

**3. The suppression path.** `sender.py` already supports `List-Unsubscribe` /
one-click. Any send path inherits suppression, rate limits, and the bounce
circuit breaker — or it isn't a send path.

**4. The audit log.** RLS-scoped, immutable. **CASL and GDPR both put the burden
of proof on us** — an action without a logged basis is an action we cannot
defend.

**Hard gates in code, not prompt guidance:**
- Jurisdiction resolution **before** composition
- Entity-type (corporate vs. sole trader) as a hard gate on UK email
- Line-type verification as a hard gate on any voice call
- Global send-rate ceiling that **fails closed**
- Consent-expiry timers (CASL 2yr/6mo)
- Suppression checked at send time, honored ≤10 business days, **across channels**

---

## 8. The refusal gate

> ### No primary-source URL, no action.

Before relying on any rule, threshold, section number, or legal conclusion, an
agent must produce a citation resolving to a **primary source** — `.gov`,
`.europa.eu`, `.gc.ca`, `legislation.gov.uk`, a court opinion, or the platform's
own policy page. **A vendor blog, an SEO article, or the agent's own
recollection is not a source. If it cannot produce one, it refuses and
escalates.**

**This is calibrated to observed failure, not paranoia.** The research pass that
produced this document made **five errors**, all in the same direction: *filling
a gap with plausible inference, stated in the same confident register as
verified fact.*

- Assumed §465 numbering was sequential (it isn't — §465.3 is [Reserved])
- Said no Part 465 case existed (*TruHeight* had just closed)
- Cited the wrong section for review gating (and the FTC declined to ban it)
- Called MCP registries "the closest thing to an unsaturated channel" (zero
  documented ROI — pure vibes)
- Said "prior express consent" where the law says **written** consent

**Not one announced itself as a guess.** And in the wild, a research pass
surfaced a **fabricated case — *"RetailScrape v. MegaMart (2026)"*** — in a
blog, with generic placeholder party names, corroborating to nothing. **It is
not real. An agent optimizing for a confident answer will cite it.**

Two more instances from this very session, both caught only by checking:

- A research agent reported "137k downloads" — **true**, but it is *asset*
  downloads across 563 assets, not 137k people. The number was right and the
  implication would have been wrong.
- A research agent's #1 priority across all three products was *"ship audio
  watermarking before Aug 2."* **OmniVoice already shipped it, default-on,
  months ago.** Two weeks would have been spent building a thing that exists.

**The rule that would have caught all of these: check the artifact, not the
claim about the artifact.**

### 🔴 You cannot verify a statistic with AI search

**Search-engine AI summaries reproduced a known misattribution live, twice,
during this project's own research.** Spot-checking a stat via AI search
**confirms the error rather than catching it** — the laundering now happens
inside the retrieval layer. Chase every number to a primary source or don't use
it.

**The fabrications are on both sides of every argument, and the fake numbers are
better than the real ones.** Verified non-existent during this research:

- *"72% of buyers use ChatGPT to evaluate vendors — Forrester 2026"* — **that
  report does not exist.**
- *"Gartner: 75% of B2B teams…"* and *"Forrester B2B Sales Automation Index Q1
  2026"* — **neither exists.**
- *"Buyers are 70% through the journey before contacting vendors"* — **appears in
  no original.** A garble of CEB/Google 2012's 57% (n=1,500, but from only **22
  organizations**). Forrester's own Lori Wizdo calls these *"discredited"* and
  building strategy on them *"irresponsible."*
- *"Lavender: 100M emails, AI 2.4% vs human 3.8%"* — Lavender's real report is
  231,818 emails **with no AI-vs-human comparison at all.**
- *"Artisan's 95% churn, from their annual report"* — **Artisan is private; no
  such report exists.** Sole origin: one pseudonymous Reddit user.
- *"RetailScrape v. MegaMart (2026)"* — a **fabricated court case** with
  placeholder party names.

And there is **provable astroturf** in this exact category: identical promotional
text posted to different subreddits **19, 20, and 76 seconds apart**; a vendor
ring where *"Enterprise sales leader here"* and *"Sales Director here"* post 76
seconds apart, both trashing a competitor, both pitching.

**Consequence for us:** there is **no credible measured evidence that AI cold
email underperforms human cold email** — the claims were never tested and the
counter-claims were invented. We reject the category on the grounds in GOAL §5
(legal exposure, our own compliance posture, reputational cost), **not** on
efficacy folklore we cannot source.

### The confabulation signal

**Watch for an agent framing its output as "corrections to my earlier report,"
referencing prior claims it never made.** This occurred repeatedly in this
project's own research and is a reliable tell. An agent's self-narrated history
is not evidence.

**Data gaps are not zeros.** A KPI with no measured value is a **DATA GAP** —
leave the stored value untouched, **never zero it**. A zeroed metric reads as
failure and silently corrupts every downstream decision.

**Get a lawyer before:** any AI voice outbound · any EU outreach · our Art.
6(1)(f) LIA · data-broker status in CA/TX/OR/VT · **OmniVoice's Art. 50 "placing
on the market" question.**

---

## 9. Recheck cadence

FCC AI-disclosure NPRM · first *litigated* Part 465 penalty · FTC penalty amount
(frozen at $53,088; resumes when CPI-U allows) · **NO FAKES Act in the House** ·
EU Digital Fairness Act (proposal targeted Q4 2026) · CMA's five open
investigations · DPDP phase-ins to 2027-05-13 · whether mail providers escalate
p=none → quarantine/reject · **EU AI Act Art. 50(2) — applies 2026-08-02.**

---

**The one-line version:** the law mostly permits US cold email and mostly
forbids everything else you'd want to automate; the platforms forbid nearly all
of it; and the channels that actually work for a small product are **the ones a
swarm can't do for you.** Aim the swarm at research, drafting, docs, and
listings. Keep a human on anything that speaks to a stranger in our voice.
