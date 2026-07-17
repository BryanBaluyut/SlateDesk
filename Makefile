# SlateDesk Makefile.
# Toolchain lives outside the default PATH on the dev box:
export PATH := $(HOME)/.local/node/bin:$(HOME)/.local/go/bin:$(PATH)

# Tooling note: sqlc pinned; bump deliberately and regenerate.
SQLC_VERSION := v1.31.1

DATABASE_URL ?= postgres://slatedesk:slatedesk_dev@127.0.0.1:5432/slatedesk?sslmode=disable

.PHONY: build frontend run test vet sqlc codegen check-codegen lint-api db-up db-down

# Build the SPA (when frontend/ exists), embed it, and compile the binary.
build: frontend
	go build -o bin/slatedesk ./cmd/slatedesk

# If a frontend project exists, build it and copy the output into the
# embed directory (go:embed cannot reach outside internal/web). Without a
# frontend/, the committed placeholder internal/web/dist/index.html is used.
frontend:
	@if [ -d frontend ]; then \
		cd frontend && npm ci && npm run build && cd .. && \
		rm -rf internal/web/dist && \
		cp -r frontend/dist internal/web/dist; \
	fi

run: build
	DATABASE_URL="$(DATABASE_URL)" ./bin/slatedesk serve

test:
	go test ./...

vet:
	go vet ./...

# Regenerate the sqlc store from internal/store/queries + internal/migrate/sql.
sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

# Regenerate Go + TS code from api/openapi.yaml (spec-first contract).
codegen:
	tools/codegen.sh

# CI drift check: regenerate and fail if committed generated code differs.
check-codegen:
	tools/check-codegen.sh

# Validate the OpenAPI document.
lint-api:
	npx --yes @redocly/cli@2.39.0 lint api/openapi.yaml

# Dev database helpers. Docker group membership is not active in this
# session's login shell, hence `sg docker`.
db-up:
	sg docker -c "docker run -d --name slatedesk-pg \
		-e POSTGRES_USER=slatedesk \
		-e POSTGRES_PASSWORD=slatedesk_dev \
		-e POSTGRES_DB=slatedesk \
		-p 5432:5432 postgres:17"

db-down:
	sg docker -c "docker rm -f slatedesk-pg"

# --- Container image (appended) ----------------------------------------------
.PHONY: docker-build compose-up compose-down

# Build the production image (multi-stage: SPA -> static binary -> distroless).
docker-build:
	sg docker -c "docker build -t slatedesk:dev ."

# Full stack from docker-compose.yml; requires POSTGRES_PASSWORD in .env.
compose-up:
	sg docker -c "docker compose up -d --build"

compose-down:
	sg docker -c "docker compose down"
