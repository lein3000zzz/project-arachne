-- What the pages an artifact's newest version cites say now. A claim rests
-- on words of a page; when the page changes and the words are gone, the
-- claim is flagged, and the artifact asks to be rebuilt.
CREATE TABLE artifact_checks (
    artifact_id uuid        NOT NULL,
    version     int         NOT NULL,
    -- '' records a version with no sources to check.
    url         text        NOT NULL,
    -- The page's version, as its review knows it, last checked against.
    review_hash text        NOT NULL DEFAULT '',
    -- Claims whose words the page held when first checked: only these can
    -- be found gone later, so words never found are never a false alarm.
    -- Each is [section, claim].
    held        jsonb       NOT NULL DEFAULT '[]',
    missing     jsonb       NOT NULL DEFAULT '[]',
    -- The page left the index.
    gone        boolean     NOT NULL DEFAULT false,
    changed_at  timestamptz,
    checked_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (artifact_id, version, url),
    FOREIGN KEY (artifact_id, version) REFERENCES artifact_versions (artifact_id, version) ON DELETE CASCADE
);
