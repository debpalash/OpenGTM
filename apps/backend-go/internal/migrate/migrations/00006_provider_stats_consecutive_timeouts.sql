-- +goose Up
-- One timeout used to cool a provider down for 180 s for every row. The cooldown now needs a run of consecutive
-- timeouts for the same provider and field; this is the run length (reset by any success or miss).
ALTER TABLE public.provider_stats ADD COLUMN consecutive_timeouts integer NOT NULL DEFAULT 0;
