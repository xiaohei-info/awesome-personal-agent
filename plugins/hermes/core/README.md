# apa-core (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-core`, then `hermes plugins enable apa-core`.
Needs `apa` on PATH (or `APA_BIN`) plus the usual `APA_URL` / `APA_TOKEN`.

`register(ctx)` calls `register(ctx)` of every submodule; later plugin nodes only add a submodule.
Sections are callables rendered once per new session (Hermes freezes the result); any apa failure
or a read over 2 s yields no section and never blocks the session.

Submodules:

- `instructions`: the standing instructions (`apa guide --template`) as a system prompt section.
- `session_env`: the session identity on every terminal command of the session. Through `ctx.register_session_env`
  Hermes gives each terminal child of a session `APA_SESSION=<Hermes session id>` and `APA_HARNESS=hermes`, so an `apa`
  the model runs in a terminal carries the same identity as the plugins' own calls. The kernel uses it only to tell which
  session wrote what (the inject cursor, "this session is tracking", the capture turn's dedupe, unattached dispatches);
  it never decides what the session may do (design §11.2, §14.2). A process with no session gets nothing.

## Requirements

Hermes with `register_system_prompt_section` (callable content), `hermes_cli.plugins.render_system_prompt_sections`, and
`register_session_env` for the session identity (checked against the Hermes source on 2026-11-28: the local terminal
environment calls the resolver with the child's session id).
Verified on Hermes Agent v0.22.5 (commit `eefc4148cb23`); older builds without that API are unsupported.

## Live gate

`APA_REQUIRE_LIVE=1 HERMES_SRC=<hermes checkout> python3 -m pytest -q plugins/hermes/tests/core/test_core_live.py`
fails (instead of skipping) when Hermes, Go, or the section API is missing. Without the flag it skips with the reason.
