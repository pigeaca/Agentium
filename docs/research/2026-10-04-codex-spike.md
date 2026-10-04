# Codex recipe: spike (Codex plan, step 1)

- Date: 2026-10-04
- Scope: [Codex plan](../../.agents/plans/2026-10-04-codex.md) step 1. Hand-run with throwaway bash and Python scripts in a scratch folder; no product code.
- Codex CLI 0.160.0 (Homebrew cask), `gpt-6.1-sol` at the CLI's default effort (one session at `medium`), signed in with the user's ChatGPT login in Agentium's own Codex home, `~/.agentium/codex`. Source read at tag `rust-v0.160.0` of [openai/codex](https://github.com/openai/codex) where noted **[source]**.
- macOS 15.7 (Darwin 24.6), Apple silicon. Go 1.26.3 for the fixture.
- Spend: 5 paid sessions, 23 requests: 261,103 input tokens (195,200 of them cached) and 3,917 output tokens. At $2 input and $10 output per million (the plan's list prices), with cached input at $0.20 (an assumed rate, not yet pinned), that is **$0.21 notional**; **$0.56** with every input token at the full rate. The ChatGPT plan's 5-hour window went from 0% to 2% used, and the weekly window stayed at 0%. Free checks ran with an empty `CODEX_HOME` and no sign-in.
- Console output below is real and trimmed. Paths are placeholders: `<run>` (a run folder), `<decoy>` (a folder standing in for credentials), `<scratch>`, `~`.

## Credentials

- Nothing under `~/.agentium/codex` that may hold a token was opened, copied or moved. The spike listed that folder's file names and sizes, read the `config.toml` and `.sandbox_migration` that Codex wrote there, and copied each run's own rollout out of `sessions/`.
- `~/.codex` was not used. `codex login` and `logout` were not run.
- One isolation recipe was not tried: a per-run `CODEX_HOME` holding a symlink to the shared `auth.json`. Creating the link was refused as credential handling, so that recipe is left to the user (see [open questions](#open-questions-for-step-3)).

## The run recipe

Each session ran `codex exec` in its own process group. Codex's environment was rebuilt from scratch: `PATH`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `TERM`, a run-local `HOME` and `TMPDIR`, and three decoy variables (`DECOY_API_KEY`, `DECOY_PASSWORD`, `DECOY_PLAIN`). `CODEX_HOME` was the shared login home. Everything else came from `-c` overrides:

```
CODEX_HOME=~/.agentium/codex HOME=<run>/home TMPDIR=<run>/tmp \
codex exec --json -m gpt-6.1-sol --ignore-user-config \
  -c approval_policy="never" -c default_permissions="agentium" -c forced_login_method="chatgpt" \
  -c check_for_update_on_startup=false -c web_search="disabled" -c history.persistence="none" \
  -c sqlite_home="<run>/codex-state" -c log_dir="<run>/codex-log" \
  -c analytics.enabled=false -c feedback.enabled=false \
  -c 'features={apps=false,plugins=false,remote_plugin=false,browser_use=false,browser_use_external=false,
       computer_use=false,image_generation=false,memories=false,hooks=false,tool_suggest=false,
       daemon_auto_start=false,fast_mode=false,realtime_conversation=false,
       skill_mcp_dependency_install=false,goals=false}' \
  -c 'shell_environment_policy.set={HOME="~",GOCACHE="<run>/cache/go-build",GOPATH="<run>/cache/gopath",
       GOTOOLCHAIN="local",TMPDIR="<run>/tmp",TMPPREFIX="<run>/tmp/zsh"}' \
  -c shell_environment_policy.ignore_default_excludes=false \
  -c 'shell_environment_policy.exclude=["CODEX_*","OPENAI_*"]' \
  -c 'permissions.agentium.filesystem={":root"="read","<run>/checkout"="write","<run>/tmp"="write",
       "<run>/cache"="write","<decoy>"="deny","<user-repo>"="deny","<run>/records"="deny",
       "<run>/codex-state"="deny","<run>/codex-log"="deny","~/.agentium"="deny","~/.codex"="deny","~/.agents"="deny"}' \
  -c 'permissions.agentium.network={enabled=false}' \
  -c 'projects={"<run>/checkout"={trust_level="untrusted"}}' \
  -C <run>/checkout -o <run>/records/last-message.txt -   < prompt
```

`TMPPREFIX`, the `exclude` lines and the `projects` table were added after sessions 1–2 showed they were needed (below). The fixture was a Go module with a wrong `Sub` and its test, plus a one-line `AGENTS.md`.

## Answers

### Offline edits and tests
Yes. Every session fixed `Sub` through `apply_patch` (a `file_change` item) and ran `go test ./...` in the sandbox, with `GOCACHE` and `GOPATH` in the run's cache and no network:

```
exit=0 elapsed=46.5s thread=<id> rollout_total_tokens=48011 ... leftover: none        # session 1
exit=0 elapsed=24.5s thread=<id> rollout_total_tokens=44732 ... leftover: none        # session 5
```

zsh heredocs failed in session 3 (`zsh:1: can't create temp file for here document: operation not permitted`): zsh writes them under `TMPPREFIX`, `/tmp/zsh` by default, not under `TMPDIR`. With `TMPPREFIX=<run>/tmp/zsh`, session 5's heredoc worked.

### Denied reads and writes
Session 1's model refused to touch the denied paths, because Codex tells it the deny list (`<permissions instructions>`). Session 2 ran with `include_permissions_instructions=false` and made each call:

| Step | Tool | Result |
|---|---|---|
| `cat <decoy>/credentials.json` | shell | `cat: <decoy>/credentials.json: Operation not permitted` |
| update `<decoy>/credentials.json` | `apply_patch` | `apply_patch verification failed: Failed to read file to update <decoy>/credentials.json: Operation not permitted (os error 1)` |
| add `<decoy>/new.txt` | `apply_patch` | `patch rejected: writing outside of the project; rejected by user approval settings` |
| add `<run>/tmp/ok.txt` | `apply_patch` | written |
| `<decoy>/secret.png` | `view_image` | ``unable to locate image at `<decoy>/secret.png`: Operation not permitted (os error 1)`` |
| read the decoy without tools | code mode (JS) | `fetch`, `require` and `process` are undefined; `import("node:fs")` gives `unsupported import in exec` |
| `git commit --allow-empty` | shell | `fatal: Unable to create '<run>/checkout/.git/index.lock': Operation not permitted` |

The free `codex sandbox -P agentium` check agreed: the decoy is unreadable in its `/private/tmp` and `/tmp` forms; `ls ~/.agentium/codex` gives `Operation not permitted`; writes work in the checkout and the run's temp root, but not in `/tmp`, `.git` or the records folder. So `view_image` is covered by the sandbox and does not need to be turned off.

**The stream hides denials.** A command the sandbox denies leaves no `command_execution` item in `exec --json` (in session 2, `cat <decoy>/...` and `git commit` are missing, while the commands that succeeded are there), and failed `apply_patch` and `view_image` calls leave no item either. The source confirms it: the `SandboxDenied` branch returns its output to the model without an end event **[source: `core/src/tools/handlers/unified_exec/exec_command.rs`]**. Denials show only in the rollout's `custom_tool_call_output` records (`exit_code` and the error text).

### No network, no key in `env`, `.git` read-only
- **Network:** `curl: (6) Could not resolve host: example.com`. Binding a loopback port gives `PermissionError: [Errno 1] Operation not permitted`.
- **`.git`:** `touch: .git/audit-probe: Operation not permitted`, and the `git commit` above.
- **Environment.** In session 1 the agent's `env` listed `CODEX_HOME`, `DECOY_API_KEY`, `DECOY_PASSWORD` and `DECOY_PLAIN`. In 0.160.0, `shell_environment_policy.ignore_default_excludes` defaults to **true** when read from TOML, so the documented `*KEY*`/`*SECRET*`/`*TOKEN*` filter is off **[source: `config/src/shell_environment_policy.rs`]**. With `ignore_default_excludes=false` and `exclude=["CODEX_*","OPENAI_*"]` (session 2), `CODEX_HOME` and `DECOY_API_KEY` were gone. `DECOY_PASSWORD` stayed, so Agentium's own allowlist must still filter first. Codex adds `CODEX_CI`, `CODEX_PERMISSION_PROFILE`, `CODEX_SANDBOX`, `CODEX_SANDBOX_NETWORK_DISABLED`, `CODEX_SESSION_ID`, `CODEX_THREAD_ID`, `CODEX_VERSION`, pagers and `NO_COLOR` after the filter.
- API-key mode was not run, so a real `CODEX_API_KEY` in the agent's `env` is unverified; the `CODEX_*` exclude covers it by pattern.

### SIGINT in the middle of a turn
Sessions 3 and 4 were interrupted (SIGINT to the process group):

```
SIGINT at 39.14s, rollout total_tokens=45313
exit=1 elapsed=40.4s ... sigint=True ... killed=False          # session 3: exited 1.3 s after SIGINT
SIGINT at 45.01s, rollout total_tokens=43851
exit=1 elapsed=46.2s ... rollout_total_tokens=55271 ... killed=False   # session 4: 1.2 s
```

- Exit code 1. stderr is empty.
- The stream just stops: no `turn.completed`, `turn.failed` or `error`, so it holds no usage. Session 4's `sleep 900` has an `item.started` and no `item.completed`. The `-o` last-message file is not written.
- The rollout keeps every `token_usage_record` written so far, a final `token_count`, a developer message (`<turn_aborted> The previous turn was interrupted on purpose...`) and `{"type":"turn_aborted","reason":"interrupted",...,"duration_ms":39050}`. An interrupted run's spend can be read from the rollout.

### Live token counts and limit windows
The rollout is written live, and it has two usage records:
- **`token_usage_record`** (top-level type): one per model response, written when the response ends, with `response_id`, the request's `usage`, and the turn's and thread's running totals.
- **`token_count`** (an `event_msg`): the thread total, the last request's usage, `model_context_window`, and the plan's `rate_limits`. It can lag: in session 4 the fifth response's `token_usage_record` was written at 14:56:52.3, but its `token_count` only at 14:57:04.2, when the interrupt ended a 12-second tool call.

The `exec --json` stream has usage only in `turn.completed`, at the end.

Limit windows in login mode, from `token_count.rate_limits` after every request (account fields dropped):

```json
{"limit_id":"codex","limit_name":null,
 "primary":{"used_percent":2.0,"window_minutes":300,"resets_at":1791143361},
 "secondary":{"used_percent":0.0,"window_minutes":10080,"resets_at":1791730161},
 "rate_limit_reached_type":null}
```

The same object also carries `credits`, `plan_type`, `individual_limit` and `spend_control_reached`. Agentium should keep only the windows.

### Leftover processes and daemons
- None after any session, including after SIGINT in session 4. The agent had started `sleep 900` as a background terminal session and `sh -c 'nohup sleep 901 >/dev/null 2>&1 &'`; neither survived the exit. A `setsid` process was not tried, so the sweep stays.
- No new `codex` process (with `daemon_auto_start=false`). The user's own app-server daemon from `~/.codex` and the ChatGPT app's processes ran before the spike and were left alone.

### Reroutes
Not observed: they cannot be triggered safely. From the source:
- `exec --json` prints a reroute as an `item.completed` whose item has type `error` and the message `model rerouted: <from> -> <to> (HighRiskCyberActivity)` **[source: `exec/src/event_processor_with_jsonl_output.rs`]**.
- The rollout does **not** keep `ModelReroute` events **[source: `rollout/src/policy.rs`]**, and `token_usage_record` has no model field. Agentium must watch the stream for reroutes.

### Session fields for drift
| Record | Fields |
|---|---|
| `session_meta` | `cli_version`, `originator` (`codex_exec`), `source` (`exec`), `model_provider`, `history_mode`, `git.commit_hash` |
| `turn_context` | `model`, `effort` (present only when `model_reasoning_effort` is passed), `approval_policy`, `sandbox_policy`, `active_permission_profile.id`, `permission_profile`, `collaboration_mode.settings.reasoning_effort`, `multi_agent_version` |
| `task_started` | `model_context_window` (258,400 here; the catalog says 272,000) |
| `world_state` | `model`, `skills`, `host_skills` (the skill list shown to the model), `multi_agent_mode`, `agents_md` |

`session_meta` also carries `creator_user_id` and `creator_account_id`. Agentium must never record them.

The catalog's default effort for `gpt-6.1-sol` is `low` (`codex debug models`). Without `-c model_reasoning_effort`, the rollout shows `effort` absent and `reasoning_effort: null`, so Agentium should always pass the effort explicitly.

### Per-request tokens and pricing
Each `token_usage_record.usage` has `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens` and `total_tokens`. In all 23 requests:
- `total_tokens = input_tokens + output_tokens`;
- `cached_input_tokens` is part of `input_tokens`;
- `reasoning_output_tokens` is part of `output_tokens`;
- `cache_write_input_tokens` was 0.

Price per request: (input − cached − cache writes) × input rate + cached × cached rate + cache writes × cache-write rate + output × output rate. Requests here were 10–12K input tokens, mostly cached after the first (`11636` input, `11136` cached). Code mode made one request per tool batch.

### Gradle's local binding
Gradle itself was not run; a Python loopback server and client stood in for it, under `codex sandbox -P agentium`:

| Profile network | Loopback bind and connect | `curl https://example.com` | DNS to an outside resolver |
|---|---|---|---|
| `{enabled=false}` | `Operation not permitted` | cannot resolve | `dig: isc_socket_bind: unexpected error` |
| `{enabled=false, allow_local_binding=true}` | `Operation not permitted` | cannot resolve | — |
| `{enabled=true, allow_local_binding=true, mode="limited"}` | works | **200** (full network) | — |
| `{enabled=true, allow_local_binding=true, domains={}}` and `--enable network_proxy` | works | `CONNECT tunnel failed, response 403` | **`dig @8.8.8.8` resolves** |

So Codex has an equivalent only through the experimental `network_proxy` feature. It opens raw DNS (port 53 to any host, a leak channel), sets proxy variables in the agent's environment, and writes its CA files into `CODEX_HOME/proxy`, which is the shared home in login mode. The seatbelt policy allows `*:53` whenever local binding and proxy ports are on **[source: `sandboxing/src/seatbelt.rs`]**.

### The cap overshoot
- Usage can be read only between requests. Each `token_usage_record` lands as its response ends (within 0.1 s in these runs), so a watcher that reads it sees each request at once. A watcher on `token_count` can trail by a whole tool call; Codex polls background terminals for up to 5 minutes by default (`background_terminal_max_timeout`).
- From SIGINT to exit took 1.2–1.3 s, and no request was recorded after the signal.
- A request in flight at SIGINT is never recorded. Whether OpenAI bills it is unknown, so the allowance has to cover one whole request: at most the effective context (258,400 input tokens) plus that request's output. At $2/$10 the input is $0.52 at most; the output limit per request is unknown.
- Observed: session 3 crossed its 45,000-token threshold on a request boundary and stopped with 0 tokens over it. In session 4 a `token_count` watcher saw the fifth request (11,420 tokens) 12 s late.

### Network outages
Free check: no account, the API pointed at a closed local port.
- With defaults, exec prints `Reconnecting... 2/5` to `5/5`, `Falling back from WebSockets to HTTPS transport`, then `Reconnecting... waiting for network` without end (stopped after 45 and 60 s).
- With `--disable unbounded_connection_retries` (a stable feature, on by default), it prints `Connection failed: error sending request` and `turn.failed`, and exits 1 after about 32 s.
- Without any sign-in, exec fails with 401 and exits 1 within seconds.

## Run isolation with the shared login home

Codex keeps its sign-in at `CODEX_HOME/auth.json` and always writes sessions to `CODEX_HOME/sessions/` (no setting moves them) **[source: `login/src/auth/storage.rs`, `core/src/rollout.rs`]**. In login mode every run therefore uses `CODEX_HOME=~/.agentium/codex`, and isolation comes from the flags:
- `--ignore-user-config` and `-c` overrides only. `exec` has no `--config FILE` flag, and `-p NAME` reads `CODEX_HOME/NAME.config.toml`.
- `sqlite_home` and `log_dir` in the run's folder. The state, logs, memories, goals, queue and thread-history SQLite files went there (`log_dir` stayed empty: logs go to `logs_2.sqlite`).
- `history.persistence="none"`, and `memories` off.
- A run-local `HOME`, so `~/.agents/skills` does not load; the agent's shells get the user's `HOME` back.
- The project pinned as untrusted (below).
- `~/.agentium` denied to the agent, which covers the shared home and every other run's rollout.

What the shared home kept after the five sessions (names and sizes only):

```
4321  auth.json                   # unchanged in size
   3  .sandbox_migration          # "v1"
  36  installation_id
418899 models_cache.json          # the model catalog, fetched at start
 369  config.toml                 # two trust entries, sessions 1-2 (below)
      sessions/2026/10/04/rollout-<time>-<thread>.jsonl     # one per run, 68-90 KB
1178  shell_snapshots/<thread>.<n>.sh   # session 4's only: left behind by SIGINT
   0  thread-writer-locks/.coordination.lock
      skills/.system/{imagegen,openai-docs,review-agent,skill-creator,skill-installer}/...   # bundled skills, 49 files
      tmp/arg0/                   # the apply_patch helper links
```

**Project trust.** On its first run, exec wrote this into the shared `config.toml`:

```toml
[projects."<run>/checkout"]
trust_level = "trusted"
```

When the permission profile can write the working folder and the project has no trust level, the app server trusts it and saves that **[source: `app-server/src/request_processors/thread_processor.rs`]**. A trusted checkout would load its `.codex/config.toml`, rules and hooks, against decision 7. The dotted form `-c projects."<path>".trust_level="untrusted"` is ignored (session 2 was still trusted); the table form `-c 'projects={"<path>"={trust_level="untrusted"}}'` works (sessions 3–5 wrote nothing, and a free check with an empty home agreed). Session 2's checkout had a `.codex/config.toml` that set a marker variable. The marker did not reach the agent, but the CLI's own `shell_environment_policy.set` replaces that table, so this does not show whether project config loads.

**After each run**, Agentium should move the run's rollout (found by `thread_id`) to the run's records, and delete a leftover `shell_snapshots/<thread>.*.sh`. A snapshot holds the user's shell environment, but only in Agentium's own home, which the agent cannot read.

**Concurrent runs.** A token refresh first rereads `auth.json` from disk, but only an in-process semaphore guards it **[source: `login/src/auth/manager.rs`]**. Two runs that refresh at the same moment could race, which is the same risk the user's own parallel Codex sessions carry.

## Other observations
- **Code mode.** `gpt-6.1-sol` works through one `exec` tool that runs JavaScript calling `tools.exec_command`, `tools.apply_patch`, `tools.view_image` and `tools.write_stdin`. The stream still shows `command_execution` and `file_change` items.
- **Login shell.** Commands run as `/bin/zsh -lc '<command>'`, a login shell that reads the user's zsh startup files from the restored `HOME`. The `allow_login_shell` setting exists; it was not tried.
- **Bundled skills.** Codex installs five skills into `CODEX_HOME/skills/.system` and lists them to the model (four showed in `codex debug prompt-input`, `imagegen` included while image generation is off). `skills.bundled.enabled=false` removes them (`codex debug prompt-input`).
- **Unknown `:special` paths** in a permission profile are ignored with a startup warning, not refused **[source: `core/src/config/permissions.rs`]**. Access values are `read`, `write` and `deny`; `none` is a legacy alias of `deny`.
- **The fast tier** is `service_tiers: priority` ("2x speed, increased usage") in the catalog; `fast_mode=false` keeps it off.

## Fixtures

Sanitized streams and rollout excerpts from sessions 2, 4 and 5 and the two network checks are in [`internal/codex/testdata`](../../internal/codex/testdata/README.md).

## Gaps
- No reroute was observed; its shape comes from the source.
- API-key mode was not run (no key), so `CODEX_API_KEY` in the agent's environment is unverified.
- The per-run `CODEX_HOME` with a linked `auth.json` was not tried.
- Whether a trusted checkout's `.codex/config.toml` loads was not shown.
- The cached-input rate for `gpt-6.1-sol` is not pinned, and whether an interrupted in-flight request is billed is unknown.
- Gradle was not run; a Python loopback stood in, under `codex sandbox`, not `exec`.
- `setsid` leftovers were not tried.

## Open questions for step 3
1. **The login home.** Keep the shared `CODEX_HOME` (the recipe above, verified), or try a per-run `CODEX_HOME` whose `auth.json` links to the shared file (Codex writes refreshes through the link, by the source)? The second isolates sessions, skills, the catalog and snapshots fully, but it needs the user's approval, since it touches the credential's path.
2. **Permission instructions.** Codex tells the model the deny list. Keep that default for fewer wasted turns, or turn it off? Check what Claude Code's prompt says about its sandbox, for parity.
3. **Login shell.** Should `allow_login_shell=false` be set, so the user's zsh startup files do not run in the agent's shells? Compare with how Claude Code's shell starts, for parity.
4. **Bundled skills.** Should they stay, as in the user's own sessions, or be turned off? They count toward the skill-set drift check.
5. **Gradle.** Refuse Codex arms on Gradle tasks in v1, or allow the proxy mode with its DNS channel when the user opts in?
6. **Concurrent login-mode runs.** Should Agentium serialize Codex login-mode runs to avoid refresh races, or accept the risk?
7. **Allowance.** A fixed allowance of one full-context request, or one priced from the largest request seen so far in the run?
