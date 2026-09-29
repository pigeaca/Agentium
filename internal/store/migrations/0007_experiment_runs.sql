-- Running experiments. The lock (versions, contexts, task specs, schedule, caps, price table) is written before the
-- first run and never replaced, except to record a raised budget. Runs point to their slot in the schedule and their
-- attempt; a slot's state is read from its runs, so an interrupted process leaves nothing else to repair.
ALTER TABLE experiments ADD COLUMN lock TEXT;                               -- JSON; NULL until the first run
ALTER TABLE experiments ADD COLUMN status TEXT NOT NULL DEFAULT 'draft';    -- draft, running, stopped, budget, done
ALTER TABLE experiments ADD COLUMN status_note TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN experiment_id INTEGER REFERENCES experiments (id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN slot INTEGER;    -- the position in the experiment's schedule
ALTER TABLE runs ADD COLUMN attempt INTEGER; -- 1, then more after infrastructure failures
CREATE INDEX runs_by_experiment ON runs (experiment_id, slot);
