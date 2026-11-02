# CompanyEnrich

[CompanyEnrich](https://companyenrich.com/) is a B2B data provider with company
and people research APIs. It enriches known companies, searches for target
accounts and professional profiles, finds similar companies, and reports observed
workforce information. This integration makes those tasks separate workbook
choices, so researching one company does not silently buy people or workforce data.

## Setup

Add a CompanyEnrich key in **Settings → Enrichment providers → CompanyEnrich**,
or configure `COMPANYENRICH_API_KEY` in the operator environment. The existing
workspace-secret resolver can override the operator key per workspace. Saved
operator credentials use the existing encrypted settings store.

Select one of the following providers in an enrichment column and choose its
target field. These providers are not added to any default waterfall. An absent
key or missing required input produces an error before any provider request.

| Provider | Row input | Endpoint | Target fields / result | Maximum credits per call |
|---|---|---|---|---|
| `companyenrich_company` | `website`; otherwise `company` name | `GET /companies/enrich`; otherwise `POST /companies/enrich` | Company size, description, industries, technologies, founding year, revenue band, funding stage, location and social URLs; full JSON in `companyenrich_company` | 1 |
| `companyenrich_person` | Existing `email` | `POST /people/lookup` | Name, title and LinkedIn URL; full JSON in `companyenrich_person` | 5 |
| `companyenrich_workforce` | Company `website` | `GET /companies/workforce` | Full JSON in `companyenrich_workforce`, including department counts and history when available | 5 |
| `companyenrich_people` | Company `website` | `POST /people/search` | First 10 profiles with page metadata in `companyenrich_people` | 20 |
| `companyenrich_search` | Target-market description in `description` (1–500 characters) | `POST /companies/search` | First 10 semantic-search companies with page metadata in `companyenrich_search` | 20 |
| `companyenrich_similar` | Seed company `website` | `POST /companies/similar` | First 10 lookalikes with page metadata/scores in `companyenrich_similar` | 50 |

The full-response fields are JSON **text**. This preserves exact 64-bit person
IDs, nested fields and metadata through the existing text-based workbook cache.
The provider returns the same shape on a fresh call and on a cache hit. Its cache
identity includes the provider and complete row input, including email and search
criteria, rather than sharing different lookups under a company-domain key.

For example, a company row with `website: "https://example.com"` can use
`companyenrich_company` with target `company_size`. A separate column using
`companyenrich_similar` with target `companyenrich_similar` explicitly buys a
bounded lookalike search. A search row can put `B2B payroll software in Europe` in
`description` and choose `companyenrich_search` as both provider and target.

## Cost, data and failure behavior

- Calls consume the operator/workspace's CompanyEnrich credits. Catalog USD
  estimates use the published Starter rate of **$0.0098 per credit**, checked
  2026-11-02. Search estimates reserve the maximum for ten results. Subscription
  rates and actual returned-result costs can differ; the provider account is the
  billing authority. See [pricing](https://companyenrich.com/pricing).
- Search is one bounded page. It never paginates, expands paid fields, launches an
  export, or performs another endpoint call automatically. The full response
  retains provider pagination metadata so a ten-result sample is not represented
  as the entire market or workforce.
- People search returns professional profiles, **not emails or phone numbers**.
  Person lookup uses an email that already exists in the row; it does not discover
  an address. No contact details are synthesized.
- Workforce counts are observed records in CompanyEnrich's people dataset, not a
  claim to know every employee. Company size bands are mapped to OpenGTM's numeric
  band spelling (`501-1K` → `501-1000`); the original stays in full JSON.
- Domain enrichment and lookalikes explicitly send `waitForEnrichment=false`.
  A missing company/seed can return 404 while the provider queues enrichment. A
  later user-requested run may succeed; the integration does not poll or retry.
- Requests use the fixed HTTPS API host and the existing egress guard, with bounded
  responses and deadlines. No redirects or automatic application retries are
  followed. Standard transports use a fresh HTTP/1 connection for each request to
  prevent transparent replay of a paid GET after a reused-connection failure.
  Error messages contain status codes, not raw provider bodies or credentials.
- Confidence `0.7` is an unmeasured planner prior. Fixture tests establish mapping
  and failure behavior, not provider accuracy; the integration remains **beta**.

## Scope

This contribution covers the existing row-enrichment/research surface. It does
not materialize new rows, import contacts, push to an external CRM, or enroll anyone
in an outreach campaign. On a workbook row linked to an existing OpenGTM lead,
the normal enrichment write-back applies returned profile fields to that lead.
Company names and websites remain in the full JSON response rather than being
returned as automatic identity updates. Company search, people search and lookalikes are deliberately
available as complete JSON result cells.

Provider-ID retrieval is unnecessary when starting from the existing row identity.
Batch/bulk jobs, scroll traversal, async exports, saved-list administration, beta
email/mobile discovery and preview/count-only APIs are not exposed here: they
need different inputs, continuation/spend contracts, or account entitlements.
They are excluded deliberately rather than invoked behind an enrichment call.
Advanced search filters and arbitrary page navigation should be a separate source
integration with explicit configuration and spend review.

API schemas were checked against the [official API reference](https://docs.companyenrich.com/reference)
on 2026-11-02. Tests use synthetic fixtures and fake HTTP transports; they need no
live API key and spend no credits.
