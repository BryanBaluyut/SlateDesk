#!/usr/bin/env bash
# Regenerates all code derived from api/openapi.yaml:
#   - internal/api/gen.go          (Go types + chi ServerInterface + embedded spec)
#   - frontend/src/api/schema.d.ts (TypeScript types for the SPA)
# Run from anywhere; operates on the repo root.
set -euo pipefail

cd "$(dirname "$0")/.."

# Dev-box toolchain lives outside the default PATH (harmless elsewhere).
export PATH="$HOME/.local/node/bin:$HOME/.local/go/bin:$PATH"

# Pinned; bump deliberately and regenerate.
OPENAPI_TYPESCRIPT_VERSION=7.13.0

echo "==> go: oapi-codegen -> internal/api/gen.go"
mkdir -p internal/api
go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

echo "==> ts: openapi-typescript -> frontend/src/api/schema.d.ts"
mkdir -p frontend/src/api
npx --yes "openapi-typescript@${OPENAPI_TYPESCRIPT_VERSION}" api/openapi.yaml \
	--output frontend/src/api/schema.d.ts

echo "==> gofmt check on generated code"
gofmt -l internal/api/gen.go >/dev/null

echo "codegen: done"
