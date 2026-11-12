-- The fact ledger: what the pages of labs assert, each fact resting on
-- sentences of one indexed version of its page. Pages of the shared base
-- alone are not digested: the model's time goes where someone asks.

-- A page as it was digested. The review's hash says a new version came; the
-- index's hash names the chunks the facts were drawn from.
CREATE TABLE fact_pages (
    url          text        PRIMARY KEY,
    review_hash  text        NOT NULL,
    index_hash   text        NOT NULL,
    -- The chunks chosen to digest now; the rest wait until an answer reads them.
    planned      integer[]   NOT NULL DEFAULT '{}',
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Each chunk digested, facts found or not, and what it cost.
CREATE TABLE fact_chunks (
    url          text        NOT NULL,
    index_hash   text        NOT NULL,
    ordinal      integer     NOT NULL,
    -- The chunk's text: a new version with the same chunk is not digested
    -- again, its facts carry over.
    text_hash    text        NOT NULL,
    facts        integer     NOT NULL,
    -- Offered by the model, not borne out by the sentences they named.
    rejected     integer     NOT NULL,
    seconds      real        NOT NULL,
    model        text        NOT NULL,
    -- The reply could not be read: recorded, so the chunk is not asked about
    -- forever. A model that was down is not recorded; it is asked again.
    error        text        NOT NULL DEFAULT '',
    digested_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (url, index_hash, ordinal)
);

CREATE TABLE facts (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    url          text        NOT NULL,
    index_hash   text        NOT NULL,
    ordinal      integer     NOT NULL,
    claim        text        NOT NULL,
    about        text        NOT NULL DEFAULT '',
    -- The page's own sentences the claim rests on, as the index holds them.
    quote        text        NOT NULL,
    model        text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- A later version of the page no longer holds the quote.
    stale_at     timestamptz
);

CREATE INDEX facts_url ON facts (url, ordinal);
CREATE INDEX fact_chunks_text ON fact_chunks (url, text_hash);
