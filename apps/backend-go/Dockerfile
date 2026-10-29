# syntax=docker/dockerfile:1
# OpenGTM Go backend: one multi-stage build, one target per binary.
#
#   docker build --target api       -t opengtm-api-go:local       --build-context web=apps/web apps/backend-go
#   docker build --target worker    -t opengtm-worker-go:local    apps/backend-go
#   docker build --target mcp       -t opengtm-mcp-go:local       apps/backend-go
#   docker build --target scheduler -t opengtm-scheduler-go:local apps/backend-go   (also carries /opengtm-seed)
#   docker build --target migrate   -t opengtm-migrate-go:local   apps/backend-go
#
# The api image also carries /opengtm-migrate (schema migrations), so a single image can run both.
#
# The api image embeds the web UI: the `web` named build context (compose: additional_contexts,
# ./apps/web) is built with Bun and baked in via the `webui` build tag. worker/mcp need no `web`.
#
# The toolchain tag must match `toolchain` in go.mod (go1.27.3). Binaries are
# static (CGO off), stripped, and run as the distroless non-root user.
ARG GO_VERSION=1.27.3

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/worker ./cmd/worker \
 && go build -ldflags="-s -w" -o /out/suppressnorm ./cmd/suppressnorm \
 && go build -ldflags="-s -w" -o /out/opengtm-mcp ./cmd/mcp \
 && go build -ldflags="-s -w" -o /out/opengtm-migrate ./cmd/migrate \
 && go build -ldflags="-s -w" -o /out/opengtm-scheduler ./cmd/scheduler \
 && go build -ldflags="-s -w" -o /out/opengtm-seed ./cmd/seed

# Stage: build apps/web (the `web` named context) into static files.
FROM oven/bun:1 AS web-build
WORKDIR /web
COPY --from=web package.json bun.lock ./
COPY --from=web vendor/ ./vendor/
COPY --from=web patches/ ./patches/
RUN bun install --frozen-lockfile --ignore-scripts
COPY --from=web . .
RUN bun run build

# API binary with the fresh UI embedded (-tags webui).
FROM build AS build-api
RUN rm -rf internal/webui/dist
COPY --from=web-build /web/dist ./internal/webui/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -tags webui -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build-api /out/api /api
# One-shot schema migrations: `docker compose run --rm migrate` (entrypoint /opengtm-migrate).
COPY --from=build /out/opengtm-migrate /opengtm-migrate
USER nonroot:nonroot
ENV API_ADDR=:8000
EXPOSE 8000
# Distroless has no shell or curl: `/api -healthcheck` GETs 127.0.0.1:<API_ADDR port>/healthz.
HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 CMD ["/api", "-healthcheck"]
ENTRYPOINT ["/api"]

FROM gcr.io/distroless/static-debian12:nonroot AS worker
COPY --from=build /out/worker /worker
# One-shot maintenance: `docker compose run --rm --entrypoint /suppressnorm worker-go --dry-run`.
COPY --from=build /out/suppressnorm /suppressnorm
USER nonroot:nonroot
# No published ports: the debug/metrics server defaults to loopback inside the container.
ENTRYPOINT ["/worker"]

FROM gcr.io/distroless/static-debian12:nonroot AS mcp
COPY --from=build /out/opengtm-mcp /opengtm-mcp
USER nonroot:nonroot
# stdio by default; the compose `mcp` service passes `--sse 3100`.
ENTRYPOINT ["/opengtm-mcp"]

# Recurring-job scheduler (one active instance: Postgres advisory lock). Also carries the opt-in demo seed:
# compose `seed-go` (profile demo) runs /opengtm-seed from this image.
FROM gcr.io/distroless/static-debian12:nonroot AS scheduler
COPY --from=build /out/opengtm-scheduler /opengtm-scheduler
COPY --from=build /out/opengtm-seed /opengtm-seed
USER nonroot:nonroot
ENTRYPOINT ["/opengtm-scheduler"]

# One-shot schema migrations (`opengtm-migrate up`); no web UI build needed.
FROM gcr.io/distroless/static-debian12:nonroot AS migrate
COPY --from=build /out/opengtm-migrate /opengtm-migrate
USER nonroot:nonroot
ENTRYPOINT ["/opengtm-migrate"]
CMD ["up"]
