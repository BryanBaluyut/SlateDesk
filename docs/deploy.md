# Deploying SlateDesk v2

SlateDesk is a single static Go binary with the React SPA embedded, plus one
PostgreSQL database. There is no Node process, no Redis, no object store to
run — the whole app is one container (or one binary) and Postgres.

This guide covers every supported install path, the first-run setup wizard,
backups, and upgrades. For the architecture behind these choices see
[architecture.md](architecture.md) §5 (onboarding) and §6 (deployment).

## Pinned versions

These are the versions this beta is built and tested against. Bump them
deliberately.

| Component            | Version                                     |
|----------------------|---------------------------------------------|
| App image            | `ghcr.io/bryanbaluyut/slatedesk:2.0-beta`   |
| PostgreSQL           | `postgres:17-alpine`                        |
| Go toolchain (build) | `golang:1.26.5`                             |
| Node (SPA build)     | `node:24`                                   |
| Runtime base image   | `gcr.io/distroless/static-debian12:nonroot` |
| pg client for backup | `postgresql-client` **≥ 17** (server major) |

The only environment variable the app *requires* is `DATABASE_URL`.

---

## Path A — one-line installer (recommended)

```sh
curl -fsSL https://get.slatedesk.io | sh
```

The installer ([`install.sh`](../install.sh)):

1. Checks Docker + the Compose plugin are present and the daemon is reachable.
2. Creates an install directory (`./slatedesk` by default).
3. Generates a strong `POSTGRES_PASSWORD` into `.env` (kept on re-runs).
4. Writes a `docker-compose.yml` pinned to the image above.
5. `docker compose up -d`, then waits for the app to answer `/healthz`.
6. Tells you how to find the **one-time setup URL** (see below).

Override behaviour with env vars:

```sh
SLATEDESK_DIR=/opt/slatedesk SLATEDESK_PORT=8080 \
  SLATEDESK_IMAGE=ghcr.io/bryanbaluyut/slatedesk:2.0-beta \
  sh -c "$(curl -fsSL https://get.slatedesk.io)"
```

The installer is safe to re-run: it never regenerates an existing password
and never clobbers an existing compose file.

---

## Path B — manual Docker Compose

If you cloned the repo, the root [`docker-compose.yml`](../docker-compose.yml)
builds the image from source:

```sh
echo "POSTGRES_PASSWORD=$(openssl rand -hex 24)" > .env
docker compose up -d --build
```

If you only want the published image (no source checkout), create two files
in an empty directory:

`.env`

```env
POSTGRES_PASSWORD=change-me-to-a-long-random-string
SLATEDESK_IMAGE=ghcr.io/bryanbaluyut/slatedesk:2.0-beta
SLATEDESK_PORT=8000
```

`docker-compose.yml` — copy the one the installer writes (see `install.sh`),
or the repo file with the `build:` line removed. Then `docker compose up -d`.

The stack defines two named volumes: **`pgdata`** (the database) and
**`attachments`** (uploaded blobs, mounted at `/data`). Both survive
`docker compose down`; only `docker compose down -v` deletes them.

---

## Path C — bare binary (bring your own Postgres)

No Docker required. Download the static binary for your platform, point it at
any PostgreSQL 17, and run:

```sh
export DATABASE_URL='postgres://user:pass@db-host:5432/slatedesk?sslmode=require'
export SLATEDESK_DATA_DIR=/var/lib/slatedesk/data     # attachment blobs
./slatedesk serve
```

`serve` applies pending migrations (under a Postgres advisory lock), ensures
the instance secret, then serves the API + SPA + SSE with embedded job
workers on `:8000` (override with `--addr` or `SLATEDESK_ADDR`).

Roles (one binary):

| Command                 | What it does                                        |
|-------------------------|-----------------------------------------------------|
| `slatedesk serve`       | HTTP + SSE + embedded workers (default).            |
| `slatedesk serve --no-worker` | HTTP only; run workers elsewhere.             |
| `slatedesk worker`      | Job workers + mailbox pollers, no HTTP.             |
| `slatedesk migrate`     | Apply migrations and exit.                          |
| `slatedesk admin create`| Create/reset an admin (see below).                  |
| `slatedesk backup`      | Snapshot database + attachments (see below).        |
| `slatedesk restore`     | Rebuild from a snapshot (see below).                |

---

## First-run setup wizard

