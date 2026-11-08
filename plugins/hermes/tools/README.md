# apa-tools (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-tools`, then `hermes plugins enable apa-tools`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`.

Registers toolset `apa`: `apa_propose`, `apa_ask`, `apa_decide`, `apa_accept`, `apa_dismiss`, `apa_executors`, `apa_delegate`, `apa_report`,
`apa_declare_executors`, `apa_executor_call`, and the read-tier set `apa_read_file`, `apa_list_dir`, `apa_item_update`, `apa_material_commit`. Every CLI tool runs `apa` with `APA_SESSION=<Hermes session_id>`,
so the ledger audits to the session. The result is `{exit_code, ok, stdout, stderr}` with the kernel's verdict untouched
(exit 20 web_only, 21 executor not allowed, ... are the model's cue to stop; nothing is retried or faked).

Executors come from the owner's table: a JSON list at `$APA_HERMES_EXECUTORS` (default `$HERMES_HOME/apa-executors.json`),
e.g. `[{"id":"work-host.cli","kind":"external-cli","scenarios":["coding"],"host":"work-host","command":["codex","exec","{prompt}"]}]`.
No file = nothing configured and nothing declared. `apa_executor_call` maps a configured id to a Hermes call
(`delegate_task` for native-subagent, `terminal` for external-cli; human has none). `apa_declare_executors` submits only
`{id, kind, scenarios, host?}`; the command never reaches the ledger.

Only the on-behalf tools (`apa_decide`, `apa_accept`, `apa_dismiss`, `apa_item_update`) take optional `evidence` (the owner's own words from this turn, never reworded) or `grant`, passed verbatim to `--evidence` / `--grant`; propose, ask, delegate, report and declare tools have neither.

## Live gate

`APA_REQUIRE_LIVE=1 HERMES_SRC=<hermes checkout> python3 -m pytest -q plugins/hermes/tests/tools/test_live.py`

## Read tier (`-t apa,web`)

Hermes has no read-only file toolset, and task runs with `access: read` (brief, review, guideline-draft) must still read
`{materials_path}` and write their result back. The `apa` toolset covers that without `file` or `terminal`:

- `apa_read_file` / `apa_list_dir`: read-only, confined (symlinks resolved) to the working directory (the run directory, or
  the project checkout for `cwd: project_checkout`) and `runs/$APA_RUN` (the kernel passes no APA_HOME, so the layout `<home>/runs/<run id>/` is matched; sibling runs stay refused). Anything else is refused; files over 200 KB come back truncated and flagged. There is no write tool.
- `apa_item_update`: `apa item update <id> --spec <json> --reason ...`; the kernel's run identity still limits it to the run's own item.
- `apa_material_commit`: `apa material commit [<id>] --from - --reason ... [--kind --title --project]`; the text travels on stdin, never in a shell. It only creates a pending revision the owner confirms.
