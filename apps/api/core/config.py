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
