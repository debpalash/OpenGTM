# Shared records and Clay data migration

Implements issue #20 on the Go backend. A Company is valid without a Lead or any Person. People have independent identities and employment histories. Workbooks reference shared records; accepted edits and enrichment become visible through other workbooks, audiences, monitoring, and activation.

## Acceptance matrix

- [x] Company/Person records and field ownership with employment history, observations, and protected accepted edits.
- [x] Company-only import and warehouse before person discovery.
- [x] Multiple people at one company, stable person identity after employer changes.
- [x] Companies and People navigation, browsable tables, detail/edit screens.
- [x] Independently configurable company/person enrichment workflows using the durable worker.
- [x] Workbook references and shared accepted values across edits, imports, provider writeback, and callbacks.
- [x] Explicit Company/Person audience membership and activation subjects.
- [x] Company-only signals and monitoring independent of Lead availability.
- [x] Person outreach with unchanged suppression and historical send/delivery deduplication.
- [x] Idempotent migration of Lead identities, existing workbooks, queued work, and campaign history.
- [x] Clay CSV/JSON mapping preview and import with source-scoped row IDs, custom/nested field preservation, linked companies/people, replay safety, and per-row rejection details.
- [x] Clay-friendly CSV export and lossless OpenGTM JSON export, including stable IDs and relationships.
- [x] Tenant isolation, identity conflicts, concurrent writers, rollback/failure cases, and representative round-trip/large-import tests.

## Interchange contract

Clay exports CSV with user-defined columns and supports CSV mapping and webhook sources. Stable headers are suggested presets rather than a universal Clay schema. An import accepts explicit mappings, otherwise suggests common company/person aliases. Unmapped columns and JSON-like cell values are preserved. Source table and row IDs are namespaced; they never become globally unique identifiers by assumption. Company links use explicit OpenGTM/external company IDs or normalized company domains before weak names.

A CSV cannot recover object values or workflow execution configuration that Clay did not include in its export. The import preview reports missing identifiers and unsupported/ambiguous values rather than inventing them. Imported data does not imply imported provider credentials, credit balances, or executable Clay workflow definitions. JSON export is versioned and preserves OpenGTM records, custom fields, identity mappings, observations, and employment relationships for faithful restore.

Official references:
- https://university.clay.com/lessons/download-as-a-csv-clay-101
- https://university.clay.com/docs/sources
- https://university.clay.com/docs/lookup-rows
- https://university.clay.com/fr/lessons/audiences-workbooks-and-tables-explained

Validation and measured limits are recorded in [the local test report](../testing/company-person-clay-migration.md). User instructions are in [the migration guide](../clay-migration.md).
