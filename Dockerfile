# Stage 1: Build Frontend
FROM node:18-alpine AS frontend-builder
WORKDIR /app/frontend
COPY apps/web/package.json apps/web/package-lock.json* ./
RUN npm install
COPY apps/web/ ./
RUN npm run build

# Stage 2: Backend
FROM python:3.11-slim
WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y \
    gcc \
    libpq-dev \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY apps/api/requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# Copy Backend Code
COPY apps/api ./apps/api

# Copy Built Frontend from Stage 1
COPY --from=frontend-builder /app/frontend/dist ./apps/web/dist

# Set Environment Variables
ENV PYTHONPATH=/app
ENV ROOT_DIR=/app
ENV DB_PATH=/app/data/scribd.db

# Create data directory
RUN mkdir -p /app/data

# Expose Port
EXPOSE 8000

# Run API
CMD ["uvicorn", "apps.api.api:app", "--host", "0.0.0.0", "--port", "8000"]
