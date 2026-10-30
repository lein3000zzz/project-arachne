-- What the model decided about each page before indexing it, by URL: the
-- indexer knows pages by URL, and a review belongs to one version of the text.
CREATE TABLE page_reviews (
    url          text        PRIMARY KEY,
    content_hash text        NOT NULL,
    decision     text        NOT NULL CHECK (decision IN ('index', 'skip')),
    reasons      text[]      NOT NULL DEFAULT '{}',
    summary      text        NOT NULL DEFAULT '',
    -- No usable verdict came back; the page was indexed anyway.
    unreviewed   boolean     NOT NULL DEFAULT false,
    model        text        NOT NULL,
    tool_calls   integer     NOT NULL,
    seconds      real        NOT NULL,
    reviewed_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE runs ADD COLUMN docs_rejected integer NOT NULL DEFAULT 0;
