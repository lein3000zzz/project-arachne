-- A run may be focused: one work's citations followed, or one subtopic
-- researched further. Neither set is a run over the whole topic.
ALTER TABLE research_runs
    ADD COLUMN focus_work     uuid REFERENCES works (id) ON DELETE SET NULL,
    ADD COLUMN focus_subtopic uuid REFERENCES lab_subtopics (id) ON DELETE SET NULL;
