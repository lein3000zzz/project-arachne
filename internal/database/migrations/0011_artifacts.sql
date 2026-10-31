-- Longer works the model builds inside a lab: research reports, saved
-- answers. Read-only for their owner; every claim cites a document.
CREATE TABLE artifacts (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lab_id     bigint      NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    kind       text        NOT NULL CHECK (kind IN ('report', 'answer')),
    title      text        NOT NULL,
    request    text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX artifacts_lab ON artifacts (lab_id, updated_at DESC);

-- Each build is a version, a rebuild adds one: sections and sources as built,
-- and the Markdown assembled from them.
CREATE TABLE artifact_versions (
    artifact_id bigint           NOT NULL REFERENCES artifacts (id) ON DELETE CASCADE,
    version     int              NOT NULL,
    status      text             NOT NULL CHECK (status IN ('running', 'done', 'failed')),
    outline     jsonb            NOT NULL,
    sections    jsonb            NOT NULL DEFAULT '[]',
    sources     jsonb            NOT NULL DEFAULT '[]',
    markdown    text             NOT NULL DEFAULT '',
    error       text             NOT NULL DEFAULT '',
    model       text             NOT NULL DEFAULT '',
    seconds     double precision NOT NULL DEFAULT 0,
    started_at  timestamptz      NOT NULL DEFAULT now(),
    finished_at timestamptz,
    PRIMARY KEY (artifact_id, version)
);
-- One build at a time per artifact.
CREATE UNIQUE INDEX artifact_versions_running ON artifact_versions (artifact_id) WHERE status = 'running';
