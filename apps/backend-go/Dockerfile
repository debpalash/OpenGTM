# syntax=docker/dockerfile:1
# OpenGTM Go backend: one multi-stage build, one target per binary.
#
#   docker build --target api    -t opengtm-api-go:local    apps/backend-go
#   docker build --target worker -t opengtm-worker-go:local apps/backend-go
#   docker build --target mcp    -t opengtm-mcp-go:local    apps/backend-go
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
    go build -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -ldflags="-s -w" -o /out/worker ./cmd/worker \
 && go build -ldflags="-s -w" -o /out/opengtm-mcp ./cmd/mcp

FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/api /api
USER nonroot:nonroot
ENV API_ADDR=:8000
EXPOSE 8000
# Distroless has no shell or curl: `/api -healthcheck` GETs 127.0.0.1:<API_ADDR port>/healthz.
HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 CMD ["/api", "-healthcheck"]
ENTRYPOINT ["/api"]

FROM gcr.io/distroless/static-debian12:nonroot AS worker
COPY --from=build /out/worker /worker
USER nonroot:nonroot
# No published ports: the debug/metrics server defaults to loopback inside the container.
ENTRYPOINT ["/worker"]

FROM gcr.io/distroless/static-debian12:nonroot AS mcp
COPY --from=build /out/opengtm-mcp /opengtm-mcp
USER nonroot:nonroot
# stdio by default; the compose `mcp` service passes `--sse 3100`.
ENTRYPOINT ["/opengtm-mcp"]
