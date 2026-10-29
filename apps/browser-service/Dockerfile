# syntax=docker/dockerfile:1.7
# OpenGTM browser service: Playwright Chromium (headless shell) + scraper libs.
# Run with --security-opt seccomp=seccomp-chromium.json (Chromium sandbox).
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
COPY scripts/entrypoint.sh /app/scripts/entrypoint.sh
RUN uv sync --frozen --no-dev \
 && /app/.venv/bin/playwright install --with-deps --only-shell chromium \
 && /app/.venv/bin/patchright install --only-shell chromium \
 && rm -rf /var/lib/apt/lists/* /root/.cache \
 && useradd --system --uid 10001 --create-home --home-dir /home/browser browser \
 && useradd --system --uid 10002 --no-create-home --home-dir /tmp chromium

USER 10001
ENV PATH=/app/.venv/bin:$PATH \
    BROWSER_SERVICE_HOST=0.0.0.0 \
    BROWSER_SERVICE_PORT=8090
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/healthz', timeout=3)"]
# uid 10001: service + Chromium (sandboxed, token-free env). Started as root with
# only SETUID/SETGID: split mode, Chromium as uid 10002 (see scripts/entrypoint.sh).
# Chromium's sandbox needs seccomp-chromium.json (user namespaces).
ENTRYPOINT ["/app/scripts/entrypoint.sh"]
