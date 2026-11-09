# apa-capture (Hermes plugin)

Install (next to `apa-core`): `ln -s "$PWD" ~/.hermes/plugins/apa-capture`, then `hermes plugins enable apa-capture`.
Needs `APA_URL` / `APA_TOKEN` (or `apa login`).

Candidate capture (design §11.2, appendix A.5): at `on_session_end` (turn level) the conversation delta is posted to
`POST /v1/capture` as `{text, path, host}` with `X-APA-Session`, `X-APA-Harness: hermes` and an `Idempotency-Key` (never `X-APA-Evidence`: candidates carry no first-person quote and land as `proposed`). Hermes' `on_session_end` carries no transcript, so
`post_llm_call` buffers each finished turn (`user:` / `assistant:`) per session; each turn is sent once.

- Off by default: the kernel only accepts it for projects with `capture` on (`apa project update <id> --capture`),
  and answers 200 `captured: false` otherwise. Under a run identity (`APA_RUN`, or the session's run context, see `tools/README.md`) the kernel never captures either, and
  the plugin only ever uses the run credential.
- Asynchronous and invisible: the POST runs in a detached helper process with all output discarded.
- No forced continuation (`inject_message`, `continue`, ...); no judgment logic; any failure is swallowed.

## Failures are visible (appendix A.1)

State dir `$APA_HERMES_CAPTURE_DIR` (default `$HERMES_HOME/apa-capture`): `capture.log` gets one line per failure (command, status/reason; no credentials, no conversation text).
Kernel answered but refused (4xx/5xx): logged only, not re-sent. Kernel unreachable: the request stays in `pending/` with its
original key and the next hook after recovery re-sends it once; a refused re-send is treated as above.

## Live gate

`APA_REQUIRE_LIVE=1 python3 -m pytest -q plugins/hermes/tests/capture/test_live.py` (Hermes at `HERMES_SRC`).
