# ADR: The task list's gap checks are kept on disk, keyed by the build

## Status
Accepted (the user, 2026-10-05)

## Context
`agentium task list` shows, for every task, how many requirements its hidden tests have that nothing states (`task.Gaps`). The check is a chain of local git searches and reads. After the in-memory fixes of 2026-10-05 (each object read once, several tasks checked at a time) a list of 17 tasks still started 461 git processes: 0.8 s on a quiet machine, 2.5 s on a busy one. Every one of those calls asks something different, so nothing more can be saved inside one command. Two ways to cut them without storing anything were rejected: searching in process instead of `git grep` (git's case folding depends on its build and the locale, so answers could differ), and one `git grep` for several texts (it cannot say which text matched).

An answer depends on the task (two commits, the instruction, two file lists), on the code that checks, and on git (its build and locale decide how a search that ignores case matches). The user set the rule: a cache must be keyed so that it can never serve a stale answer, and it lives in the data folder. Asked how to key the code that checks, the user chose the build over a hand-kept version number.

## Decision
- Keep each task's gaps in `cache/gaps/<project ID>.json` of the data folder (`task.GapsCache`).
- An answer is kept under the task's input: its base and solution commits (by full ID only), instruction, hidden test files and reference files.
- Answers belong to one checker: the build of Agentium, plus git's identity (the program on `PATH`, its version and build options, the locale variables). A checker reads only its own answers. The file holds those of the 8 checkers that saved last, each apart.
- The build is the SHA-256 of the binary's file together with the build information the Go toolchain put in the running program (module, version, revision, settings). The program notes which file it is when it starts; if that file has been replaced or rewritten by the time it is read, the build has no identity and nothing is kept or taken.
- Only complete answers are kept: not a check that failed, was cancelled or skipped a read. A git that a signal ended has answered nothing: its search is an error, never "no match".
- Not kept either, because no key can name what they depend on:
  - an answer whose check searched, ignoring case, for text outside ASCII. How such text matches depends on what git loads to match it (PCRE2 and its Unicode tables, or the C library's), and an upgrade of those changes neither git's version nor its path. ASCII matches the same in all of them. Such a task is checked on every list;
  - anything, for a repository that has replacement refs (`refs/replace/`): git then reads another object than the one an ID names. Agentium makes no such ref; a repository that has one is checked on every list.
- The file's folder must be a real folder and the file a regular file. Through a link the text would leave the folder agents are denied, and anything could be read back as an answer: nothing is read through one, and a save through one is refused and said.
- Only `task list` reads and writes the file. Every other command checks afresh.

## Consequences
- A warm `task list` starts 2 git processes and hashes the binary (about 16 MB, some 10 ms): 0.02 s in all.
- The digest covers what build information cannot tell apart: uncommitted changes, another toolchain, a build overlay. The build information covers the one moment the digest cannot: a binary replaced in the instant between the program's start and its first look at its own file. Only two builds with the same build information, swapped in that instant, would share an identity.
- Every new Agentium build, git upgrade or locale change makes the next list check everything once. The alternative, a version number for the gap check bumped by hand (with a test that fails when its sources change), would survive rebuilds but relies on remembering to bump it; the user preferred the key that cannot be wrong.
- One build is several checkers when it runs under different locale variables: measured on 2026-10-05, a shell with no locale set, the same shell through a Python script (which sets `LC_CTYPE=C.UTF-8` for what it starts) and a terminal with `LANG` set are three. A file of one checker would be replaced at every switch between a terminal, an agent's shell and a script, so it holds the last 8, and two builds used in turn do not undo each other. The ninth checker pushes out the one that saved longest ago.
- The file holds text of hidden tests. It sits in the data folder's `cache`, which runs already deny to agents.
- No migration: the file is disposable. Deleting it, or the whole `cache/gaps` folder, costs one slow list.
- If a second command needs kept gaps, it must use the same key and rules; a different derived result gets its own file, not a field here.
