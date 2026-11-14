# Precleared compliance check in the Go backend

`precleared_compliance` is a declarative provider manifest
(`apps/backend-go/internal/manifest/builtin/manifests/compliance/precleared.yaml`)
that asks [Precleared](https://precleared.dev) whether a cold or marketing email
may be sent to one recipient. It covers UK PECR and UK GDPR, US CAN-SPAM, Canada's
CASL, Germany's UWG, France's CNIL guidance, Australia's Spam Act, Ireland's
S.I. 336/2011 and the Dutch Telecommunicatiewet. The answer is decision support
with sources, not legal advice and not a guarantee.

It is opt-in and inert by default. With no key configured it makes no requests,
including to Precleared's keyless free tier, and nothing else in OpenGTM changes.

## Turn it on for a workspace

1. Save the workspace's own key and context. Every `PRECLEARED_*` setting is
   workspace-owned, so a workspace is never enrolled by an operator-wide value:

   ```bash
   curl -X PUT "$OPENGTM/api/settings/workspace-integrations/precleared?workspace_id=$WS" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"values": {
           "PRECLEARED_API_KEY": "pc_live_...",
           "PRECLEARED_SENDER_COUNTRY": "GB",
           "PRECLEARED_ADDRESS_SOURCE": "published",
           "PRECLEARED_PITCH": "Bookkeeping software for small firms"}}'
   ```

   A single-tenant self-host can instead save the values once under
   Settings, Integrations, Precleared and set `ALLOW_GLOBAL_CREDENTIAL_FALLBACK=true`,
   the same switch that lets CRM and outreach integrations use global credentials.

2. Add an enrichment column with the provider `precleared_compliance`. Name it
   `compliance`, or set its target field to `compliance`, so the cell shows the
   verdict. No default waterfall includes this provider.

## What it sends

One `POST https://precleared.dev/api/v1/check?src=opengtm` per row, with the key
only in a bearer `Authorization` header. The JSON body has five fields:

| Field | From |
|---|---|
| `recipient_email` | the lead's `email` |
| `company_name` | the lead's `company` |
| `sender_country` | `PRECLEARED_SENDER_COUNTRY` (GB, US or AE) |
| `address_source` | `PRECLEARED_ADDRESS_SOURCE` (published, bought, scraped, given or unknown) |
| `pitch` | `PRECLEARED_PITCH` |

Nothing is inferred. An unset value is sent blank, which Precleared reads as not
given: a blank `address_source` becomes `unknown`, the cautious value, and a
blank `sender_country` is recorded as Precleared's documented default, GB. The
manifest never supplies a recipient country, consent or existing-customer fact;
Precleared works the country out from the domain, company records and the
company's website.

## What it returns

| Field | Meaning |
|---|---|
| `compliance` | Verdict: `ok_to_email`, `consent_needed`, `blocked` or `unknown` |
| `compliance_confidence` | `high`, `medium` or `low` |
| `compliance_rule` | Key of the rule that decided the verdict |
| `compliance_rule_source` | URL of that rule's legal source |
| `compliance_reason` | The rule's plain-English reason |
| `compliance_note` | First note, for example why a verdict is `unknown` |
| `compliance_next_step` | What to do next |
| `compliance_footer` | What the email's footer must include, when sending is allowed |
| `compliance_recipient_country` | Country whose rules applied |
| `compliance_receipt_id` | Signed receipt id |
| `compliance_receipt_url` | Where anyone can fetch and verify the receipt |
| `compliance_checked_at` | When the check ran (the receipt's timestamp) |
| `compliance_rules_version` | Version of the rule that applied |
| `compliance_disclaimer` | Precleared's decision-support notice |

`unknown` is a successful but uncertain answer. Treat it, like `consent_needed`,
as "do not send yet". This manifest only records the verdict; holding sends is a
separate, optional send gate.

## Cost and limits

Precleared bills the workspace's own key: a free daily allowance, then the key's
plan or prepaid packs (2,000 checks for 39 GBP at pack prices). `cost_per_lookup`
(0.03) is the worst case OpenGTM uses for spend estimates. A 400, 401, 402 or 429
runs no check. Each key may make 60 checks a minute.

## Errors

| Result error | Cause |
|---|---|
| `precleared_compliance: missing PRECLEARED_API_KEY` | No key saved for the workspace; no request was made |
| `http_400` | Invalid input, for example a lead without an email address |
| `http_401` | The key is wrong or revoked |
| `http_402` | The key's allowance and credits are used up |
| `http_429` | Rate limit reached |
| `http_5xx` | Precleared could not complete the check |
| `timeout` | No answer within 25 seconds |
| `non_json_response` | The response was not JSON or exceeded 1 MiB |

Error bodies are not parsed, so Precleared's payment links never reach a lead or
a workbook.

## Security and behavior

- This is an outbound call to `precleared.dev` carrying the lead's email address
  and company name and the workspace's declared context. It goes through the same
  egress guard and SSRF checks as every declarative provider; the URL is fixed and
  never templated.
- The key and settings resolve through `secrets.Resolver` like every manifest
  credential, but stop at the workspace level (`secrets.WorkspaceOwned`) unless
  `ALLOW_GLOBAL_CREDENTIAL_FALLBACK` is set. Credential values are scrubbed from
  error text.
- Compliance verdicts are never served from, or written to, the shared enrichment
  cache, which is keyed per company or record: each row is checked fresh.
- Precleared does not store the recipient name (none is sent here). Its terms and
  privacy notice are at https://precleared.dev/terms and https://precleared.dev/privacy.

Fixture tests (`internal/manifest/builtin/precleared_test.go`) cover the request
shape and the success, consent-needed, uncertain, invalid-input, auth, quota,
rate-limit, server-error, malformed-response, network-failure and timeout
outcomes, and that no request is made without a key. Resolver and provider-run
tests cover workspace-owned settings and the cache exclusion. None of them call
Precleared.
