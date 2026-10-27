CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      text        NOT NULL,
    -- argon2id as a PHC string, parameters included, so they can be raised
    -- without invalidating older hashes.
    password_hash text        NOT NULL,
    is_admin      boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz
);
CREATE UNIQUE INDEX users_username ON users (lower(username));

-- Sessions and API tokens are stored as sha256 of the secret only: a leaked
-- database hands out nothing that still works.
CREATE TABLE sessions (
    token_hash   bytea       PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    user_agent   text        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE api_tokens (
    token_hash   bytea       PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    UNIQUE (user_id, name)
);

-- Failed logins per 'user:<name>' and per 'ip:<address>'.
CREATE TABLE login_failures (
    key            text        PRIMARY KEY,
    failures       integer     NOT NULL,
    last_failed_at timestamptz NOT NULL,
    locked_until   timestamptz
);
