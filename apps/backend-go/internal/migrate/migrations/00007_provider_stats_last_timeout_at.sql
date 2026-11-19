-- +goose Up
-- Calls launched together (twelve rows at once) that all time out are one event, not twelve. The run of consecutive
-- timeouts only grows for a call that started after the previous counted timeout finished; this is that finish time,
-- NULL when the run is empty.
ALTER TABLE public.provider_stats ADD COLUMN last_timeout_at timestamp without time zone;
