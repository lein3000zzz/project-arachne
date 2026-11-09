-- Research: rounds of finding, judging and reading the works on a lab's
-- topic, and every work found, read or not - the lab's map.

-- The subtopics a lab's topic splits into: the tree of the map.
CREATE TABLE lab_subtopics (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lab_id     bigint      NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    position   integer     NOT NULL,
    name       text        NOT NULL,
    focus      text        NOT NULL DEFAULT '',
    queries    text[]      NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX lab_subtopics_name ON lab_subtopics (lab_id, lower(name));

CREATE TABLE research_runs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lab_id       bigint      NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    request      text        NOT NULL DEFAULT '',
    found_budget integer     NOT NULL,
    read_budget  integer     NOT NULL,
    status       text        NOT NULL CHECK (status IN ('running', 'done', 'failed')),
    -- Why it ended: saturated, budget, rounds, time, interrupted.
    stopped      text        NOT NULL DEFAULT '',
    rounds       jsonb       NOT NULL DEFAULT '[]',
    error        text        NOT NULL DEFAULT '',
    model        text        NOT NULL DEFAULT '',
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX research_runs_lab ON research_runs (lab_id, started_at DESC);
-- One at a time per lab.
CREATE UNIQUE INDEX research_runs_running ON research_runs (lab_id) WHERE status = 'running';

-- What research found for a lab: papers and web works, judged, some read.
CREATE TABLE works (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    lab_id          bigint      NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    url             text        NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('paper', 'web')),
    title           text        NOT NULL DEFAULT '',
    snippet         text        NOT NULL DEFAULT '',
    site            text        NOT NULL DEFAULT '',
    year            integer,
    doi             text        NOT NULL DEFAULT '',
    openalex_id     text        NOT NULL DEFAULT '',
    -- Shown, never used to judge: niche works count as much as famous ones.
    cited_by        integer,
    subtopic_id     bigint      REFERENCES lab_subtopics (id) ON DELETE SET NULL,
    verdict         text        NOT NULL DEFAULT 'pending' CHECK (verdict IN ('pending', 'keep', 'maybe', 'drop')),
    reason          text        NOT NULL DEFAULT '',
    by_user         boolean     NOT NULL DEFAULT false,
    -- The lab source it became when read.
    source_id       bigint      REFERENCES sources (id) ON DELETE SET NULL,
    -- OpenAlex was asked for what it cites, and for what cites it.
    references_done boolean     NOT NULL DEFAULT false,
    citing_done     boolean     NOT NULL DEFAULT false,
    first_run       bigint      REFERENCES research_runs (id) ON DELETE SET NULL,
    first_round     integer     NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (lab_id, url)
);
CREATE UNIQUE INDEX works_doi ON works (lab_id, doi) WHERE doi <> '';
CREATE UNIQUE INDEX works_openalex ON works (lab_id, openalex_id) WHERE openalex_id <> '';
CREATE INDEX works_source ON works (source_id);

-- Every way a work was found, once per query or citing work: the coverage
-- estimate counts what search found against what citations found.
CREATE TABLE work_finds (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    work_id  bigint      NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    channel  text        NOT NULL CHECK (channel IN ('seed', 'search', 'reference', 'citing')),
    query    text        NOT NULL DEFAULT '',
    via_work bigint      REFERENCES works (id) ON DELETE CASCADE,
    run_id   bigint      REFERENCES research_runs (id) ON DELETE SET NULL,
    round    integer     NOT NULL DEFAULT 0,
    found_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX work_finds_once ON work_finds (work_id, channel, query, coalesce(via_work, 0));

-- A work of the lab citing another: the map's lines.
CREATE TABLE work_links (
    lab_id    bigint NOT NULL REFERENCES labs (id) ON DELETE CASCADE,
    from_work bigint NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    to_work   bigint NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    PRIMARY KEY (from_work, to_work)
);
CREATE INDEX work_links_lab ON work_links (lab_id);

ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
ALTER TABLE sources ADD CONSTRAINT sources_kind_check CHECK (kind IN ('link', 'search', 'reference', 'upload', 'research'));
