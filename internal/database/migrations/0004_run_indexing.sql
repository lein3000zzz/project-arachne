-- Documents of a run handed to the indexer and how many it got through: a crawl
-- is done long before its pages are all searchable.
ALTER TABLE runs
    ADD COLUMN docs_queued  integer NOT NULL DEFAULT 0,
    ADD COLUMN docs_indexed integer NOT NULL DEFAULT 0,
    ADD COLUMN docs_failed  integer NOT NULL DEFAULT 0,
    ADD COLUMN indexed_at   timestamptz;
