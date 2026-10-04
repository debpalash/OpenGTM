# OpenGTM server: one static Go binary with the React dashboard and the Rust
# kernels (WebAssembly) embedded. Build from the repository root:
#
#   docker build -f apps/server/Dockerfile -t opengtm-server:local .

# Stage 1: dashboard. Same Bun workspace install as the main Dockerfile.
FROM oven/bun:1 AS web
WORKDIR /app
COPY package.json bun.lock ./
COPY apps/web/package.json ./apps/web/
COPY apps/api/package.json ./apps/api/
COPY apps/docs/package.json ./apps/docs/
COPY packages/chrome-extension/package.json ./packages/chrome-extension/
COPY packages/n8n-nodes-yupcha/package.json ./packages/n8n-nodes-yupcha/
RUN bun install --frozen-lockfile --ignore-scripts
COPY apps/web/ ./apps/web/
RUN bun run --cwd apps/web build

# Stage 2: binary. The kernel module is committed, so no Rust toolchain is
# needed here; CI rebuilds it from crates/ to catch drift.
FROM golang:1.27 AS server
ARG VERSION=dev
WORKDIR /src/apps/server
COPY apps/server/go.mod apps/server/go.sum ./
RUN go mod download
COPY apps/server/ ./
COPY --from=web /app/apps/web/dist/ ./internal/server/webdist/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/opengtm ./cmd/opengtm

# Stage 3: runtime. Static binary, non-root, no shell. First-party plugins,
# the v1 connector manifests and the trusted-publisher store ship with it;
# mount more plugin directories and list them in OPENGTM_PLUGIN_DIRS.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/opengtm /usr/local/bin/opengtm
COPY plugins/ /opt/opengtm/plugins/
COPY apps/api/services/leadgen/enrichment/declarative/manifests/ /opt/opengtm/connectors/
COPY docs/connectors/trusted-publishers.json /opt/opengtm/trusted-publishers.json
ENV OPENGTM_PLUGIN_DIRS=/opt/opengtm/plugins \
    OPENGTM_CONNECTOR_DIRS=/opt/opengtm/connectors \
    OPENGTM_PLUGIN_TRUST_STORE=/opt/opengtm/trusted-publishers.json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/opengtm"]
CMD ["serve"]
