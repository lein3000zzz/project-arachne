CREATE TABLE pages (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    url          text        NOT NULL UNIQUE,
    host         text        NOT NULL,
    -- NULL: a link target never fetched.
    status       integer,
    title        text,
    content_type text,
    last_run_id  text,
    found_at     timestamptz NOT NULL DEFAULT now(),
    fetched_at   timestamptz
);

-- kind 1 is a hyperlink, 2 a resource the page loads (script, style, image).
-- Only hyperlinks carry rank.
CREATE TABLE links (
    src  bigint   NOT NULL REFERENCES pages (id) ON DELETE CASCADE,
    dst  bigint   NOT NULL REFERENCES pages (id) ON DELETE CASCADE,
    kind smallint NOT NULL CHECK (kind IN (1, 2)),
    PRIMARY KEY (src, dst),
    CHECK (src <> dst)
);

CREATE INDEX links_dst ON links (dst);

-- A URL the server permanently redirects to another page.
CREATE TABLE aliases (
    url       text   PRIMARY KEY,
    target_id bigint NOT NULL REFERENCES pages (id) ON DELETE CASCADE
);

CREATE INDEX aliases_target ON aliases (target_id);

CREATE TABLE page_ranks (
    page_id bigint           PRIMARY KEY REFERENCES pages (id) ON DELETE CASCADE,
    score   double precision NOT NULL
);

CREATE TABLE host_ranks (
    host  text             PRIMARY KEY,
    score double precision NOT NULL
);

CREATE TABLE rank_runs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    computed_at timestamptz      NOT NULL DEFAULT now(),
    pages       integer          NOT NULL,
    links       integer          NOT NULL,
    hosts       integer          NOT NULL,
    host_links  integer          NOT NULL,
    iterations  integer          NOT NULL,
    residual    double precision NOT NULL
);

CREATE TABLE meta (
    key   text PRIMARY KEY,
    value text NOT NULL
);
