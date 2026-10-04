# Codex fixtures

Real output of Codex CLI 0.160.0 (`gpt-6.1-sol`, ChatGPT sign-in), recorded by the [Codex spike](../../../docs/research/2026-10-04-codex-spike.md) on 2026-10-04. They are sanitized:
- account fields (`creator_user_id`, `creator_account_id`, `plan_type`, `credits`) are dropped;
- paths are placeholders: `<run>` (the run folder), `<decoy>` (a stand-in credential folder), `<scratch>`, `~`;
- thread, turn, response, call and item IDs are placeholders;
- `base_instructions` is `<omitted>`, `timezone` is `UTC`, and permission-profile entries are trimmed to two.

| File | Session | Shows |
|---|---|---|
| `exec-ok.jsonl`, `rollout-ok.jsonl` | Fix a bug with a heredoc, effort `medium` | The golden path; `turn.completed` usage; `token_usage_record` per request; `turn_context.effort` |
| `exec-interrupted.jsonl`, `rollout-interrupted.jsonl` | SIGINT at 45 s, `sleep 900` still running | No final event in the stream; `turn_aborted`; the last `token_count` written 12 s after its `token_usage_record` |
| `exec-denials.jsonl`, `rollout-denials.jsonl` | Denied reads and writes through the shell, `apply_patch`, `view_image` and code mode | Sandbox-denied commands and failed tools leave no stream item; their errors are in `custom_tool_call_output` |
| `exec-waiting-for-network.jsonl` | A placeholder API key and the API at a closed local port, default retries | `Reconnecting... waiting for network` with no end (stopped after 45 s) |
| `exec-network-failed.jsonl` | The same with `--disable unbounded_connection_retries` | `turn.failed` after about 32 s, exit 1 |
