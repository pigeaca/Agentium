-- The module of a monorepo a project measures (`agentium init --module PATH`): a folder of the repository, slash-
-- separated and relative to its root, whose build files decide the project's tools, verify commands and dependencies.
-- Like the other project settings it lives in the data folder, never in the repository. '' (the default) means not set:
-- the repository's root is the project, as before this migration.
ALTER TABLE projects ADD COLUMN module TEXT NOT NULL DEFAULT '';
