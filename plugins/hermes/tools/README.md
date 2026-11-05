# apa-tools (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-tools`, then `hermes plugins enable apa-tools`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`.

Registers toolset `apa`: `apa_propose`, `apa_ask`, `apa_decide`, `apa_accept`, `apa_dismiss`, `apa_executors`, `apa_delegate`, `apa_report`,
`apa_declare_executors`, `apa_executor_call`. Every CLI tool runs `apa` with `APA_SESSION=<Hermes session_id>`,
so the ledger audits to the session. The result is `{exit_code, ok, stdout, stderr}` with the kernel's verdict untouched
(exit 20 web_only, 21 executor not allowed, ... are the model's cue to stop; nothing is retried or faked).

Executors come from the owner's table: a JSON list at `$APA_HERMES_EXECUTORS` (default `$HERMES_HOME/apa-executors.json`),
e.g. `[{"id":"work-host.cli","kind":"external-cli","scenarios":["coding"],"host":"work-host","command":["codex","exec","{prompt}"]}]`.
No file = nothing configured and nothing declared. `apa_executor_call` maps a configured id to a Hermes call
(`delegate_task` for native-subagent, `terminal` for external-cli; human has none). `apa_declare_executors` submits only
`{id, kind, scenarios, host?}`; the command never reaches the ledger.

## Live gate

`APA_REQUIRE_LIVE=1 HERMES_SRC=<hermes checkout> python3 -m pytest -q plugins/hermes/tests/tools/test_live.py`
