# OpenGTM build. `make build` produces one self-contained Go API (UI embedded) in ./bin.
#
#   make build   clean, build the web UI, build bin/opengtm-{api,worker,mcp,scheduler,migrate} + bin/suppressnorm
#   make migrate build bin/opengtm-migrate and apply pending schema migrations (DATABASE_URL = owner)
#   make run     build, then start api + worker in the background (pids/logs in .run/)
#   make dev     vite dev server for apps/web (pid in .run/web.pid; proxies to VITE_API_TARGET)
#   make kill    stop what run/dev started (pid file, verified against /proc; never by port or name)
#   make up/down docker compose stack
#   make test    go vet + go test, web lint + build
#
# run needs the same environment as the API/worker (DATABASE_URL, SECRET_KEY, ...); it is inherited.
SHELL := /bin/bash

GO_DIR  := apps/backend-go
WEB_DIR := apps/web
UI_DIST := $(GO_DIR)/internal/webui/dist
BIN     := $(CURDIR)/bin
RUN     := $(CURDIR)/.run
GOFLAGS_BUILD := -trimpath -ldflags="-s -w"

.PHONY: build web clean run dev kill up down test migrate migrate-bin

build: clean web
	rm -rf $(UI_DIST) && mkdir -p $(UI_DIST) $(BIN)
	cp -a $(WEB_DIR)/dist/. $(UI_DIST)/
	cd $(GO_DIR) && go build $(GOFLAGS_BUILD) -tags webui -o $(BIN)/opengtm-api ./cmd/api
	cd $(GO_DIR) && go build $(GOFLAGS_BUILD) -o $(BIN)/opengtm-worker ./cmd/worker
	cd $(GO_DIR) && go build $(GOFLAGS_BUILD) -o $(BIN)/opengtm-mcp ./cmd/mcp
	cd $(GO_DIR) && go build $(GOFLAGS_BUILD) -o $(BIN)/opengtm-scheduler ./cmd/scheduler
	cd $(GO_DIR) && go build $(GOFLAGS_BUILD) -o $(BIN)/suppressnorm ./cmd/suppressnorm
	@$(MAKE) --no-print-directory migrate-bin
	@echo "built $(BIN)/{opengtm-api,opengtm-worker,opengtm-mcp,opengtm-scheduler,opengtm-migrate,suppressnorm}"

# Schema migrations (internal/migrate, goose): safe to run concurrently; see docs/go-port/runbooks/migrations.md.
migrate-bin:
	@mkdir -p $(BIN)
	cd $(GO_DIR) && go build $(GOFLAGS_BUILD) -o $(BIN)/opengtm-migrate ./cmd/migrate

migrate: migrate-bin
	@test -n "$$DATABASE_URL" || { echo "DATABASE_URL (owner credentials) is required" >&2; exit 1; }
	$(BIN)/opengtm-migrate up

web:
	cd $(WEB_DIR) && bun install --frozen-lockfile && bun run build

clean:
	rm -rf $(BIN) $(WEB_DIR)/dist $(UI_DIST)

run:
	@mkdir -p $(RUN)
	@for s in api worker; do \
		if scripts/runctl.sh alive $$s; then \
			echo "$$s already running (pid $$(cat $(RUN)/$$s.pid)); run 'make kill' first" >&2; exit 1; fi; \
	done
	@$(MAKE) --no-print-directory build
	@nohup $(BIN)/opengtm-api    >$(RUN)/api.log    2>&1 & echo $$! >$(RUN)/api.pid
	@nohup $(BIN)/opengtm-worker >$(RUN)/worker.log 2>&1 & echo $$! >$(RUN)/worker.pid
	@echo "started api (pid $$(cat $(RUN)/api.pid)) and worker (pid $$(cat $(RUN)/worker.pid)); logs in $(RUN)/"

# vite is started under setsid so its pid is a process-group leader: `make kill` verifies it and stops the whole group.
dev:
	@mkdir -p $(RUN)
	@if scripts/runctl.sh alive web; then \
		echo "vite already running (pid $$(cat $(RUN)/web.pid)); run 'make kill' first" >&2; exit 1; fi
	@cd $(WEB_DIR) && (bun install --frozen-lockfile >/dev/null) && \
		(setsid nohup bun run dev:app >$(RUN)/web.log 2>&1 </dev/null & echo $$! >$(RUN)/web.pid)
	@echo "vite started (pid $$(cat $(RUN)/web.pid)); log $(RUN)/web.log; proxy target $${VITE_API_TARGET:-http://127.0.0.1:3000}"

# Signals only pids whose /proc entry proves they belong to this repo (see scripts/runctl.sh); stale/foreign pids are skipped.
kill:
	@scripts/runctl.sh kill

up:
	docker compose up -d --build

down:
	docker compose down

test:
	cd $(GO_DIR) && go vet ./... && go test ./...
	cd $(WEB_DIR) && bun install --frozen-lockfile && bun run lint && bun run build
