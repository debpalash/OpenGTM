---
title: Enrichment providers (BYOK)
description: Every built-in provider, its key, its free tier, and how to add one with a YAML manifest.
sidebar:
  order: 2
---

Providers fill fields. Most are free and keyless; the paid ones use your own
API key at the vendor's direct price. Keys go in **Settings → API Keys**
(encrypted per workspace) or in `.env`.

## Paid, bring-your-own-key

| Provider id | Fills | Key | Free tier | List price |
|---|---|---|---|---|
| `hunter_io` | email | `HUNTER_API_KEY` | 25 lookups/month | $0.04 |
| `apollo_io` | email, phone, contact person and title, LinkedIn, company size | `APOLLO_API_KEY` | 50 credits/month | $0.03 |
| `snovio` | email, contact person and title | `SNOVIO_CLIENT_ID` + `SNOVIO_CLIENT_SECRET` | 50 credits/month | $0.03 |
| `prospeo` | email, email verify | `PROSPEO_API_KEY` | 75 credits/month | $0.02 |
| `people_data_labs` | person and company data | `PDL_API_KEY` | 100 matches/month | $0.03 |
| `abstract_api` | email verify | `ABSTRACT_API_KEY` | 100/month | $0.01 |
| `debounce` | email verify | `DEBOUNCE_API_KEY` | 100 verifications | $0.008 |
| `numverify` | phone verify, carrier, line type | `NUMVERIFY_API_KEY` | 100/month | $0.005 |
| `ipinfo` | company from IP/domain, org, location, ASN | `IPINFO_TOKEN` | 50,000 requests/month | free tier |
| `google_maps` | phone, address for local businesses | `GOOGLE_MAPS_API_KEY` | $200/month credit | $0.005 |
| `leadmagic_email` | email | `LEADMAGIC_API_KEY` | | $0.05 |
| `leadmagic_company` | company size, description | `LEADMAGIC_API_KEY` | | $0.05 |
| `leadmagic_mobile` | mobile phone | `LEADMAGIC_API_KEY` | | $0.05 (0 if not found) |
| `prospeo_mobile` | mobile phone | `PROSPEO_API_KEY` | | $0.10 |
| `companies_house` | UK register firmographics | `COMPANIES_HOUSE_API_KEY` | free | $0 |
| `mca_registry` | India MCA register | `DATA_GOV_IN_KEY` | free | $0 |

The last four rows in the first group (`leadmagic_*`, `prospeo_mobile`) are
[declarative YAML manifests](#adding-a-provider-with-a-yaml-manifest).

## Free and keyless

| Provider id | Fills |
|---|---|
| `mailscout` | SMTP email verification (built in) |
| `holehe` | email existence across 120+ sites |
| `ddg_email`, `ddg_company` | email, website, phone, description via search |
| `email_harvester`, `website_scraper`, `deep_scraper` | emails, phones, socials, description scraped from the company site |
| `jsonld_firmographics` | schema.org firmographics |
| `company_intel` | funding, news, size |
| `local_business`, `facebook_pages` | phone, address, industry tags |
| `social_finder` | LinkedIn and Twitter URLs |
| `crosslinked`, `decision_maker` | decision makers, contact person and title |
| `tech_stack` | technologies (needs `TECH_STACK_WEBSITE_FETCH_ENABLED=1`) |
| `ats_hiring`, `jobspy` | hiring signals, open roles, technologies |
| `wikidata`, `gleif` | firmographics, legal entity and hierarchy (CC0) |
| `sec_edgar` | US filings, funding and executives (set `SEC_EDGAR_USER_AGENT`) |
| `lead_scorer` | score and tier |
| `company_size_heuristic` | company size inference (`COMPANY_SIZE_HEURISTIC_ENABLED=1`) |
| `staffspy` | full LinkedIn roster; opt-in via `STAFFSPY_ENABLED` and a session file |

Every source must respect the target's terms of service; credentialed
providers are inert until their key is set.

## Reacher (self-hosted verification)

[Reacher](https://github.com/reacherhq/check-if-email-exists) adds unlimited,
$0 SMTP RCPT probing with catch-all, disposable and role detection. It is
dual-licensed AGPL / commercial and runs as a separate container:

```bash
docker compose --profile reacher up
# then in .env
REACHER_ENABLED=1
REACHER_API_KEY=<the container's header secret>
```

`REACHER_URL` is global and never overridable per workspace (SSRF mitigation).
Most clouds block outbound port 25, so a self-hosted Reacher usually needs a
SOCKS5 proxy (`REACHER_PROXY_*`); otherwise it returns `unknown` and the
cascade falls back to the bundled SMTP probe. A circuit breaker opens after
three consecutive failures for 60 seconds.

## Provenance and licences

With `PROVENANCE_TRACKING_ENABLED=1`, every filled field records its source,
licence (`CC0-1.0`, `CC-BY-4.0`, `public-record`, `scraped`,
`proprietary-api`, `user-provided`, `unknown`), confidence and fetch time,
exposed per cell as `provenance` in the API. Off by default so existing
payloads stay byte-identical.

## Adding a provider with a YAML manifest

Most REST providers need no Python. Drop a manifest under
`apps/api/services/leadgen/enrichment/declarative/manifests/<capability>/`:

```yaml
name: acme_email
capability: email
description: Acme email finder (BYOK; inert until ACME_API_KEY is set)
default_confidence: 0.8
cost_per_lookup: 0.03
auth:
  type: header
  param: X-API-Key
  env_var: ACME_API_KEY
request:
  method: GET
  url: https://api.acme.example/v1/find
  query:
    domain: "{website_domain}"
    name: "{contact_person}"
  timeout: 10
response:
  error_path: error.message
  mappings:
    email: data.email
    email_confidence: data.score
input_fields: [website_domain, contact_person]
```

The manifest compiler validates it at load, registers it under `name`, and the
vendor catalog picks up `cost_per_lookup` for estimates. Ship a golden fixture
so the accuracy evaluation can rank it:

```bash
uv run python -m apps.api.services.leadgen.enrichment.eval.cli --json
uv run python -m apps.api.services.leadgen.enrichment.eval.cli --persist   # feed the planner
```
