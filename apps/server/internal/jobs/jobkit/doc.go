// Package jobkit holds the pieces shared by the Go executors of migrated
// Python job types (retention_enforce, research_playbook_schedule,
// audience_refresh, ...), so each port stays a small, reviewable translation
// of its Python handler rather than a copy of the same plumbing:
//
//   - Python value semantics for decoded JSON (truthiness, str(), int(),
//     repr(), exception text) used wherever a handler must reproduce Python's
//     behaviour on malformed stored data;
//   - tenant transactions fenced by the job lease;
//   - the scheduling idioms every recurring job uses: cancelling pending
//     occurrences by fire_key pattern, clamped intervals, the non-RLS
//     "schedule mirror" upsert, Python's datetime.isoformat() for fire keys;
//   - keyset batching;
//   - Python-owned-by-default registration (Declare).
//
// The Python code remains the source of truth for every executor; the helpers
// here only reproduce its observable effects.
package jobkit
