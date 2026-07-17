#!/usr/bin/env bash
# CI drift check: regenerates everything from api/openapi.yaml and fails if
# the committed generated files differ (spec-first contract — architecture
# doc §1: "CI check makes drift impossible").
#
# Generated files MUST be committed; an untracked generated file is drift too.
set -euo pipefail

cd "$(dirname "$0")/.."

GENERATED=(internal/api/gen.go frontend/src/api/schema.d.ts)

tools/codegen.sh

drift=0
if ! git diff --exit-code -- "${GENERATED[@]}"; then
	drift=1
fi
untracked="$(git status --porcelain -- "${GENERATED[@]}" | grep '^??' || true)"
if [ -n "$untracked" ]; then
	echo "$untracked"
	drift=1
fi

if [ "$drift" -ne 0 ]; then
	echo "" >&2
	echo "ERROR: generated code is out of date with api/openapi.yaml." >&2
	echo "Run tools/codegen.sh and commit the result." >&2
	exit 1
fi

echo "check-codegen: no drift"
