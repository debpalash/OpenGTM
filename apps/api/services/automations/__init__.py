"""Signal -> Action Trigger Engine ("Automations / Recipes").

A tenant-scoped rule binds a trigger (on_signal / on_row_added / on_row_changed /
on_schedule) to a condition over a WorkbookRow and an ordered list of actions
(re_enrich / push_crm / webhook in v1). Evaluation rides the existing durable job
queue; every paid action goes through billing.check_and_debit and is idempotent.

The engine operates exclusively on the workbook/PG-RLS plane (WorkbookRow + columns).
It never reads the legacy global SQLite signal/lead/outreach stores.

See features/trigger-engine-spec.md for the authoritative design (v1 LOCKED SCOPE).
"""
