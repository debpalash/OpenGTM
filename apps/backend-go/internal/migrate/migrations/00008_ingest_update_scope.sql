-- +goose Up
-- Existing ingest tokens remain append-only. Editors may grant a replacement token a fixed
-- set of row fields it may refresh when a duplicate company is found.
ALTER TABLE public.workbook_ingest_tokens
  ADD COLUMN allowed_update_fields text[] NOT NULL DEFAULT '{}';
