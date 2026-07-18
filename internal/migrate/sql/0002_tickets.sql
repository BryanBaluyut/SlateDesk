-- 0002_tickets.sql — M2 ticket core: tickets, per-day number counters,
-- articles, attachments, events, tags.
--
-- Email-related nullable columns on articles (message_id, in_reply_to,
-- references_header, delivery_status) are included NOW so the M3 email
-- milestone stays purely additive — no M2 code touches them.
--
-- FTS design: one generated tsvector column per table (tickets.subject,
-- articles.body_text), each with its own GIN index; search queries UNION
-- the two match sets and rank per ticket. A durable events outbox is
-- deliberately deferred to M4 (webhooks); M2 realtime uses in-transaction
-- pg_notify only.

CREATE TYPE ticket_status AS ENUM ('open', 'waiting_on_customer', 'on_hold', 'closed');
CREATE TYPE ticket_priority AS ENUM ('low', 'medium', 'high', 'critical');
CREATE TYPE article_sender AS ENUM ('customer', 'agent', 'system');
CREATE TYPE article_channel AS ENUM ('email', 'api', 'web');

-- Per-day ticket number counters. Numbers are YYYYMMDD-NNNN; the NNNN part
-- comes from an UPSERT ... RETURNING on this table, which is race-free at
-- any replica count (the row lock serializes same-day allocations).
CREATE TABLE ticket_number_counters (
    day     date PRIMARY KEY,
    counter integer NOT NULL
);

CREATE TABLE tickets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number       text UNIQUE NOT NULL,
    subject      text NOT NULL,
    status       ticket_status NOT NULL DEFAULT 'open',
    priority     ticket_priority NOT NULL DEFAULT 'medium',
    requester_id uuid NOT NULL REFERENCES users (id),
    assignee_id  uuid REFERENCES users (id),
    team_id      uuid REFERENCES teams (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    closed_at    timestamptz,
    search_tsv   tsvector NOT NULL GENERATED ALWAYS AS (to_tsvector('english', subject)) STORED
);

CREATE INDEX tickets_status_idx ON tickets (status);
CREATE INDEX tickets_assignee_id_idx ON tickets (assignee_id);
CREATE INDEX tickets_team_id_idx ON tickets (team_id);
CREATE INDEX tickets_requester_id_idx ON tickets (requester_id);
CREATE INDEX tickets_updated_at_idx ON tickets (updated_at DESC);
CREATE INDEX tickets_search_tsv_idx ON tickets USING GIN (search_tsv);

CREATE TABLE articles (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id         uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    -- Null author = external/system origin (e.g. inbound email in M3).
    author_id         uuid REFERENCES users (id),
    sender_type       article_sender NOT NULL,
    channel           article_channel NOT NULL,
    is_internal       boolean NOT NULL DEFAULT false,
    body_text         text NOT NULL,
    body_html         text,
    -- Email columns: reserved for M3, unused by M2 code.
    message_id        text,
    in_reply_to       text,
    references_header text,
    delivery_status   text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    search_tsv        tsvector NOT NULL GENERATED ALWAYS AS (to_tsvector('english', body_text)) STORED
);

CREATE INDEX articles_ticket_id_created_at_idx ON articles (ticket_id, created_at);
CREATE INDEX articles_search_tsv_idx ON articles USING GIN (search_tsv);

CREATE TABLE article_attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    article_id   uuid NOT NULL REFERENCES articles (id) ON DELETE CASCADE,
    filename     text NOT NULL,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL,
    storage_key  text UNIQUE NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX article_attachments_article_id_idx ON article_attachments (article_id);

-- Slim audit trail: type + free-form jsonb payload + actor (null = system).
-- The identity pk doubles as the ordering key: created_at (= transaction
-- time) is identical for every event of one transaction, so timestamp
-- ordering alone would be nondeterministic within a write.
CREATE TABLE ticket_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ticket_id  uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    actor_id   uuid REFERENCES users (id),
    type       text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ticket_events_ticket_id_created_at_idx ON ticket_events (ticket_id, created_at);

CREATE TABLE tags (
    id    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name  citext UNIQUE NOT NULL,
    color text
);

CREATE TABLE ticket_tags (
    ticket_id uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    tag_id    uuid NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    PRIMARY KEY (ticket_id, tag_id)
);

CREATE INDEX ticket_tags_tag_id_idx ON ticket_tags (tag_id);
