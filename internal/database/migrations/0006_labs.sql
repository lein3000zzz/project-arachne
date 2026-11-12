CREATE TABLE labs (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_id   uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       text        NOT NULL,
    topic      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX labs_owner_name ON labs (owner_id, lower(name));

-- Where a lab's material comes from: a link crawled as the request said
-- (scope is a preset or a URL prefix, as runs.Request takes it).
CREATE TABLE sources (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    lab_id     uuid        NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    kind       text        NOT NULL CHECK (kind IN ('link')),
    url        text        NOT NULL,
    scope      text        NOT NULL DEFAULT '',
    max_depth  integer     NOT NULL,
    max_links  integer     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (lab_id, url)
);

ALTER TABLE runs
    ADD COLUMN source_id    uuid REFERENCES sources (id) ON DELETE SET NULL,
    ADD COLUMN submitted_by uuid REFERENCES users (id) ON DELETE SET NULL;
CREATE INDEX runs_source ON runs (source_id);

-- Which pages a source's runs brought in. Membership only: the pages belong to
-- the shared base and outlive every lab.
CREATE TABLE source_pages (
    source_id uuid        NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    page_id   uuid        NOT NULL REFERENCES pages (id) ON DELETE CASCADE,
    added_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, page_id)
);
CREATE INDEX source_pages_page ON source_pages (page_id);
