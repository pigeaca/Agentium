-- Retired tasks (the task pool): retired_at is when a task was retired and retired_reason why (its base is too old, or
-- a file it names is gone from the default branch). Both are empty while the task is active. Retiring never deletes:
-- runs and locked experiments keep their rows, and clearing both columns makes the task active again. Every task
-- before this migration stays active.
ALTER TABLE tasks ADD COLUMN retired_at TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN retired_reason TEXT NOT NULL DEFAULT '';
