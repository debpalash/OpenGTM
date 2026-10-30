# Move company and people data from Clay

OpenGTM imports Clay table data through CSV or a JSON array. Companies and People have separate identities and reusable enrichment workflows. A company can exist before any people are discovered. A person's identity and previous employment survive a change of employer.

## Import

1. In Clay, expose the columns you want to carry across and download the table as CSV. Export company and people tables separately when possible.
2. Open **Companies → Import from Clay**. Upload the company file and give the source a stable table name, such as `clay:target-accounts`.
3. Click **Preview & validate**. Review the suggested mappings and row errors. Change mappings and preview again before importing.
4. Import the people file from **People**. Map company domains or existing company IDs to connect people to the imported companies.
5. Open a record to review accepted fields, source identities, observations, and employment history.

Use the same source table name on later updates. `Row ID` identifies a row within that source; it does not identify the same person across unrelated Clay tables. Email, LinkedIn profiles, and company domains provide stronger identity evidence. Conflicting identifiers are rejected for review rather than silently combining records.

| Common Clay column | Company import | People import |
| --- | --- | --- |
| Company Name | `company.name` | `company.name` |
| Company Domain / Domain | `company.domain` | `company.domain` |
| Company ID | `company_id` | `company_id` |
| Full Name | Custom company field | `person.full_name` |
| Work Email | Custom company field | `person.email` |
| LinkedIn URL | `company.linkedin_url` | `person.linkedin_url` |
| Job Title | Custom company field | `employment.title` |
| Row ID | `external_id` | `external_id` |

Unknown columns remain custom fields. Explicit `company.*`, `person.*`, and `employment.*` mappings select their owner. Clear a mapping to ignore a column. JSON objects and arrays in cells, Unicode, multiline CSV values, and large numeric identifiers are preserved. A role can be stored before the employer is known.

Imports support 10,000 rows and 256 columns per batch. The browser accepts files up to 15 MB. Split larger Clay exports into batches; use a distinct batch key for each API import. A repeated API request with the same key and payload returns the stored result. Reusing that key for different data returns a conflict. Each rejected row rolls back its own changes; accepted rows commit together. Download the row report to correct failures.

## Enrich and activate

Configure **Enrichment workflow** separately on Companies and People. Select records and choose **Enrich selected** to create a linked workbook and run it through the existing durable worker. Accepted changes appear in other linked workbooks and audience refreshes. Manual accepted edits take precedence over ordinary imported/provider observations.

On Companies, **Find people** builds a people-search workbook from the selected companies. Search providers require their normal configuration. **Monitor companies** works without a legacy Lead or a discovered person. Both tables can create audiences. People can enroll in outreach with the existing consent, suppression, and send deduplication rules.

## Export and restore

**Export** downloads CSV with stable identity columns and custom values encoded as JSON where needed. It can be mapped into Clay's CSV source or reimported into the same OpenGTM workspace. OpenGTM IDs are local workspace references; when moving a CSV to another workspace, ignore/remap those ID columns and link records by domain/email/source IDs.

**Backup JSON** includes both Companies and People, employment history, accepted facts, observations, source identifiers, and saved record workflows. **Restore backup** validates and restores that file into an empty workspace, remapping internal IDs. Browser restores accept files up to 100 MB. This record backup does not include workbook executions, audience/campaign definitions, credentials, or delivery history; use a database backup for a full installation backup.

Clay's CSV export cannot provide hidden object contents, skipped results, provider credentials, credit balances, or executable Clay workflow definitions that were not exported. The migration imports the data supplied in the file. It does not claim access to a private Clay API or automatic workflow conversion.

## Local preview

The implementation is on branch `feat/company-person-clay-migration` in `OpenGTM-entities`. Open `http://localhost:8002`. Login details are in the worktree's private `.opengtm-initial-credentials` file. This instance has its own database and Redis port; the Python instance on 8000 and original Go PR instance on 8001 are separate.

After building the UI and Go binaries, `scripts/run-records-local.sh` starts the API, worker, and scheduler using the worktree's `.env`. Production upgrades should take a database backup and run `opengtm-migrate up` with owner credentials before starting schema-version-5 binaries. Runtime connections use a restricted RLS role.
