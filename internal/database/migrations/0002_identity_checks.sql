-- When a never-fetched page was last asked whether it is another page's alias.
ALTER TABLE pages ADD COLUMN identity_checked_at timestamptz;
