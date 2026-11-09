# apa-tools (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-tools`, then `hermes plugins enable apa-tools`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`, Hermes v0.23.0 or later, and in the profile's `config.yaml`:

```yaml
terminal:
  client_env_passthrough: [APA_RUN_CONTEXT]
```

Without that line a kernel-launched run is not recognised as a run: file tools refuse, and CLI tools run under the gateway's own credential instead of the run's, so the run-identity checks do not apply. Set it before any run.

## Run identity

Tools and hooks run in Hermes' shared gateway process, so its cwd and environment say nothing about the run (a gateway started by a run's client even keeps that run's `APA_RUN` / `APA_TOKEN`). `run_context.py` is the single source: the session's bound `APA_RUN_CONTEXT` (Hermes freezes it from the client's environment and binds it for that session's turns) names the file the kernel wrote at launch with the run's id, credential, `run_dir` and `work_dir` (design §7.7). Every `apa` call in these plugins gets that path and never the inherited run variables; a session with no run uses the gateway's own credential.

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

- `apa_read_file` / `apa_list_dir`: read-only, confined (symlinks resolved) to this run's `work_dir` (the run directory, or
  the project checkout for `cwd: project_checkout`) and `run_dir`; relative paths start at `work_dir`. Sibling runs and anything else are refused. With no run context (not a run, the config line above missing, the run ended) they refuse and say why, and never fall back to the process cwd. Files over 200 KB come back truncated and flagged. There is no write tool.
- `apa_item_update`: `apa item update <id> --spec <json> --reason ...`; the kernel's run identity still limits it to the run's own item.
- `apa_material_commit`: `apa material commit [<id>] --from - --reason ... [--kind --title --project]`; the text travels on stdin, never in a shell. It only creates a pending revision the owner confirms.
