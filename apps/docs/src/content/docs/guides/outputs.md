---
title: Outputs and CRM push
description: Push rows to a webhook, HubSpot, Salesforce, Google Sheets, Airtable, or an email sequencer.
sidebar:
  order: 5
---

An `output` column pushes each row to a destination after enrichment. It is
side-effecting, so it runs once per row and is idempotent by default; clear
the cell to re-push.

```json
{ "type": "output", "config": { "destination": "<kind>", "destination_config": { … } } }
```

## Destinations

| `destination` | `destination_config` | Credential |
|---|---|---|
| `webhook` | `url` (templated with `{Column}`), `method` (default `POST`), `headers`, `body` (template; JSON when it parses, otherwise raw text; defaults to the full row as JSON) | none |
| `crm` | `crm_type`: `hubspot` or `salesforce`; `field_map`: `{lead_field: crm_property}` | `HUBSPOT_TOKEN`, or `SALESFORCE_INSTANCE_URL` + `SALESFORCE_ACCESS_TOKEN` |
| `sheets` | `spreadsheet_id`, `range` (default `Sheet1`), `columns` (default company, email, phone, website, city) | `GOOGLE_SHEETS_TOKEN` |
| `airtable` | `base_id`, `table`, `field_map` (default Company, Email, Phone, Website, City) | `AIRTABLE_TOKEN` |
| `sequencer` | `sequence_id` of an OpenGTM outreach sequence | none |
| `instantly` | `campaign_id`, `field_map` | `INSTANTLY_API_KEY` |
| `smartlead` | `campaign_id`, `field_map` | `SMARTLEAD_API_KEY` |

Credentials resolve per workspace from **Settings → Integrations**, falling
back to `.env`.

CRM pushes send only the mapped fields and upsert by external id; a new
record is a HubSpot contact or a Salesforce lead. Sequencer pushes enrol the
row's email into the sequence and skip suppressed addresses. The cold-email
destinations default to a `{email, first_name, last_name, company_name}` map.

Audience destinations can also accept idempotent HubSpot or Salesforce
callbacks. Admins rotate a destination-bound bearer token from the Audiences
screen; only its hash is stored. Tokens expire after 90 days by default
(`OPENGTM_DESTINATION_TOKEN_TTL_DAYS`, clamped to 1–365 days), can be revoked,
and expose prefix, expiry, and last-use telemetry without revealing plaintext.
Tokens created before lifecycle enforcement must be rotated before reuse.

## Audience sequencer destinations

An audience destination adds each member to a campaign in the sequencer you
already use, with your own API key (a workspace secret under **Settings →
Integrations**). Delivery is idempotent: a lead the vendor already holds in the
campaign is reported as "Lead already in campaign", and a vendor rate limit
(HTTP 429) is retried with backoff before the member is marked failed. These
types are available to audiences and automations; they are not workbook output
columns.

| `destination_type` | `config` | Credential | Member fields used |
|---|---|---|---|
| `instantly` | `campaign_id`, `skip_if_in_campaign` | `INSTANTLY_API_KEY` | `email` |
| `smartlead` | `campaign_id`, `settings` | `SMARTLEAD_API_KEY` | `email` |
| `heyreach` | `campaign_id` (number), `linkedin_account_id` (number) | `HEYREACH_API_KEY` | `linkedin_url` (required), `first_name`, `last_name`, `company_name`, `position`, `email` |
| `lemlist` | `campaign_id` (`cam_…`) | `LEMLIST_API_KEY` | `email` (required), `first_name`, `last_name`, `company_name`, `job_title`, `linkedin_url`, `phone` |

Map your fields to these names with the destination's field map; any other
mapped field is sent as a custom field or variable. HeyReach needs the numeric
ID of the connected LinkedIn sender account as well as the campaign. Adding
members can enrol people in outbound sequences, so review the audience first.
The lead's email and LinkedIn URL are never written to logs.

## Webhook safety

Webhook URLs must be `http` or `https` without embedded credentials, and the
host is resolved and rejected if it points at loopback, private, link-local
or metadata ranges (including `169.254.169.254`). The check is repeated at
send time so a hostname cannot flip to a private address after validation.

## Automations do the same thing

The `push_crm` and `webhook` automation actions reuse this code path, so a
row can be pushed when a signal fires rather than only at the end of a run.
See [Automations](/guides/automations/).
