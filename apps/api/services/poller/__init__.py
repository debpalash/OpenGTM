"""Scheduled Intent-Signal Poller (v1).

A durable ``watch_poll`` job type that periodically pulls fresh buying signals
(funding / hiring band-shift / tech adoption / executive appointments / RSS news)
for the companies each workspace tracks, and writes deduped, workspace-stamped
rows into the shared PG ``signals`` table via ``PgLeadStore.add_signal`` (which
fires ``on_signal`` automations rules). Default OFF, PG-only.

See ``docs/specs/intent-signal-poller-spec.md``.
"""
