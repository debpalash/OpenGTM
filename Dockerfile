# ──────────────────────────────────────────────────────────────────
# OpenGTM — Production Dockerfile
# Multi-stage build: Frontend (Bun workspace) → Backend (Python 3.13)
# ──────────────────────────────────────────────────────────────────

# NOTE: the Python backend lives in legacy/python/, which is gitignored
# (untracked by design). This Dockerfile therefore only builds from a working
# tree that contains legacy/python (local `docker compose build`). A CI or
# remote build from a clean git checkout must provide legacy/python first, or
# every COPY legacy/python/... below fails.
#
# Stage 1: Build Frontend
# The repo is a Bun workspace with a single root bun.lock, so the install has
# to run from the root with every workspace manifest present for
# --frozen-lockfile to resolve. Only apps/web is built here.
FROM oven/bun:1 AS frontend-builder
WORKDIR /app
COPY package.json bun.lock ./
COPY apps/web/package.json ./apps/web/
COPY apps/web/vendor/ ./apps/web/vendor/
COPY legacy/python/apps/api/package.json ./apps/api/
COPY apps/docs/package.json ./apps/docs/
COPY packages/chrome-extension/package.json ./packages/chrome-extension/
COPY packages/n8n-nodes-yupcha/package.json ./packages/n8n-nodes-yupcha/
RUN bun install --frozen-lockfile --ignore-scripts
COPY apps/web/ ./apps/web/
RUN bun run --cwd apps/web build

# Stage 2: Backend + serve static
FROM python:3.13-slim
WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libpq-dev curl \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY legacy/python/apps/api/pyproject.toml legacy/python/apps/api/setup.py* legacy/python/apps/api/setup.cfg* ./apps/api/
COPY legacy/python/pyproject.toml* ./
RUN pip install --no-cache-dir -e ./apps/api 2>/dev/null || \
    (cd apps/api && pip install --no-cache-dir -e .) 2>/dev/null || \
    echo "No pyproject.toml install, will use requirements.txt"

# Fallback: requirements.txt
COPY legacy/python/apps/api/requirements.txt* ./apps/api/
RUN if [ -f apps/api/requirements.txt ]; then pip install --no-cache-dir -r apps/api/requirements.txt; fi

# Copy backend code
COPY legacy/python/apps/api/ ./apps/api/

# Alembic is the production schema authority. These live at the
# legacy/python/ root, so copying apps/api alone produces an image that can create tables but
# cannot apply migrations or RLS policies.
COPY legacy/python/alembic.ini ./alembic.ini
COPY legacy/python/migrations/ ./migrations/

# Copy built frontend from Stage 1
COPY --from=frontend-builder /app/apps/web/dist ./apps/web/dist

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
