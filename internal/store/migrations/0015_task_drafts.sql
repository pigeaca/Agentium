-- Drafts of a task's text (agentium task draft): one no-tools Claude call writes a text from the commit message, the
-- reference change and the hidden tests, kept beside the instruction and never in its place until the owner accepts it
-- (task edit --accept-draft, which clears it). draft is the text ('' when none is stored), draft_at when it was written
-- and draft_model by which model ('' with no draft). draft_spend_usd is what every draft call on the task cost, stored or
-- refused, and stays when a draft is accepted. Every task before this migration has no draft and no drafting spend.
ALTER TABLE tasks ADD COLUMN draft TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN draft_at TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN draft_model TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN draft_spend_usd REAL NOT NULL DEFAULT 0;
-- instruction_from_draft is 1 while the instruction is a draft the owner put in place (task edit --accept-draft): such a
-- text is never marked reviewed by anything but the owner (start and pool update --accept-mined hold it back). Changing
-- the instruction by hand clears it.
ALTER TABLE tasks ADD COLUMN instruction_from_draft INTEGER NOT NULL DEFAULT 0;
-- draft_calls lists every draft call whose cost was counted, by the call's id (the name of its folder in the data folder),
-- so a call's cost is added to its task's spend once: the call's folder is removed after the count, and a crash between
-- the two leaves a folder whose call is already listed here. task_id is the task the call was for, which may since be
-- removed (no foreign key): its calls' costs stay here, and a task that took its id later is not charged for them.
CREATE TABLE draft_calls (
    id         TEXT PRIMARY KEY,
    task_id    INTEGER NOT NULL,
    cost_usd   REAL NOT NULL,
    counted_at TEXT NOT NULL
);
