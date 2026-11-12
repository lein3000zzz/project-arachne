-- Facts that say the same thing, or cannot both be true, across pages.

-- A claim's embedding, for finding the facts worth comparing; from the model
-- named, so a new embedder embeds again.
ALTER TABLE facts ADD COLUMN embedding real[];
ALTER TABLE facts ADD COLUMN embed_model text NOT NULL DEFAULT '';

-- A pair of facts the model compared, the lower id first. "different" is
-- kept too: a pair is never asked about twice.
CREATE TABLE fact_links (
    a          uuid        NOT NULL REFERENCES facts (id) ON DELETE CASCADE,
    b          uuid        NOT NULL REFERENCES facts (id) ON DELETE CASCADE,
    relation   text        NOT NULL CHECK (relation IN ('same', 'contradict', 'different')),
    similarity real        NOT NULL,
    model      text        NOT NULL,
    checked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (a, b),
    CHECK (a < b)
);

CREATE INDEX fact_links_b ON fact_links (b);

-- Where comparing a lab's facts got to: its live embedded facts, counted, as
-- they were when every pair worth asking about had been asked.
CREATE TABLE fact_lab_state (
    lab_id     uuid        PRIMARY KEY REFERENCES labs (id) ON DELETE CASCADE,
    signature  text        NOT NULL,
    related_at timestamptz NOT NULL DEFAULT now()
);
