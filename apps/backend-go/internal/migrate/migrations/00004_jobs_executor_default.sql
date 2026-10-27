-- +goose Up
-- Plan 15 WP-CUTOVER: the Go worker is the only executor, so a job row inserted without an explicit executor is a
-- Go job. Historical 'python' rows stay valid (the check constraint keeps both values); the cutover runbook
-- reassigns the pending ones.
ALTER TABLE public.jobs ALTER COLUMN executor SET DEFAULT 'go';
