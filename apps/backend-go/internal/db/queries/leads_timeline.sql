-- Unified lead timeline sources (port of lead_timeline in routers/leads.py).
-- All tables are FORCE RLS: run inside tenancy.InWorkspace. Each source is capped
-- at 200 rows like Python; the explicit workspace filter mirrors the ORM filter.

-- name: TimelineSignals :many
SELECT id, signal_type, title, description, source, source_url, weight, created_at
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND lead_id = @lead_id::int4
ORDER BY created_at DESC, id DESC
LIMIT 200;

-- name: TimelineAudienceEvents :many
SELECT e.id, e.event_type, e.audience_id, a.name AS audience_name, e.created_at
FROM audience_membership_events e
JOIN audiences a ON a.id = e.audience_id
WHERE e.workspace_id = current_setting('app.workspace_id', true)
  AND e.lead_id = @lead_id::int4
ORDER BY e.created_at DESC, e.id DESC
LIMIT 200;

-- name: TimelineDeliveries :many
SELECT d.id, d.destination_id, d.operation, d.status, d.summary, d.error,
       d.delivered_at, d.updated_at, x.name AS destination_name, x.destination_type
FROM destination_deliveries d
JOIN audience_destinations x ON x.id = d.destination_id
WHERE d.workspace_id = current_setting('app.workspace_id', true)
  AND d.lead_id = @lead_id::int4
ORDER BY d.updated_at DESC, d.id DESC
LIMIT 200;

-- name: TimelineOutreachSends :many
SELECT id, sequence_id, step_number, subject, status,
       replied_at, opened_at, bounced_at, sent_at, created_at
FROM outreach_sends
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND lead_id = @lead_id::int4
ORDER BY created_at DESC, id DESC
LIMIT 200;
