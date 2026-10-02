-- A project's own settings (`agentium init --verify CMD --setup CMD --require-lock --jobs N --verify-timeout D`): the
-- defaults that mining, task import, validation and start use unless a command is given its own flag. They live in the
-- data folder, never in the repository, so a teammate's commit cannot change them. Every column's default means "not
-- set": a project registered before this migration behaves as it did. verify and setup are JSON arrays of commands
-- ('[]': not set); jobs 0 and verify_timeout_ms 0 are the built-in defaults.
ALTER TABLE projects ADD COLUMN verify TEXT NOT NULL DEFAULT '[]';
ALTER TABLE projects ADD COLUMN setup TEXT NOT NULL DEFAULT '[]';
ALTER TABLE projects ADD COLUMN require_lock INTEGER NOT NULL DEFAULT 0 CHECK (require_lock IN (0, 1));
ALTER TABLE projects ADD COLUMN jobs INTEGER NOT NULL DEFAULT 0 CHECK (jobs >= 0);
ALTER TABLE projects ADD COLUMN verify_timeout_ms INTEGER NOT NULL DEFAULT 0 CHECK (verify_timeout_ms >= 0);
