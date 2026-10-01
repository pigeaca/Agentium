-- How a task's runs are graded: "tests" (its hidden tests and verification commands; every task before this
-- migration) or "judge" (no hidden tests: the LLM judge compares a run's change with the reference solution).
ALTER TABLE tasks ADD COLUMN grading TEXT NOT NULL DEFAULT 'tests' CHECK (grading IN ('tests', 'judge'));
