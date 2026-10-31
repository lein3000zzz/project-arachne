-- Files users upload to their labs. The id is in the file's address, so it
-- is random rather than a hash anyone holding the file could compute.
CREATE TABLE uploads (
    id           text        PRIMARY KEY,
    owner_id     bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    sha256       bytea       NOT NULL,
    filename     text        NOT NULL,
    content_type text        NOT NULL DEFAULT '',
    size         bigint      NOT NULL,
    data         bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, sha256)
);

ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
ALTER TABLE sources ADD CONSTRAINT sources_kind_check CHECK (kind IN ('link', 'search', 'reference', 'upload'));
ALTER TABLE sources ADD COLUMN filename text NOT NULL DEFAULT '';
