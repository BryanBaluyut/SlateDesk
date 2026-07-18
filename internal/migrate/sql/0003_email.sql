-- 0003_email.sql — M3 email channel: mailboxes, email_message_ids,
-- mailbox_leases.
--
-- River's own tables (river_job etc.) are NOT here: they are applied by
-- River's Go migration API in the same boot-time migrate step (see
-- internal/jobs.Migrate, called from internal/migrate.Run under the same
-- advisory lock).

CREATE TYPE mailbox_auth_kind AS ENUM ('basic', 'oauth_m365', 'oauth_google');

-- TLS posture per connection. 'none' exists ONLY for local dev/test against
-- GreenMail (plaintext SMTP/IMAP); production mailboxes use tls or starttls.
CREATE TYPE mail_tls_mode AS ENUM ('tls', 'starttls', 'none');

CREATE TYPE email_direction AS ENUM ('inbound', 'outbound');

CREATE TABLE mailboxes (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name              text NOT NULL,
    email_address     citext UNIQUE NOT NULL,
    active            boolean NOT NULL DEFAULT true,
    auth_kind         mailbox_auth_kind NOT NULL,

    imap_host         text NOT NULL,
    imap_port         integer NOT NULL,
    imap_tls_mode     mail_tls_mode NOT NULL DEFAULT 'tls',
    imap_username     text NOT NULL,

    smtp_host         text NOT NULL,
    smtp_port         integer NOT NULL,
    smtp_tls_mode     mail_tls_mode NOT NULL DEFAULT 'starttls',
    smtp_username     text NOT NULL,

    -- AES-256-GCM-encrypted JSON blob (internal/secrets, purpose
    -- "mailbox-credentials"): password / client_secret / refresh_token /
    -- cached access_token, as applicable to auth_kind.
    credentials_enc   text NOT NULL,
    oauth_tenant_id   text,
    oauth_client_id   text,

    from_display_name text NOT NULL DEFAULT '',
    signature         text NOT NULL DEFAULT '',
    auto_ack_enabled  boolean NOT NULL DEFAULT true,

    -- Health columns feeding the admin mailbox screen's health pills.
    last_poll_at      timestamptz,
    last_error        text,
    last_error_at     timestamptz,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Every inbound AND outbound Message-ID (architecture doc §4): one table
-- gives inbound dedup (PK conflict = redelivery) and reply threading
-- against our own outbound IDs. mailbox_id is history, not ownership —
-- deleting a mailbox must not delete threading state, hence SET NULL.
CREATE TABLE email_message_ids (
    message_id text PRIMARY KEY,
    direction  email_direction NOT NULL,
    mailbox_id uuid REFERENCES mailboxes (id) ON DELETE SET NULL,
    ticket_id  uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    -- Null while an outbound row is recorded before its article's send job
    -- runs, or when the article was since deleted.
    article_id uuid REFERENCES articles (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX email_message_ids_ticket_id_idx ON email_message_ids (ticket_id);

-- Mailbox ownership leases: each active mailbox is polled by exactly one
-- worker's supervisor goroutine, which claims and renews a short lease.
-- On worker death the lease expires and another worker adopts the mailbox
-- (architecture doc §4: River provides ownership, not the connection).
CREATE TABLE mailbox_leases (
    mailbox_id uuid PRIMARY KEY REFERENCES mailboxes (id) ON DELETE CASCADE,
    owner      text NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE INDEX mailbox_leases_expires_at_idx ON mailbox_leases (expires_at);
