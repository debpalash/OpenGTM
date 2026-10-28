# Companies and People own accepted facts

Companies, People, and Employment own shared accepted facts; workbook rows and audiences reference their stable identities. Existing entity observation/history storage is retained, with explicit accepted values and a shared record-operation interface. Legacy Lead identifiers remain mapped compatibility identifiers so queued work, suppression, campaign history, and delivery receipts retain their identities during migration.

All supported write paths must reach the shared record operations, including legacy SQL writers. Read projections expose current shared facts without eagerly rewriting every workbook snapshot; this avoids write fan-out and row-lock cycles during concurrent enrichment. Source observations remain separate from accepted values, and manual edits are protected from later import replay. Migration must be idempotent, tenant-scoped, and preserve historical delivery keys.
