import os
from pydantic_settings import BaseSettings
from functools import lru_cache

# Repo root, resolved from this file's location so it's independent of the
# process CWD (the API is launched from apps/api, scripts from the root, etc.).
_ROOT_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../../"))
# Load the root .env regardless of CWD. A sibling .env.local (gitignored) wins
# for per-developer overrides without touching the shared file.
_ENV_FILES = (
    os.path.join(_ROOT_DIR, ".env"),
    os.path.join(_ROOT_DIR, ".env.local"),
)


# Sentinel value for the never-safe-in-prod default signing key. Code that needs
# to detect "the operator never set a real key" matches on this substring.
INSECURE_DEFAULT_SECRET_KEY = "INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY"

# Environments where the insecure default SECRET_KEY is tolerated (boot anyway).
# Anything not in this set is treated as a real deployment and fails closed.
_DEV_ENVS = {"dev", "development", "test", "testing", "local"}


class Settings(BaseSettings):
    PROJECT_NAME: str = "Yupcha Engine"
    SECRET_KEY: str = INSECURE_DEFAULT_SECRET_KEY
    ALGORITHM: str = "HS256"
    # Access tokens are short-lived (default 30 min) so a leaked token has a
    # small blast radius. Long-lived sessions are carried by refresh tokens
    # (see REFRESH_TOKEN_EXPIRE_MINUTES).
    ACCESS_TOKEN_EXPIRE_MINUTES: int = 30
    # Refresh tokens are longer-lived (default 14 days). They carry a
    # "type": "refresh" claim and only mint new access tokens — they are not
    # accepted as access tokens themselves.
    REFRESH_TOKEN_EXPIRE_MINUTES: int = 60 * 24 * 14

    # Deployment environment. Values in _DEV_ENVS (dev/test/local/...) relax the
    # SECRET_KEY fail-closed check so the insecure default still boots locally.
    # Any other value ("prod", "production", "staging", …) refuses to boot when
    # the SECRET_KEY is still the insecure default.
    APP_ENV: str = "dev"

    # Server
    API_HOST: str = "0.0.0.0"
    API_PORT: int = 8000
    CORS_ORIGINS: str = "*"
    DEBUG: bool = False

    # Paths
    BASE_DIR: str = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ROOT_DIR: str = os.path.abspath(os.path.join(BASE_DIR, "../../"))
    DATA_DIR: str = os.path.join(ROOT_DIR, "data")

    # Database — Postgres (MVCC, real concurrent writers). The real connection
    # string (host/user/password/db) MUST come from the DATABASE_URL env var /
    # .env — never hardcode credentials here. This default is a credential-free
    # local fallback: libpq fills the user from PGUSER/$USER and uses the local
    # socket, so it works for a stock local Postgres without baking in a username.
    DATABASE_URL: str = "postgresql+psycopg://localhost:5432/yupcha"

    # Envelope-encryption master key for per-workspace integration secrets
    # (spec WI-6). A urlsafe-base64 32-byte Fernet key. When unset we DERIVE a
    # key from SECRET_KEY so dev/test work out of the box; production must set a
    # real SECRETS_MASTER_KEY (see services/workspace/secrets.py, which fails
    # closed when this is empty AND SECRET_KEY is the insecure default).
    SECRETS_MASTER_KEY: str = ""

    # ── Billing / credit ledger (WI-9) ─────────────────────────────────
    # Master switch. OFF by default so self-host deployments are unaffected:
    # when disabled, runs are NEVER blocked and no debits happen. Operators who
    # want platform billing set BILLING_ENABLED=true.
    BILLING_ENABLED: bool = False
    # Stripe keys for credit top-ups. Read from env/.env; never hardcode.
    STRIPE_SECRET_KEY: str = ""
    STRIPE_WEBHOOK_SECRET: str = ""

    # ── Postgres multi-tenant leads/signals store (RLS) ─────────────────
    # When DATABASE_URL is Postgres, leads/signals live in ONE shared,
    # RLS-protected table instead of per-workspace SQLite files. This is the
    # default on Postgres; set to False to force the legacy SQLite path even on
    # Postgres (escape hatch). Ignored on SQLite (always uses LeadDB).
    PG_LEAD_STORE: bool = True
    # The non-superuser, non-BYPASSRLS role the runtime should connect as for the
    # PG store. Must match the role created by the tenancy Alembic migration. At
    # startup we verify current_user is NOT superuser/BYPASSRLS (RLS is silently
    # inert otherwise); see services/leadgen/store.py:assert_rls_role.
    APP_DB_ROLE: str = "yupcha_app"
    # If True, REFUSE TO BOOT when the PG store is active but the connection role
    # is superuser/BYPASSRLS (RLS would be off). If False, log CRITICAL and fall
    # back to disabling the PG store. Default True = fail fast (recommended).
    PG_RLS_REQUIRE_SAFE_ROLE: bool = True

    # ── Automations / Trigger Engine (Signal->Action) ──────────────────
    # Master switch. OFF: router 404s, event emitters no-op, the trigger_eval
    # handler early-exits. Default OFF so hot paths are untouched until enabled.
    AUTOMATIONS_ENABLED: bool = False
    # Gates the legacy sequencer / send_email action types (global, unscoped
    # outreach.db). DEFAULT OFF and, per the v1 LOCKED SCOPE, those action types
    # are rejected at rule-create regardless until outreach.db is RLS-hardened.
    AUTOMATIONS_ALLOW_LEGACY_OUTREACH: bool = False
    # Workspace-global daily automations spend cap in USD (0 = unlimited).
    AUTOMATIONS_GLOBAL_DAILY_USD: float = 0.0
    AUTOMATIONS_MAX_RULES_PER_WS: int = 50
    AUTOMATIONS_MAX_ACTIONS_PER_RULE: int = 10
    AUTOMATIONS_MAX_ROWS_PER_EVAL: int = 500
    # Optional egress allowlist for webhook actions (empty = any public host).
    # Cloud deployments may populate this; comma-separated when set via env.
    AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST: list[str] = []

    # ── Outreach (RLS-hardened email sending) ───────────────────────────
    # Platform-billed cost per metered send (USD). Default 0.0 → free on
    # self-host (caps/billing bypassed; the in_flight marker is still written).
    OUTREACH_SEND_COST_USD: float = 0.0
    # Autonomous ticker interval (seconds). Default 15 min.
    OUTREACH_TICK_INTERVAL: int = 900
    # Per-tick enqueue cap (bounds throughput against a sequential worker).
    OUTREACH_TICK_MAX_ENQUEUE: int = 200
    # Unsubscribe HMAC token TTL (days). Tokens older than this reject on POST.
    OUTREACH_UNSUB_TTL_DAYS: int = 90
    # Async soft-bounce threshold → suppress + terminal enrollment.
    OUTREACH_SOFT_BOUNCE_MAX: int = 3
    # Circuit-breaker thresholds (LOCKED SCOPE decision 2): auto-pause a
    # sequence at >5% bounce OR >0.3% complaint (rates over sent volume).
    OUTREACH_BOUNCE_PAUSE_RATE: float = 0.05
    OUTREACH_COMPLAINT_PAUSE_RATE: float = 0.003
    # Minimum sent volume before the circuit breaker can trip (avoid pausing on
    # tiny samples where a single bounce is >5%).
    OUTREACH_CIRCUIT_MIN_SENDS: int = 20
    # Public base URL used to build the unsubscribe one-click link.
    OUTREACH_PUBLIC_BASE_URL: str = "http://localhost:8000"
    # Platform-global bounce/complaint webhook shared secret (cloud).
    OUTREACH_BOUNCE_WEBHOOK_SECRET: str = ""

    # ── Scheduled Intent-Signal Poller (v1) ────────────────────────────
    # Master switch. OFF: /api/watches router 404s, handle_watch_poll early-
    # exits, bootstrap_watch_schedules no-ops. PG-only (requires use_pg_store()).
    INTENT_POLLER_ENABLED: bool = False
    INTENT_POLLER_DEFAULT_INTERVAL: str = "daily"   # daily | hourly | weekly
    INTENT_POLLER_MAX_WATCHES_PER_WS: int = 200
    # Scheduled-poll daily budget per (ws, UTC-day). 0 = unlimited. Manual
    # poll-now counts against this same ledger (closes the bypass, spec §7).
    INTENT_POLLER_DAILY_POLL_BUDGET: int = 0
    # Poll-now has its OWN small per-(ws, UTC-day) quota (separate ledger key)
    # plus a per-watch rate limit. 0 = unlimited.
    INTENT_POLLER_POLL_NOW_DAILY_QUOTA: int = 50
    INTENT_POLLER_POLL_NOW_MIN_INTERVAL_SEC: int = 60
    # Hard auto-disable after this many consecutive poll failures (spec §9.16).
    INTENT_POLLER_MAX_CONSECUTIVE_FAILURES: int = 12
    # JobSpy DDG-snippet cap (band fidelity vs cost, spec §7).
    INTENT_POLLER_JOBSPY_MAX_JOBS: int = 5
    # RSS feed entry cap per poll (spec §9.12).
    INTENT_POLLER_FEED_MAX_ENTRIES: int = 100
    # Fan-out advisory bound for on_signal fires per signal (spec §7, AC-18).
    INTENT_POLLER_MAX_FIRES_PER_SIGNAL: int = 200
    # Whether the FIRST (bootstrap) poll emits signals for pre-existing items.
    # Default False: bootstrap records state, suppresses emission (spec §8.4).
    INTENT_POLLER_BACKFILL: bool = False
    # SEC EDGAR descriptive User-Agent (SEC 403s requests without one).
    SEC_EDGAR_USER_AGENT: str = "Yupcha Enrichment admin@yupcha.com"
    # Per-fetch platform-billed cost (USD). Free sources stay 0.0 → debit no-op.
    POLLER_FUNDING_COST_USD: float = 0.0
    POLLER_HIRING_COST_USD: float = 0.0
    POLLER_FEED_COST_USD: float = 0.0

    # Optional integrations
    SCRIBD_COOKIES: str = ""
    GOOGLE_API_KEY: str = ""
    GOOGLE_CSE_ID: str = ""
    LINKEDIN_LI_AT_COOKIE: str = ""

    class Config:
        env_file = _ENV_FILES
        env_file_encoding = "utf-8"
        extra = "ignore"

    # ── Security helpers ────────────────────────────────────────────────
    @property
    def is_dev_env(self) -> bool:
        """True when running in a dev/test/local environment (insecure default
        SECRET_KEY tolerated)."""
        return (self.APP_ENV or "").strip().lower() in _DEV_ENVS

    @property
    def secret_key_is_insecure(self) -> bool:
        """True when SECRET_KEY is still the shipped insecure default."""
        return INSECURE_DEFAULT_SECRET_KEY in (self.SECRET_KEY or "")

    def validate_security(self) -> None:
        """Fail closed: refuse to run a real deployment with the insecure
        default SECRET_KEY. Dev/test/local environments are allowed to boot
        (with a warning emitted by the caller) for ergonomics."""
        if self.secret_key_is_insecure and not self.is_dev_env:
            raise RuntimeError(
                "Refusing to start: SECRET_KEY is the insecure default while "
                f"APP_ENV={self.APP_ENV!r}. Set a strong SECRET_KEY in the "
                "environment/.env, or set APP_ENV to one of "
                f"{sorted(_DEV_ENVS)} for local development."
            )


@lru_cache()
def get_settings():
    s = Settings()
    # Fail closed at construction time so any code path that imports settings in
    # a real deployment with the insecure default key blows up immediately.
    s.validate_security()
    return s


settings = get_settings()
