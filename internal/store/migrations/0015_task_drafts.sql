-- Drafts of a task's text (agentium task draft): one no-tools Claude call writes a text from the commit message, the
-- reference change and the hidden tests, kept beside the instruction and never in its place until the owner accepts it
-- (task edit --accept-draft, which clears it). draft is the text ('' when none is stored), draft_at when it was written
-- and draft_model by which model ('' with no draft). draft_spend_usd is what every draft call on the task cost, stored or
-- refused, and stays when a draft is accepted. Every task before this migration has no draft and no drafting spend.
ALTER TABLE tasks ADD COLUMN draft TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN draft_at TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN draft_model TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN draft_spend_usd REAL NOT NULL DEFAULT 0;
-- draft_calls lists every draft call whose cost was counted, by the call's id (the name of its folder in the data folder),
-- so a call's cost is added to its task's spend once: the call's folder is removed after the count, and a crash between
-- the two leaves a folder whose call is already listed here. task_id has no foreign key: a task removed later keeps no
-- row, and its calls' costs stay here.
CREATE TABLE draft_calls (
    id         TEXT PRIMARY KEY,
    task_id    INTEGER NOT NULL,
    cost_usd   REAL NOT NULL,
    counted_at TEXT NOT NULL
);
