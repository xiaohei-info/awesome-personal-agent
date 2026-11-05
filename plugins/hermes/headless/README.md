# apa-headless (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-headless`, then `hermes plugins enable apa-headless`.
Submit: the `apa_declare_headless` tool runs `apa declare -` with `declaration.json` (+ the executors configured for the
tools plugin, because one harness holds one declaration). It takes effect after the owner confirms it in Web.
**Use it instead of `apa_declare_executors`**: a later executors-only declaration replaces this one and drops `headless`.

Measured on Hermes v0.22.5 (not copied from appendix A):

| Declared | Observed |
|---|---|
| `-q {prompt} --format stream-json` | stdout is JSONL; the **last line** is `{"type":"result","session_id":…,"exit_code":…}`, which is what the kernel reads. The `init` event's `session_id` is empty, so `extract` is `$.session_id` of the final event |
| `--resume {session_id}` | same session id comes back; the model remembers earlier turns |
| unknown session | exit 1 (same as any failure) with `error: No session found…`; `exit_codes.session_missing` is therefore **not** declared, a failed resume goes to `needs_input` |
| `access.read` = `-t apa,web` | no terminal/file toolset: Hermes has no read-only file toolset, so `read` cannot read local files |
| `access.edit` / `full` | adds `file,terminal,delegation`; `full` adds `--yolo` |
| credential `env` | `APA_RUN`/`APA_TOKEN`/`APA_URL` reach the `apa_*` tools (live test: delegate/ask/report carry `Bearer <run credential>`) **and** the `terminal` subprocess (live test `test_terminal_tool_credential_visibility_is_measured` asserts the shell printed `MARK_TOKEN_VISIBLE`), so a bare `apa` in `terminal` works with the edit/full tiers; no `env_passthrough` needed on this host config |
| `main_session.args` | not declared: enabled plugins (apa-tools) load in `--oneshot`, verified live |
