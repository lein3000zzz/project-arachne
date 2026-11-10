-- A MinHash of the page's text as reviewed, to find pages that are the same
-- text under another URL. Empty for a page too short to sketch; NULL for one
-- reviewed before sketches were kept, until the search service fills it in.
ALTER TABLE page_reviews ADD COLUMN sketch bytea;