On first boot SlateDesk prints a **one-time setup URL** with a signed token to
its logs — never a default username/password. Find it:

```sh
docker compose logs app | grep -i setup        # compose
# or, bare binary: it is on stderr at startup
```

Open the `…/setup?token=…` URL and complete three screens: admin account →
instance name + external URL → connect a mailbox (skippable). The token is
invalidated once an admin exists, so the wizard is single-use.

**Lost or expired token?** The URL is reprinted on *every* boot while setup is
incomplete, and the token is valid for 7 days. If it expired, or you lost the
URL before creating the admin, just restart the app to get a fresh one:

```sh
docker compose restart app          # compose
docker compose logs app | grep -i setup
# bare binary: restart the process; the URL is on stderr at startup
```

(Once the admin exists the token is spent for good; restarting then simply
finishes marking setup complete rather than reprinting a URL.)

### Headless / IaC bootstrap

To skip the wizard's account screen, set both of these before first boot; an
admin is created only if none exists yet (idempotent, never overwrites a live
admin):

```env
SLATEDESK_ADMIN_EMAIL=admin@example.com
SLATEDESK_ADMIN_PASSWORD=a-strong-password
```

Or at any time against a running database:

```sh
slatedesk admin create -email admin@example.com -password 'a-strong-password'
```

---

## Environment reference

| Variable                   | Default  | Purpose                                             |
|----------------------------|----------|-----------------------------------------------------|
| `DATABASE_URL`             | —        | **Required.** Postgres connection string.           |
| `SLATEDESK_ADDR`           | `:8000`  | HTTP listen address.                                |
| `SLATEDESK_DATA_DIR`       | `/data`* | Attachment blob root (`<dir>/attachments`).         |
| `SLATEDESK_ADMIN_EMAIL`    | —        | Headless admin bootstrap (with password).           |
| `SLATEDESK_ADMIN_PASSWORD` | —        | Headless admin bootstrap (with email).              |
| `SLATEDESK_COOKIE_SECURE`  | `auto`   | `auto` / `always` / `never` — session cookie Secure.|

\* The image sets `SLATEDESK_DATA_DIR=/data`; the bare binary defaults to
`./data`.

The **instance root secret** (used to encrypt mailbox credentials at rest) is
*not* an environment variable — it is generated at first boot and stored in
the `settings` table. The database is the trust root, so every replica shares
it with zero config. There is nothing to rotate through the environment.

Behind a TLS-terminating reverse proxy that does not forward
`X-Forwarded-Proto`, set `SLATEDESK_COOKIE_SECURE=always`.

---

## Backup & restore

`slatedesk backup` writes a single, cron-friendly `.tar.gz` containing a
plain-format `pg_dump` of the whole database **and** every attachment blob.
`slatedesk restore` is the inverse.

