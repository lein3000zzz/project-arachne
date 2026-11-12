-- Pages waiting to be indexed. Fetching outpaces embedding by far, and what
-- waits in memory dies with the crawler; a page leaves this table only once
-- indexed or turned down on review.
CREATE TABLE index_queue (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    url           text        NOT NULL,
    run_id        text        NOT NULL DEFAULT '',
    title         text        NOT NULL DEFAULT '',
    content       text        NOT NULL DEFAULT '',
    content_type  text        NOT NULL DEFAULT '',
    fetched_at    timestamptz NOT NULL,
    queued_at     timestamptz NOT NULL DEFAULT now(),
    -- Not before: a failed page waits a while before it is tried again.
    ready_at      timestamptz NOT NULL DEFAULT now(),
    attempts      integer     NOT NULL DEFAULT 0,
    -- A worker holds a page while it indexes it, renewing the lease; a
    -- crawler that died lets it lapse, and another takes the page.
    claimed_by    text,
    claimed_until timestamptz,
    -- Failed every try: kept with its error, for a person to look at.
    dead          boolean     NOT NULL DEFAULT false,
    error         text        NOT NULL DEFAULT ''
);

-- Two versions of a page are indexed in the order they came.
CREATE INDEX index_queue_url ON index_queue (url, id) WHERE NOT dead;
CREATE INDEX index_queue_ready ON index_queue (ready_at, id) WHERE NOT dead;
