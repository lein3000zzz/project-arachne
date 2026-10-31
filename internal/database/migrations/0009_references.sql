-- What a document links to or cites, offered to the labs that hold it. Never
-- links of the graph, never crawled unless a lab takes one as a source.
CREATE TABLE page_references (
    page_id  bigint NOT NULL REFERENCES pages (id) ON DELETE CASCADE,
    position int    NOT NULL,
    url      text   NOT NULL,
    label    text   NOT NULL DEFAULT '',
    PRIMARY KEY (page_id, url)
);

-- A source taken from a document's references: which document, and the
-- citation it came with.
ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
ALTER TABLE sources ADD CONSTRAINT sources_kind_check CHECK (kind IN ('link', 'search', 'reference'));
ALTER TABLE sources
    ADD COLUMN cited_by text NOT NULL DEFAULT '',
    ADD COLUMN citation text NOT NULL DEFAULT '';
