"""Entry point: ``python -m browser_service`` (refuses to start without a token)."""

from __future__ import annotations

import logging
import sys

from .config import ConfigError, Settings


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    try:
        settings = Settings.from_env()
    except ConfigError as e:
        print(f"browser-service: refusing to start: {e}", file=sys.stderr)
        sys.exit(2)

    import uvicorn

    from .app import create_app

    uvicorn.run(
        create_app(settings),
        host=settings.host,
        port=settings.port,
        proxy_headers=False,
        server_header=False,
        date_header=False,
        limit_concurrency=settings.max_concurrency * 4 + 16,
        timeout_graceful_shutdown=10,
        log_config=None,
    )


if __name__ == "__main__":
    main()
