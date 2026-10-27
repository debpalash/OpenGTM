# syntax=docker/dockerfile:1.7
# OpenGTM browser service: Playwright Chromium (headless shell) + scraper libs.
# Internal only: compose attaches it to browser_net (internal) + browser_egress,
# never publishes a port. Runs as an unprivileged user; refuses to start
# without BROWSER_SERVICE_TOKEN.
FROM python:3.13-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/app/.venv

COPY --from=ghcr.io/astral-sh/uv:0.10.7 /uv /usr/local/bin/uv

WORKDIR /app
COPY pyproject.toml uv.lock .python-version ./
RUN uv sync --frozen --no-dev --no-install-project
COPY src ./src
RUN uv sync --frozen --no-dev \
 && /app/.venv/bin/playwright install --with-deps --only-shell chromium \
 && rm -rf /var/lib/apt/lists/* /root/.cache \
 && useradd --system --uid 10001 --create-home --home-dir /home/browser browser

USER 10001
ENV PATH=/app/.venv/bin:$PATH \
    BROWSER_SERVICE_HOST=0.0.0.0 \
    BROWSER_SERVICE_PORT=8090
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/healthz', timeout=3)"]
ENTRYPOINT ["python", "-m", "browser_service"]
