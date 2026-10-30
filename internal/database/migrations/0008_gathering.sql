-- The external engines a lab may search.
ALTER TABLE labs ADD COLUMN engines text[] NOT NULL DEFAULT '{}';

-- One run of the model searching the web for a lab, step by step, so the lab
-- can show what was searched, what was taken and why.
CREATE TABLE gatherings (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lab_id      bigint      NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    request     text        NOT NULL DEFAULT '',
    engines     text[]      NOT NULL DEFAULT '{}',
    status      text        NOT NULL CHECK (status IN ('running', 'done', 'failed')),
    steps       jsonb       NOT NULL DEFAULT '[]',
    summary     text        NOT NULL DEFAULT '',
    error       text        NOT NULL DEFAULT '',
    model       text        NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX gatherings_lab ON gatherings (lab_id, started_at DESC);
-- One at a time per lab.
CREATE UNIQUE INDEX gatherings_running ON gatherings (lab_id) WHERE status = 'running';

-- A source the model found by searching: the query, the engines that returned
-- it, and why it was taken.
ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
ALTER TABLE sources ADD CONSTRAINT sources_kind_check CHECK (kind IN ('link', 'search'));
ALTER TABLE sources
    ADD COLUMN query        text   NOT NULL DEFAULT '',
    ADD COLUMN engines      text[] NOT NULL DEFAULT '{}',
    ADD COLUMN reason       text   NOT NULL DEFAULT '',
    ADD COLUMN gathering_id bigint REFERENCES gatherings (id) ON DELETE SET NULL;
