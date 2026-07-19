-- 0004_mvp.sql — M4 deployable MVP persistence spine: api_keys, webhooks,
-- webhook_deliveries, canned_replies.
--
-- No schema for the customer portal or the public web form: both reuse the
-- existing users/tickets/articles tables (a portal user is just a
-- customer-role user; a web-form submission is a ticket with channel='web').
--
-- The setup-wizard state (setup_completed, instance_name, external_url) lives
-- as plain rows in the existing settings table — no DDL here; typed accessors
-- live in internal/settings.

-- ---------------------------------------------------------------------------
-- API keys — scoped machine credentials, Bearer-authenticated on /api/v1.
-- The plaintext key (sd_live_…) is shown once at creation and never stored;
-- only its SHA-256 hex digest is kept, and auth is an exact-match lookup on
-- that unique digest (see internal/apikey).
-- ---------------------------------------------------------------------------
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    -- Display-only leading fragment (e.g. "sd_live_ab12cd") so the admin UI
    -- can identify a key without ever seeing its secret.
    key_prefix   text NOT NULL,
    -- SHA-256 hex digest of the full plaintext key. Unique so authentication
    -- is a single indexed equality lookup.
    key_hash     text UNIQUE NOT NULL,
    -- Scope set, subset of {read, write}. Kept simple per M4 scope.
    scopes       text[] NOT NULL,
    -- The admin/agent who minted the key; an authenticated key request acts
    -- as this user carrying the key's scopes. No cascade: keep key rows even
    -- if the creator is later removed is undesirable, so block deletion
    -- (default NO ACTION) — mirrors tickets.requester_id.
    created_by   uuid NOT NULL REFERENCES users (id),
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);

CREATE INDEX api_keys_created_by_idx ON api_keys (created_by);

-- ---------------------------------------------------------------------------
-- Webhooks — outbound HTTP notifications on ticket/article events.
-- The per-endpoint signing secret is AES-GCM-encrypted at rest via the
-- secrets package (purpose "webhook-secret"), same discipline as mailbox
-- credentials.
-- ---------------------------------------------------------------------------
CREATE TABLE webhooks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    url        text NOT NULL,
    -- Versioned base64 AES-GCM ciphertext of the HMAC-SHA256 signing secret.
    secret_enc text NOT NULL,
    -- Subscribed event types, subset of
    -- {ticket.created, ticket.updated, article.created}.
    events     text[] NOT NULL,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Webhook deliveries — the durable outbox log. Rows are enqueued
-- transactionally off ticket_events; a River job attempts HTTP delivery with
-- retry/backoff and records the outcome here.
-- ---------------------------------------------------------------------------
CREATE TYPE webhook_delivery_status AS ENUM ('pending', 'success', 'failed');

CREATE TABLE webhook_deliveries (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    webhook_id    uuid NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_type    text NOT NULL,
    -- The ticket the event concerns (null for events without one, or once a
    -- deleted ticket's log survives it). Not a strong ownership edge.
    ticket_id     uuid REFERENCES tickets (id) ON DELETE SET NULL,
    payload       jsonb NOT NULL,
    status        webhook_delivery_status NOT NULL DEFAULT 'pending',
    response_code integer,
    attempts      integer NOT NULL DEFAULT 0,
    last_error    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    delivered_at  timestamptz
);

CREATE INDEX webhook_deliveries_webhook_id_created_at_idx
    ON webhook_deliveries (webhook_id, created_at);

-- ---------------------------------------------------------------------------
-- Canned replies — reusable agent snippets with {{ticket.number}} /
-- {{requester.name}} style variable substitution done at insert time.
-- ---------------------------------------------------------------------------
CREATE TABLE canned_replies (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title      text NOT NULL,
    body       text NOT NULL,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX canned_replies_title_idx ON canned_replies (title);
