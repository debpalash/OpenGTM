# ──────────────────────────────────────────────────────────────────
# OpenGTM — Production Dockerfile
# Python 3.13 backend image. The web UI is built into the Go API image (apps/backend-go/Dockerfile).
# ──────────────────────────────────────────────────────────────────

# NOTE: the Python backend lives in legacy/python/, which is gitignored
# (untracked by design). This Dockerfile therefore only builds from a working
# tree that contains legacy/python (local `docker compose build`). A CI or
# remote build from a clean git checkout must provide legacy/python first, or
# every COPY legacy/python/... below fails.
#
# Stage 0: the ONLY stage that reads legacy/python from the build context. It fails
# fast, with an obvious message, when legacy/python is absent (the bracket globs
# match nothing instead of erroring on a missing path, so the RUN can explain;
# .dockerignore keeps .git/.venv/data/tests out of this copy).
# Every later stage takes Python sources `COPY --from=legacy-src`, so BuildKit
# cannot report a cryptic "not found" from a parallel branch first.
FROM busybox:1.37 AS legacy-src
COPY legac[y]/pytho[n]/ /legacy/
RUN test -f /legacy/pyproject.toml && test -f /legacy/alembic.ini \
    && test -d /legacy/migrations && test -f /legacy/apps/api/main.py || { \
      echo "" >&2; \
      echo "ERROR: legacy/python is missing from the build context." >&2; \
      echo "  The Python backend lives in legacy/python, which is its own git repo and is" >&2; \
      echo "  gitignored by this repo. Clone it to ./legacy/python before building the" >&2; \
      echo "  Python images (api, worker, scheduler, migrate, seed)." >&2; \
      echo "  See docs/go-port/runbooks/docker-compose.md." >&2; \
      echo "" >&2; \
      exit 1; }

# Backend (the UI is served by api-go; nothing frontend is built here)
FROM python:3.13-slim
WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libpq-dev curl \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY --from=legacy-src /legacy/apps/api/pyproject.toml /legacy/apps/api/setup.py* /legacy/apps/api/setup.cfg* ./apps/api/
COPY --from=legacy-src /legacy/pyproject.toml ./
RUN pip install --no-cache-dir -e ./apps/api 2>/dev/null || \
    (cd apps/api && pip install --no-cache-dir -e .) 2>/dev/null || \
    echo "No pyproject.toml install, will use requirements.txt"

# Fallback: requirements.txt
COPY --from=legacy-src /legacy/apps/api/requirements.txt* ./apps/api/
RUN if [ -f apps/api/requirements.txt ]; then pip install --no-cache-dir -r apps/api/requirements.txt; fi

# Copy backend code
COPY --from=legacy-src /legacy/apps/api/ ./apps/api/

# Alembic is the production schema authority. These live at the
# legacy/python/ root, so copying apps/api alone produces an image that can create tables but
# cannot apply migrations or RLS policies.
COPY --from=legacy-src /legacy/alembic.ini ./alembic.ini
COPY --from=legacy-src /legacy/migrations/ ./migrations/

# Set environment
ENV PYTHONPATH=/app
ENV ROOT_DIR=/app
# NOTE: Do NOT hardcode a SQLite DB_PATH here — it would shadow the documented
# Postgres default. The database is selected via DATABASE_URL (see
# docker-compose.yml / .env.example). Legacy code that still reads DB_PATH falls
# back to its own default for the sqlite meta files under /app/data.

# Create data directory
RUN mkdir -p /app/data

# Health check
HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
  CMD curl -f http://localhost:8000/health || exit 1

EXPOSE 8000

CMD ["uvicorn", "apps.api.main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "2"]
