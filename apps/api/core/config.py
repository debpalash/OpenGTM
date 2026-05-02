import os
from pydantic_settings import BaseSettings
from functools import lru_cache


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

    # Database
    DATABASE_URL: str = f"sqlite:///{os.path.join(DATA_DIR, 'data.db')}"

    # Optional integrations
    SCRIBD_COOKIES: str = ""
    GOOGLE_API_KEY: str = ""
    GOOGLE_CSE_ID: str = ""
    LINKEDIN_LI_AT_COOKIE: str = ""

    class Config:
        env_file = ".env"
        extra = "ignore"


@lru_cache()
def get_settings():
    return Settings()


settings = get_settings()
