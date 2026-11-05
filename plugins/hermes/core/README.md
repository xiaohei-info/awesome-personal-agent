# apa-core (Hermes plugin)

Install: `ln -s "$PWD" ~/.hermes/plugins/apa-core`, then `hermes plugins enable apa-core`.
Needs `apa` on PATH (or `APA_BIN`) plus the usual `APA_URL` / `APA_TOKEN`.

`register(ctx)` calls `register(ctx)` of every submodule; later plugin nodes only add a submodule.
Sections are callables rendered once per new session (Hermes freezes the result); any apa failure
or a read over 2 s yields no section and never blocks the session.

## Requirements

Hermes with `register_system_prompt_section` (callable content) and `hermes_cli.plugins.render_system_prompt_sections`.
Verified on Hermes Agent v0.22.5 (commit `eefc4148cb23`); older builds without that API are unsupported.

## Live gate

`APA_REQUIRE_LIVE=1 HERMES_SRC=<hermes checkout> python3 -m pytest -q plugins/hermes/tests/core/test_live.py`
fails (instead of skipping) when Hermes, Go, or the section API is missing. Without the flag it skips with the reason.