> **Client requirement.** These subcommands shell out to the standard
> PostgreSQL client tools (`pg_dump` / `psql`). Install `postgresql-client`
> whose major version is **≥ your server's** (17). The tools are deliberately
> **not** in the app image — see [the distroless note](#why-no-pg-client-in-the-app-image).

### Bare-binary / cron (simplest)

```sh
# one snapshot to a file
DATABASE_URL=… SLATEDESK_DATA_DIR=/var/lib/slatedesk/data \
  slatedesk backup > slatedesk-$(date +%F).tar.gz

# nightly cron (03:30)
30 3 * * * DATABASE_URL=… SLATEDESK_DATA_DIR=/var/lib/slatedesk/data \
  /usr/local/bin/slatedesk backup > /backups/slatedesk-$(date +\%F).tar.gz
```

Restore (refuses a database that already has users unless `--force`):

```sh
slatedesk restore slatedesk-2026-07-18.tar.gz
# overwrite an existing instance (stop the app first):
slatedesk restore --force slatedesk-2026-07-18.tar.gz
```

Restore drops and recreates the `public` schema, applies the dump, then
writes the attachments back under `SLATEDESK_DATA_DIR`. **Stop the app before
restoring.**

### Docker Compose (no pg client on the host)

The app image is distroless and has no `pg_dump`, but the **postgres** image
in your stack does — and it can run the same static binary. Lift the binary
out of the app image and run `backup` inside the postgres image, sharing the
database container's network:

```sh
cd ~/slatedesk                        # your install dir (compose + .env)
set -a; . ./.env; set +a              # load POSTGRES_PASSWORD
docker compose cp app:/slatedesk ./slatedesk-bin

docker run --rm \
  --network "container:$(docker compose ps -q postgres)" \
  -e DATABASE_URL="postgres://slatedesk:${POSTGRES_PASSWORD}@localhost:5432/slatedesk?sslmode=disable" \
  -e SLATEDESK_DATA_DIR=/data \
  -v slatedesk_attachments:/data:ro \
  -v "$PWD/slatedesk-bin:/slatedesk:ro" \
  postgres:17-alpine /slatedesk backup > slatedesk-$(date +%F).tar.gz
```

To restore, stop the app first and drop the `:ro` flags plus add `--force`:

```sh
docker compose stop app
docker run --rm \
  --network "container:$(docker compose ps -q postgres)" \
  -e DATABASE_URL="postgres://slatedesk:${POSTGRES_PASSWORD}@localhost:5432/slatedesk?sslmode=disable" \
  -e SLATEDESK_DATA_DIR=/data \
  -v slatedesk_attachments:/data \
  -v "$PWD/slatedesk-bin:/slatedesk:ro" \
  -v "$PWD/slatedesk-2026-07-18.tar.gz:/backup.tar.gz:ro" \
  postgres:17-alpine /slatedesk restore --force /backup.tar.gz
docker compose start app
```

(The named volume is `slatedesk_attachments` — the compose project name is
`slatedesk`. Confirm with `docker volume ls`.)

---

## Upgrades

SlateDesk upgrades are "pull + restart". Migrations are embedded in the binary
and self-apply on boot under an advisory lock, so N replicas can restart
simultaneously and exactly one applies each pending migration.

```sh
# edit SLATEDESK_IMAGE in .env to the new tag, then:
docker compose pull
docker compose up -d
```

Take a `slatedesk backup` before a major upgrade. Downgrades are not
supported (migrations only go forward); roll back by restoring a backup taken
on the old version.

### PostgreSQL major-version upgrade (tested path)

Because `backup`/`restore` use a **logical** `pg_dump`/`psql` (not a
file-level copy), they cross PostgreSQL major versions cleanly — this is the
supported way to move from Postgres 17 to 18+:

```sh
# 1. Back up on the OLD cluster (running).
slatedesk backup > pre-pg-upgrade.tar.gz          # (or the Docker recipe)

# 2. Stop the stack and point Postgres at the new major with a FRESH volume.
docker compose down
#    - bump `postgres:17-alpine` -> `postgres:18-alpine` in the compose file
#    - remove the old data volume so the new major starts empty:
docker volume rm slatedesk_pgdata

# 3. Bring up ONLY Postgres so it initialises the new cluster.
docker compose up -d postgres      # wait until healthy

# 4. Restore into the new cluster (use a pg client >= the new major).
slatedesk restore --force pre-pg-upgrade.tar.gz   # (or the Docker recipe,
                                                  #  postgres:18-alpine image)

# 5. Start the app. Migrations are already in the dump, so this is a no-op.
docker compose up -d
```

Keep `pre-pg-upgrade.tar.gz` until you have confirmed the new cluster is
healthy and complete.

---

## TLS

The recommended default is a small reverse proxy in front of `:8000`. A Caddy
snippet is three lines and gives automatic Let's Encrypt certificates:

```caddyfile
desk.example.com {
    reverse_proxy localhost:8000
}
```

Set `SLATEDESK_COOKIE_SECURE=always` when the proxy terminates TLS. Built-in
Let's Encrypt via `--domain` (certmagic) is a planned nice-to-have and is not
part of this beta; use a reverse proxy for now.

---

## Why no pg client in the app image

The runtime image is `gcr.io/distroless/static-debian12:nonroot`: a single
static binary, **no shell**, non-root. That is the security posture — nothing
in the image to exploit, nothing to run.

Adding `postgresql-client` for `backup`/`restore` would mean re-basing on a
full distro (a shell, a package manager, libpq, their CVE stream) on *every*
running replica, to serve an ops task that runs once a day from one host. We
did not make that trade. Instead:

- **Bare-binary / cron installs** already have (or install) `postgresql-client`
  on the host — `slatedesk backup` just works.
- **Compose installs** run the identical static binary inside the
  `postgres` image, which already ships the exact-matching client (recipe
  above).

The app image stays distroless; backups stay one command.
