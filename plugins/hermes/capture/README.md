# apa-capture (Hermes plugin)

Install (next to `apa-core`): `ln -s "$PWD" ~/.hermes/plugins/apa-capture`, then `hermes plugins enable apa-capture`.
Needs `APA_URL` / `APA_TOKEN` (or `apa login`).

Candidate capture (design §11.2, appendix A.5): at `on_session_end` (turn level) the conversation delta is posted to
`POST /v1/capture` as `{text, path, host}` with `X-APA-Session`, `X-APA-Harness: hermes` and an `Idempotency-Key` (candidates land as `proposed`). Hermes' `on_session_end` carries no transcript, so
`post_llm_call` buffers each finished turn (`user:` / `assistant:`) per session; each turn is sent once.

- On by default for every project (no per-project switch). A session whose directory is in no project is not captured:
  the kernel answers 200 `captured: false, reason: no_project` and records the class for the diagnostics page. Under a run identity (`APA_RUN`, or the session's run context, see `tools/README.md`) the kernel never captures either, and
  the plugin only ever uses the run credential.
- Asynchronous and invisible: the POST runs in a detached helper process with all output discarded.
- No forced continuation (`inject_message`, `continue`, ...); no judgment logic; any failure is swallowed.

## Failures are visible (appendix A.1)

State dir `$APA_HERMES_CAPTURE_DIR` (default `$HERMES_HOME/apa-capture`): `capture.log` gets one line per failure (command, status/reason; no credentials, no conversation text).
Each failure is also noted as `{at, class}` (`failed.json`, newest 20; class is `kernel_unreachable`, `no_credentials`, `kernel_refused` or `plugin_error`) and rides in the `failures` field of the next request the kernel accepts, so the diagnostics page (`capture_failures`) shows latest time and class. Failures while the kernel is unreachable or no credential exists can only surface after a later successful request.
Kernel answered but refused (4xx/5xx): not re-sent. Kernel unreachable: the request stays in `pending/` with its
original key and the next hook after recovery re-sends it once; a refused re-send is treated as above.

## Live gate

`APA_REQUIRE_LIVE=1 python3 -m pytest -q plugins/hermes/tests/capture/test_capture_live.py` (Hermes at `HERMES_SRC`).
