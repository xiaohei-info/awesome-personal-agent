# apa-headless (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-headless`, then `hermes plugins enable apa-headless`.
Submit: the `apa_declare_headless` tool runs `apa declare -` with `declaration.json` (+ the executors configured for the
tools plugin, because one harness holds one declaration). It takes effect after the owner confirms it in Web.
It refuses when the Hermes profile lacks `terminal.client_env_passthrough: [APA_RUN_CONTEXT]`, or the config cannot be read (see `tools/README.md`, Run identity).
**Use it instead of `apa_declare_executors`**: a later executors-only declaration replaces this one and drops `headless`.

Measured on Hermes v0.22.5 (not copied from appendix A). From v0.23.0 `--oneshot` is a client of the shared gateway; the `credential env` row no longer holds there, see `tools/README.md` (Run identity):

| Declared | Observed |
|---|---|
| `-q {prompt} --format stream-json` | stdout is JSONL; the **last line** is `{"type":"result","session_id":…,"exit_code":…}`, which is what the kernel reads. The `init` event's `session_id` is empty, so `extract` is `$.session_id` of the final event. That line has no `outcome`, so the kernel's stdout fallback (design §8.4) never completes a run here: only a CLI receipt does |
| `--resume {session_id}` | same session id comes back; the model remembers earlier turns |
| unknown session | exit 1, the same code as every other failure, with `Error: No session found matching '<id>'…` on stderr and in the final `result` line. So `exit_codes.session_missing` is `[1]` **and** `exit_output.session_missing` is `["No session found matching"]`: only a failed *resume* with that text continues in a new session (`resume.session_lost`). Any other exit 1 is a normal failure and goes to `needs_input`, for example an unreachable model endpoint (`Provider said: Connection error.`) or `storage_unavailable`. Re-measured on the 2026.9.24 build: a session removed by `hermes sessions delete` reports `No session found matching` once Hermes' local gateway has restarted; while the gateway that served the session is still up, it reports `storage_unavailable` instead, which stays a normal failure |
| `access.read` = `-t apa,web` | no terminal/file toolset: Hermes has no read-only file toolset, so the apa-tools plugin supplies `apa_read_file`/`apa_list_dir` (run directory and working directory only), `apa_item_update` and `apa_material_commit`; read-tier runs read `{materials_path}` and write back through these, never `file`/`terminal`. A successful `apa_item_update` that completes the template's `result_fields`, or an `apa_material_commit` on a `result_material` template, is the receipt (design §8.4) |
| `access.edit` / `full` | adds `file,terminal,delegation`; `full` adds `--yolo` |
| credential `env` | `APA_RUN`/`APA_TOKEN`/`APA_URL` reach the `apa_*` tools (live test: delegate/ask/report carry `Bearer <run credential>`) **and** the `terminal` subprocess (live test `test_terminal_tool_credential_visibility_is_measured` asserts the shell printed `MARK_TOKEN_VISIBLE`), so a bare `apa` in `terminal` works with the edit/full tiers; no `env_passthrough` needed on this host config |
| `main_session.args` | not declared: enabled plugins (apa-tools) load in `--oneshot`, verified live |
