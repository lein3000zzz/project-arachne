-- Crawl runs, for showing their progress. Recorded by whoever submits a run and
-- by the crawler as it starts and finishes one; runs submitted straight to Kafka
-- appear when they start.
CREATE TABLE runs (
    id           text        PRIMARY KEY,
    start_url    text        NOT NULL,
    scope        text        NOT NULL DEFAULT '',
    max_depth    integer     NOT NULL,
    max_links    integer     NOT NULL,
    submitted_at timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz,
    -- Why a run ended without crawling, when it did.
    note         text        NOT NULL DEFAULT ''
);

CREATE INDEX runs_submitted ON runs (submitted_at DESC);
CREATE INDEX pages_last_run ON pages (last_run_id);
