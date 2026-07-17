-- 0001_init.sql — M1 schema: settings, users, teams, team_members.
-- No ticket tables yet (M2).

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TYPE user_role AS ENUM ('customer', 'agent', 'admin');

CREATE TABLE settings (
    key   text PRIMARY KEY,
    value jsonb NOT NULL
);

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext UNIQUE NOT NULL,
    name          text NOT NULL DEFAULT '',
    role          user_role NOT NULL DEFAULT 'customer',
    password_hash text,
    oidc_issuer   text,
    oidc_subject  text,
    company       text NOT NULL DEFAULT '',
    token_version integer NOT NULL DEFAULT 1,
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text UNIQUE NOT NULL,
    description text NOT NULL DEFAULT ''
);

CREATE TABLE team_members (
    team_id uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, user_id)
);
