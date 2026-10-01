-- Whether agent runs on this project may get the sandbox's local binding (`agentium init --allow-local-binding`).
-- Gradle's file-lock service needs it, and it lets the agent bind any local port and connect to localhost: off until
-- the user allows it. Every project before this migration keeps it off.
ALTER TABLE projects ADD COLUMN allow_local_binding INTEGER NOT NULL DEFAULT 0 CHECK (allow_local_binding IN (0, 1));
