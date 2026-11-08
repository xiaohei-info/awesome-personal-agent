# apa-capture (Hermes plugin)

Install (next to `apa-core`): `ln -s "$PWD" ~/.hermes/plugins/apa-capture`, then `hermes plugins enable apa-capture`.
Needs `APA_URL` / `APA_TOKEN` (or `apa login`).

Candidate capture (design §11.2, appendix A.5): at `on_session_end` (turn level) the conversation delta is posted to
`POST /v1/capture` as `{text, path, host}` with `X-APA-Session`. Hermes' `on_session_end` carries no transcript, so
`post_llm_call` buffers each finished turn (`user:` / `assistant:`) per session; each turn is sent once.

- Off by default: the kernel only accepts it for projects with `capture` on (`apa project update <id> --capture`),
  and answers 200 `captured: false` otherwise. Under a run identity (`APA_RUN`) the kernel never captures either, and
  the plugin only ever uses the run credential.
- Asynchronous and invisible: the POST runs in a detached helper process with all output discarded.
- The turn's own user words go as `X-APA-Evidence` (a delegate credential needs it); failures are written to `$HERMES_HOME/apa-capture.log`.
- No forced continuation (`inject_message`, `continue`, ...); no judgment logic; any failure is swallowed.

## Live gate

`APA_REQUIRE_LIVE=1 python3 -m pytest -q plugins/hermes/tests/capture/test_live.py` (Hermes at `HERMES_SRC`).
