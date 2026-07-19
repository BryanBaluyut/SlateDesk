# SlateDesk v2

Ground-up rewrite of SlateDesk: a self-hosted IT help desk that deploys as a
single static binary + PostgreSQL.

- **Core only**: users, tickets, channels (email, REST API + webhooks, web portal/form)
- **Stack**: Go + chi + sqlc + River · React 19 + Vite + shadcn/ui (embedded via go:embed) · PostgreSQL 17
- **Goals**: working instance in under 5 minutes; one code path from a 1 GB VPS to Kubernetes

See [docs/architecture.md](docs/architecture.md) for the full architecture and build plan.

## Install

One line (Docker + the Compose plugin required):

```sh
curl -fsSL https://get.slatedesk.io | sh
```

The installer generates a database password, writes `docker-compose.yml` +
`.env`, brings the stack up, and prints where to find the **one-time setup
URL** (SlateDesk logs a signed `…/setup?token=…` link on first boot — never a
default password). Then:

```sh
cd slatedesk
docker compose logs app | grep -i setup   # open that URL to create the admin
```

Other paths — manual Compose, bare binary against your own Postgres, headless
admin bootstrap, backups, and upgrades — are in **[docs/deploy.md](docs/deploy.md)**.

Quick reference:

```sh
# manual compose (from a clone)
echo "POSTGRES_PASSWORD=$(openssl rand -hex 24)" > .env && docker compose up -d --build

# bare binary (bring your own PostgreSQL 17)
DATABASE_URL='postgres://user:pass@host:5432/slatedesk?sslmode=require' ./slatedesk serve

# headless admin (skip the wizard's first screen)
SLATEDESK_ADMIN_EMAIL=admin@example.com SLATEDESK_ADMIN_PASSWORD=… ./slatedesk serve

# backup (needs postgresql-client ≥ 17) and restore
slatedesk backup > slatedesk-$(date +%F).tar.gz
slatedesk restore --force slatedesk-2026-07-18.tar.gz

# upgrade: pull the new image and restart — migrations self-apply
docker compose pull && docker compose up -d
```

Pinned beta versions: app `ghcr.io/bryanbaluyut/slatedesk:2.0-beta`,
`postgres:17-alpine`. The only required environment variable is `DATABASE_URL`.

> v1 (FastAPI) lives on the `main` and `feat/*` branches until v2 ships.
