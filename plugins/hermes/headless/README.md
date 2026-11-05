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
| credential `env` | `APA_RUN`/`APA_TOKEN`/`APA_URL` reach the `apa_*` tools (they run in the Hermes process; live test asserts the delegate/ask/report calls carry `Bearer <run credential>`). The `terminal` tool **scrubs `APA_TOKEN`** (measured: `TOKEN_SCRUBBED`), so a bare `apa` in `terminal` has no credential: runners must use the `apa_*` tools, or the owner adds `APA_TOKEN` to `terminal.env_passthrough` (§21.3 fallback) |
| `main_session.args` | not declared: enabled plugins (apa-tools) load in `--oneshot`, verified live |
