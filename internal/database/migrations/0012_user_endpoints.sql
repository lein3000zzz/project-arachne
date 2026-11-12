-- The chat model a user brings: an OpenAI- or Anthropic-compatible endpoint.
-- The key is sealed with ENDPOINT_KEY from Vault, bound to the user's id.
CREATE TABLE user_endpoints (
    user_id    uuid        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    provider   text        NOT NULL CHECK (provider IN ('openai-compatible', 'openai', 'anthropic')),
    base_url   text        NOT NULL,
    model      text        NOT NULL,
    api_key    bytea,
    key_hint   text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
