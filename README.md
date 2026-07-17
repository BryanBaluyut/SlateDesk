# SlateDesk v2

Ground-up rewrite of SlateDesk: a self-hosted IT help desk that deploys as a
single static binary + PostgreSQL.

- **Core only**: users, tickets, channels (email, REST API + webhooks, web portal/form)
- **Stack**: Go + chi + sqlc + River · React 19 + Vite + shadcn/ui (embedded via go:embed) · PostgreSQL 17
- **Goals**: working instance in under 5 minutes; one code path from a 1 GB VPS to Kubernetes

See [docs/architecture.md](docs/architecture.md) for the full architecture and build plan.

> v1 (FastAPI) lives on the `main` and `feat/*` branches until v2 ships.
