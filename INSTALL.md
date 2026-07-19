# Installing SlateDesk

SlateDesk is one static Go binary with the web UI baked in, plus one
PostgreSQL database. No Node runtime, no Redis, no object store — the whole
app is a single container (or a single binary) and Postgres.

This is the quick-start. For backups, upgrades, TLS, and the bare-binary
service layout, see [docs/deploy.md](docs/deploy.md).

---

## What you need

- **Docker** with the **Compose plugin** (`docker compose version` works), and
  a running daemon you can reach.
- About 5 minutes and ~1 GB of RAM for a single-node install.
- A free TCP port on the host (**8000** by default).

That's it. The only thing the app itself requires is a `DATABASE_URL`, and the
Compose stack wires that up for you.

> **Beta note.** The published image
> `ghcr.io/bryanbaluyut/slatedesk:2.0-beta` is not on the registry yet, so the
> one-line `curl | sh` installer (Method 3) does not work during the beta.
> **Use Method 1 (build from source).** It is the tested path today.

---

## Method 1 — Docker Compose from source (recommended for the beta)

You need the repository checked out.

```sh
git clone https://github.com/BryanBaluyut/SlateDesk.git
cd SlateDesk
git checkout v2

# 1. Set the one required secret — a strong Postgres password:
echo "POSTGRES_PASSWORD=$(openssl rand -hex 24)" > .env

# 2. Build the image and start the stack (app + Postgres):
docker compose up -d --build
```

The first build compiles the SPA and the Go binary, so it takes a few minutes;
later starts are instant. When it finishes you have two containers running:
`slatedesk-app-1` (the app on host port 8000) and `slatedesk-postgres-1`.

Check it is healthy:

```sh
curl -fsS http://localhost:8000/healthz    # -> ok
docker compose ps                          # both services "running"/"healthy"
```

Now jump to **[First-run setup](#first-run-setup)**.

---

## Method 2 — bare binary (no Docker)

Bring your own PostgreSQL 17. Build (or download) the binary and point it at
the database:

```sh
# build from a checkout (needs Go 1.26 + Node 24):
make build            # produces ./bin/slatedesk

export DATABASE_URL='postgres://user:pass@localhost:5432/slatedesk?sslmode=disable'
export SLATEDESK_DATA_DIR="$PWD/data"      # where attachment blobs live
./bin/slatedesk serve
```

`serve` applies database migrations (under an advisory lock), generates the
instance secret on first boot, and serves the UI + API on `:8000`
(override with `--addr` or `SLATEDESK_ADDR`). Then jump to
**[First-run setup](#first-run-setup)**.

---

## Method 3 — one-line installer (after the beta)

Once the `2.0-beta` image is published this becomes the fastest path — it
writes a compose file + `.env` with a generated password, starts the stack,
and prints your setup URL:

```sh
curl -fsSL https://get.slatedesk.io | sh
```

Override the install directory, port, or image with `SLATEDESK_DIR`,
`SLATEDESK_PORT`, `SLATEDESK_IMAGE`. (Not usable during the beta — see the note
above.)

---

## First-run setup

SlateDesk has **no default username or password**. On first boot it prints a
**one-time setup URL** — a signed token — to its logs. Find it:

```sh
docker compose logs app | grep -i setup
# bare binary: the URL is on stderr at startup
```

You'll see a line ending in `/setup?token=…`. Open that URL in your browser.

> **Port note.** The app logs the URL with its *container-internal* port
> (`:8000`). If you mapped a different host port, open the URL on the host port
> instead. With the default Method 1 setup they are both 8000, so the logged
> URL works as-is.

The wizard has three screens:

1. **Admin account** — your name, email, password (min 10 characters).
2. **Instance** — a display name and the external URL people will reach it at
   (pre-filled from your browser).
3. **Connect a mailbox** — optional, skippable. Turns email into tickets; you
   can add it later under Settings → Mailboxes.

Finish, and you land in the agent workspace with a seeded **welcome ticket**.
The setup token is now spent — the wizard can't be reused.

### Prefer to skip the wizard? (headless / automation)

Set both before first boot and an admin is created automatically (only if none
exists yet):

```env
SLATEDESK_ADMIN_EMAIL=admin@example.com
SLATEDESK_ADMIN_PASSWORD=a-strong-password
```

---

## Managing the stack

```sh
docker compose ps            # status
docker compose logs -f app   # follow app logs
docker compose restart app   # restart (reprints the setup URL if setup is unfinished)
docker compose down          # stop (keeps data volumes)
docker compose down -v       # stop AND delete the database + attachments
```

Your data lives in two named volumes, `slatedesk_pgdata` and
`slatedesk_attachments`, which survive `down` (but not `down -v`).

---

## Troubleshooting

- **`POSTGRES_PASSWORD` error on `up`** — you skipped step 1. Create the `.env`
  file with a password and re-run.
- **Port 8000 already in use** — something else owns the port. Stop it, or use
  Method 3 with `SLATEDESK_PORT`, or edit the `ports:` line in
  `docker-compose.yml`.
- **Lost the setup URL** — while setup is unfinished it is reprinted on every
  boot: `docker compose restart app && docker compose logs app | grep -i setup`.
- **App won't start, Postgres not ready** — the app waits for Postgres health
  and retries; give it a few seconds. Check `docker compose logs postgres`.

For backups, upgrades, PostgreSQL major-version moves, and TLS, see
[docs/deploy.md](docs/deploy.md).
