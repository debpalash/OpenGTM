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


class Settings(BaseSettings):
    PROJECT_NAME: str = "Yupcha Engine"
    SECRET_KEY: str = "INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY"
    ALGORITHM: str = "HS256"
    ACCESS_TOKEN_EXPIRE_MINUTES: int = 2800

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


@lru_cache()
def get_settings():
    return Settings()


settings = get_settings()
