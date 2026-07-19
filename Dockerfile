# SlateDesk v2 — production image (architecture doc §1 "Artifacts":
# distroless OCI image around a single static binary).
#
# Stage 1 builds the SPA, stage 2 compiles the static Go binary with the SPA
# embedded via go:embed, and the final image is distroless: one binary, no
# shell, nonroot.
#
# Base images are pinned by tag (bump deliberately, alongside go.mod / the
# frontend toolchain). The PostgreSQL client (pg_dump/psql) is intentionally
# NOT in this image — `slatedesk backup`/`restore` run from the bare binary or
# the postgres image, keeping the runtime shell-less; see docs/deploy.md.

# --- Stage 1: frontend (Vite SPA) -------------------------------------------
FROM node:24 AS frontend
WORKDIR /build/frontend
# Dependencies first so Docker layer-caches them across source-only changes.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# --- Stage 2: backend (static Go binary, SPA embedded) -----------------------
FROM golang:1.26.5 AS backend
WORKDIR /build
# Module download first, again for layer caching.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Replace the committed placeholder with the real SPA build. go:embed reads
# internal/web/dist at compile time and cannot reach outside the package
# directory, hence the copy (mirrors the Makefile `frontend` target).
RUN rm -rf internal/web/dist
COPY --from=frontend /build/frontend/dist internal/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/slatedesk ./cmd/slatedesk
# Pre-create the attachments data dir so a fresh named volume mounted at /data
# inherits nonroot ownership (65532 = distroless nonroot) — the shell-less
# final image cannot chown at runtime, and the app runs unprivileged.
RUN mkdir -p /data/attachments && touch /data/attachments/.keep

# --- Final: distroless, nonroot ----------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend /out/slatedesk /slatedesk
COPY --from=backend --chown=65532:65532 /data /data
# The app writes attachment blobs under SLATEDESK_DATA_DIR/attachments; /data
# is a persistent volume in the compose stack.
ENV SLATEDESK_DATA_DIR=/data
EXPOSE 8000
# DATABASE_URL is the only required environment variable (architecture §5).
ENTRYPOINT ["/slatedesk", "serve"]
