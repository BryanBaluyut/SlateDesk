# SlateDesk v2 — Final Architecture Recommendation

**Decision: Build "Bedrock" — a single Go binary + one PostgreSQL, Libredesk-shaped modular monolith — with six specific grafts from the losing proposals and explicit fixes for every judge-raised risk.** All three judges (operator, solo-builder, skeptical architect) independently picked Bedrock. The rationale converged: all three proposals share the same product spec (article-centric model, 3 channels, 4 statuses, anti-Zammad UI, SSE realtime); the differentiator is that Bedrock has exactly one database, one queue, and one code path from a 1GB VPS to Kubernetes — no second-class SQLite tier where integrity bugs hide, and no untested scale path. Email correctness is the product's reputation, and Bedrock's pipeline spec was the most complete.

---

## 1. Recommended stack (exact)

| Layer | Choice | Why (one line) |
|---|---|---|
| Language | Go 1.24+ | Single static binary is the deployment story; goroutines are the natural shape for per-mailbox IMAP supervisors; Libredesk is the production existence proof. |
| HTTP | chi v5 (stdlib `net/http`) | Zero-dep routing; no framework lock-in. |
| DB | PostgreSQL 17, and only PostgreSQL | One engine = one CI matrix, MVCC for replicas, advisory-locked migrations, LISTEN/NOTIFY fanout, tsvector FTS — no Elasticsearch, no Redis, ever. |
| DB access | sqlc + pgx/v5 | SQL-first, compile-time-checked queries, no ORM. |
| Jobs | River (Postgres-native) | Transactional enqueue: article INSERT + outbound email job commit atomically — a reply can never be saved without its email durably queued; jobs are plain Postgres rows (de-risks River's youth). |
| IMAP inbound | emersion/go-imap/v2 + go-message + jhillyerd/enmime | The exact stack Libredesk proved for this domain; enmime for MIME/attachment extraction. |
| SMTP outbound | wneessen/go-mail | STARTTLS + XOAUTH2, active maintenance, prompt CVE record. |
| OAuth/OIDC | golang.org/x/oauth2 + coreos/go-oidc | M365 client-credentials XOAUTH2 and Gmail OAuth are mandatory in 2026 (Basic Auth is dead); go-oidc gives Entra/Google/Okta/Keycloak as presets. |
| Sanitization / passwords | bluemonday; argon2id (x/crypto) | Kills v1's stored-XSS holes; modern password hashing. |
| Frontend | React 19 + Vite 8 (Rolldown), pure static SPA embedded via go:embed | No Node process in production, ever; frontend adds zero deployment surface. |
| FE data layer | TanStack Router + TanStack Query, SSE-driven invalidation | Type-safe routes, filters in URL params, realtime without WebSockets or sticky sessions. |
| UI kit | shadcn/ui + Tailwind v4 + Radix + cmdk; react-hook-form + zod | Shortest supported path to the Linear/Plain/Front aesthetic — the explicit anti-Zammad brief. |
| API contract | OpenAPI 3.1, spec-first; TS types **and zod validators** generated and CI-verified on every PR | *(Graft from Unibody)* Recovers most of one-language type-safety; CI check makes drift impossible, answering the solo-builder judge's "spec-first will drift" concern. |
| Artifacts | Distroless OCI image (~30MB) + published bare static binary per platform | *(Graft from One)* The bare binary + bring-your-own-Postgres serves the no-Docker crowd. |
| TLS | 3-line Caddy snippet by default; optional built-in Let's Encrypt via certmagic behind `--domain` | *(Graft from One)* Removes the last manual VPS step, opt-in only so it adds no default attack surface. |
| License | Permissive (Apache-2.0 or MIT) — pending owner sign-off | Libredesk's AGPL reception measurably cost adoption among commercial self-hosters. |

## 2. Runner-up, and when to pick it instead

**Unibody (TypeScript: Hono + Drizzle + pg-boss + imapflow/nodemailer, Postgres-only variant).** Pick it only if team composition changes such that Go capacity is unavailable and UI-iteration speed with shared zod schemas outweighs the deployment artifact — TS has the largest 2026 hiring pool and the fastest CRUD loop. If chosen, drop its SQLite tier (all judges flagged the dual matrix) and keep everything else in this report. Do **not** revisit SlateDesk One's dual-backend design: three judges independently identified the same fatal pattern — the default tier runs the hand-rolled queue while the battle-tested path goes undogfooded, and there is no SQLite→Postgres data migration story. SQLite mode remains an *additive later project* if the homelab segment proves strategic; the storage interface and sqlc discipline keep that door ajar without paying for it in v2.0.

## 3. v2 feature scope

**Channels in (3):** Email (flagship — multi-mailbox IMAP IDLE/poll + SMTP, M365/Gmail OAuth, RFC-correct threading, dedup, loop protection, HTML + attachments); REST API + webhooks (free forever, scoped keys, OpenAPI; `ticket.created/updated`, `article.created`, HMAC-signed, River-retried); Web (authenticated customer portal + one built-in public form with rate limit/honeypot — no form builder).

**Features in:** unified users (customer/agent/admin); local auth + TOTP + generic OIDC (auto-provision as customer; admin/agent always explicit — no first-SSO-user-becomes-admin magic); teams as routing queues with per-mailbox signature; ticket lifecycle — 4 statuses (open / waiting_on_customer / on_hold / closed), 4 priorities, atomic YYYYMMDD-NNNN numbering; threaded articles with unmistakable public-reply vs internal-note; article-keyed attachments behind a storage interface (local | S3); **tags + ticket_tags** *(graft from Unibody — all three judges said ship them: two cheap tables, the metadata escape valve replacing custom fields and organizations)*; canned replies; tsvector FTS; fixed views (My tickets / Unassigned / All open / Closed); slim ticket_events audit; dashboard = four counters; per-mailbox health pills; two hardcoded behaviors instead of a trigger engine (loop-guarded auto-ack, notify-assignee); reopen on customer reply to closed ticket; synchronous admin delete/anonymize (GDPR without a task queue); name + logo branding only.

**Explicitly out (with replacement):** knowledge base; SLAs + calendars + escalations; triggers/schedulers/macros/workflows (→ 2 hardcoded behaviors + webhooks); custom fields (→ tags); overviews engine (→ fixed views); merge/split/link; checklists; organizations (→ company string); time accounting; reports module (→ counters); iCal (token-in-URL leak dies with it); mentions + in-app notification feed (→ email + SSE); live chat (v1's was unauthenticated and in-process-stateful; if revived, chat messages become articles with `channel=chat`); social/phone; form builder; custom CSS; user devices; GDPR task queue. Net: 39 tables → ~13. Every cut has a return path via API + webhooks, not schema regret.

## 4. Architecture overview

**Components.** One modular monolith with hard seams: `http` (thin chi handlers) → `core` (tickets/users/teams services) → sqlc repositories; `channels` (email/api/portal, each normalizing into the article model); `mailer`; `jobs` (River); `storage` (local | S3); `events` (outbox → SSE + webhooks). One binary, three roles: `slatedesk serve` (API + SPA + SSE + embedded workers — the default), `serve --no-worker`, `slatedesk worker`. All state in Postgres + object storage; app nodes fully stateless.

**Data model (core).** `users` (~15 cols: role enum, argon2id hash nullable, oidc_issuer+subject nullable, company string, **token_version**, active); `teams` + `team_members`; `mailboxes` (IMAP/SMTP config, auth basic|oauth_m365|oauth_google, credentials AES-GCM-encrypted, OAuth refresh-token columns, health columns `last_poll_at`/`last_error`); `tickets` (number via per-day counters UPSERT…RETURNING — race-free at any replica count; `search_tsv` generated column); `articles` (v1's crown jewel kept wholesale: per-article channel, sender_type, is_internal, sanitized body_html + body_text, email headers, delivery_status); `article_attachments` (storage_key, single table); `email_message_ids` (every inbound **and** outbound Message-ID, direction — this one table fixes mis-threading and gives dedup free); `ticket_events`; `tags` + `ticket_tags`; `canned_replies`; `webhooks`; `settings` — plus River's tables.

**Email pipeline.** *Inbound — with the judges' structural fix:* each mailbox is a **supervised long-lived goroutine holding the IMAP IDLE connection** (60s poll fallback); River provides only **ownership** — a leader-elected lease per mailbox renewed on a short interval — not the connection container *(graft from One; resolves the "long-lived connection inside a job" seam all three judges flagged; on worker death the lease lapses and another worker adopts the mailbox)*. Per message: (1) dedup on Message-ID; (2) loop guard (Auto-Submitted, Precedence bulk/auto_reply, X-Auto-Response-Suppress); (3) threading — walk References newest-first, then In-Reply-To, against `email_message_ids` (matches replies to *our* outbound IDs — v1's broken case), then subject-token fallback, else new ticket; (4) enmime parse, bluemonday-sanitize HTML, stream attachments to storage; (5) unknown sender → customer auto-created; (6) closed-ticket reply reopens; (7) article + message-ID row + auto-ack enqueue in **one transaction**; (8) **set IMAP `\Seen` only after DB commit** — at-least-once ingest that dedup makes exactly-once *(graft from Unibody, adopted verbatim)*. *Outbound:* article insert + `email_send` job commit atomically; real Message-ID under the mailbox's domain, In-Reply-To + full References chain, ID recorded before send; backoff retries; terminal failure = visible badge on the article.

**Auth.** Browser: signed HttpOnly cookies (SameSite=Lax) carrying user id + **token_version**; the per-request user fetch (needed for roles anyway) makes logout-everywhere free — replaces Bedrock's session-revocation table *(graft from Unibody; cheaper, same guarantee)*. **Secret bootstrap, chicken-and-egg resolved:** the instance root secret is auto-generated at first boot and stored *as a plain row* in `settings` — the DB is the trust root (DB compromise already implies session forgery), so "encrypted in the DB with a key from the DB" circularity is simply deleted; mailbox credentials are AES-GCM-encrypted *with a key derived from that root secret*. Every replica shares it with zero config. Machine: scoped API keys, `sd_live_`-prefixed, SHA-256-hashed, Bearer-only. Login rate-limited; RFC 9457 errors; Idempotency-Key honored on POST /tickets.

**Realtime.** One SSE endpoint; handlers write to the events outbox in-transaction; LISTEN/NOTIFY fans out to every replica's SSE hub; clients get `{type:"ticket.updated", id}` → `invalidateQueries`. Any replica serves any client.

**UI acceptance criteria** *(grafts from Unibody, written down as testable)*: three-pane workspace (queue / thread with docked composer / collapsible context sidebar); cmdk palette; keyboard map **j/k navigate, a assign, s status, r reply**; Reply vs Internal-note segmented toggle with internal notes on a **distinct amber-tinted surface**; hairline borders, calm neutrals + one accent, first-class dark mode; banned: persistent open-ticket tab strip, dense overview grids as home.

## 5. Onboarding flow (first-run, exact)

Judge-flagged handwave fixed: a static compose file cannot contain a generated password, so the canonical path is an **install script**.

1. `curl -fsSL https://get.slatedesk.io | sh` — writes `docker-compose.yml` + `.env` with a locally generated `POSTGRES_PASSWORD`, runs `docker compose up -d`. (Manual path documented: download compose, set one variable. Bare-binary path: `./slatedesk serve --db <url> [--domain desk.example.com]` with certmagic TLS.)
2. App waits on Postgres health, auto-applies embedded migrations under an advisory lock, generates and stores the instance secret. Logs print a **one-time installer URL** (`/setup?token=…`) — never default credentials; setup state lives in the DB, not process memory.
3. Wizard, three screens: admin name/email/password → instance name + external URL (pre-filled) → **Connect a mailbox**: M365 guided OAuth walkthrough (exact admin-consent scopes listed), Google OAuth, or generic IMAP/SMTP — with live **Fetch test / Send test** buttons and surfaced SPF/DKIM/DMARC lookups. Skippable; API and web form work immediately.
4. Land in the workspace with a **seeded welcome ticket** ("reply to try the composer") *(graft from One)* — first screen is a working helpdesk, not an empty table.
5. Headless/IaC: `SLATEDESK_ADMIN_EMAIL/PASSWORD` env vars or `slatedesk admin create`; mailboxes creatable via API.

Required env surface: `DATABASE_URL`, nothing else. Wall-clock: ~3 minutes to a logged-in instance; +2 for generic IMAP; only M365/Gmail app registration can exceed 5, with a pinned versioned walkthrough.

## 6. Deployment & scaling path

- **Tier 1 (1 vCPU / 1GB VPS):** compose as-is — app (serve + embedded workers) + postgres:17; ~100-150MB app RSS. **Ops-tax mitigations** *(graft from One, answering the operator judge's biggest gripe)*: ship `slatedesk backup` (pg_dump + attachments → file or S3, one command, cron-able) and `slatedesk restore`, plus a documented, tested Postgres major-version upgrade path — backup approaches "copy one directory" simplicity.
- **Tier 2:** scale `app` to N replicas behind any LB — safe by construction (signed cookies, LISTEN/NOTIFY SSE fanout, lease-owned mailbox pollers, no in-process state). Optionally one dedicated `slatedesk worker`.
- **Tier 3 (Kubernetes):** Helm chart — stateless web Deployment (HPA; /healthz + /readyz), worker Deployment (River coordinates), migrations as a **pre-upgrade hook Job**, managed Postgres, S3 attachments, zero RWX volumes.
- Ceilings: managed-Postgres HA when needed; tsvector + GIN covers helpdesk corpora; SSE connections are cheap goroutines. Upgrades at every tier: pull + restart; migrations self-apply.

## 7. Build plan — phased milestones

- **M1 — Skeleton (weeks 1-3):** repo, CI, distroless build, migrations + advisory lock, settings/secret bootstrap, users/teams/auth (local + cookies + token_version), OpenAPI pipeline with CI-verified TS+zod codegen, SPA shell embedded, /healthz + /readyz.
- **M2 — Ticket core (weeks 3-6):** tickets/articles/attachments/tags/events, atomic numbering, FTS, SSE + LISTEN/NOTIFY, agent workspace (3-pane, composer toggle, keyboard map, cmdk), fixed views.
- **M3 — Email (weeks 6-10), the make-or-break phase:** mailbox model + encrypted creds, goroutine-per-mailbox with River leases, full inbound pipeline (dedup, loop guard, threading, \Seen-after-commit), transactional outbound + delivery badges, auto-ack + notify-assignee, admin mailbox screen with fetch/send tests + health pills. **Gate:** an automated threading torture-suite (replies-to-replies, redelivery, autoresponders, closed-ticket reopen) must pass before M4.
- **M4 — Deployable MVP (weeks 10-13):** first-run wizard + installer token + install script, M365/Gmail OAuth wizards, customer portal + public form, canned replies, API keys + webhooks, `slatedesk backup/restore`, seeded ticket, docs with pinned versions. **This is the shippable v2.0-beta.**
- **M5 — Scale & polish (weeks 13-16):** Helm chart, S3 storage, OIDC SSO + TOTP, dark-mode QA, load tests (SSE fanout, ingest at N replicas), pilot migration of real mailbox traffic, versioned changelog + release cadence (the trust signals that capped Peppermint).

## 8. Open questions — product owner only

1. **License:** permissive (recommended, converts commercial self-hosters) vs AGPL (protects against hosted clones)? Blocks the repo going public.
2. **v1 data migration:** do existing SlateDesk installs need an importer (tickets/articles/users), or is v2 greenfield? No proposal budgeted this; an importer is roughly a milestone of work.
3. **Target segment priority:** if homelab/solo-admin is strategic, the SQLite tier gets scheduled as the first post-2.0 project; if mid-market IT, tags/KB pressure comes first. Which wins?
4. **Hosted SaaS ambition:** does a future managed offering exist? It changes license calculus and argues for early multi-tenancy hooks (we recommend *not* building them now either way).
5. **M365 depth:** is IMAP+XOAUTH2 acceptable at launch, or do flagship customers require Microsoft Graph API integration day one (currently a flagged fast-follow behind the email seam)?
6. **First fast-follow after 2.0:** knowledge base, SLA timers, or in-app notifications? Community pressure will demand a public answer at launch.
7. **i18n scope at launch:** English-only vs localized agent UI (affects M2 string discipline).
8. **Name/branding for the anti-Zammad identity** — the UI is the headline; marketing needs it before beta.