-- Monorepo modules (`agentium init --module PATH`). A module is a folder of the repository, slash-separated and
-- relative to its root, whose build files decide the tools, verify commands and dependencies of the tasks in it.
--
-- projects.module is only the default for tasks created later (mined, imported, added); tasks.module is what a task
-- runs in: its setup, verification, validation, warm-up and grading use the task's module, never the project's current
-- setting, so changing the setting moves no existing task. '' (the default of both) means the repository's root: a
-- project or task made before this migration behaves as it did. Like the other project settings, both live in the data
-- folder, never in the repository.
ALTER TABLE projects ADD COLUMN module TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN module TEXT NOT NULL DEFAULT '';
