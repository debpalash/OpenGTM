# ──────────────────────────────────────────────────────────────────
# Lead Data — Production Dockerfile
# Multi-stage build: Frontend (Bun) → Backend (Python 3.13)
# ──────────────────────────────────────────────────────────────────

# Stage 1: Build Frontend
FROM oven/bun:1 AS frontend-builder
WORKDIR /app
COPY apps/web/package.json apps/web/bun.lock* ./apps/web/
RUN cd apps/web && bun install --frozen-lockfile
COPY apps/web/ ./apps/web/
RUN cd apps/web && bun run build

# Stage 2: Backend + serve static
FROM python:3.13-slim
WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libpq-dev curl \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY apps/api/pyproject.toml apps/api/setup.py* apps/api/setup.cfg* ./apps/api/
COPY pyproject.toml* ./
RUN pip install --no-cache-dir -e ./apps/api 2>/dev/null || \
    (cd apps/api && pip install --no-cache-dir -e .) 2>/dev/null || \
    echo "No pyproject.toml install, will use requirements.txt"

# Fallback: requirements.txt
COPY apps/api/requirements.txt* ./apps/api/
RUN if [ -f apps/api/requirements.txt ]; then pip install --no-cache-dir -r apps/api/requirements.txt; fi

# Copy backend code
COPY apps/api/ ./apps/api/

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
