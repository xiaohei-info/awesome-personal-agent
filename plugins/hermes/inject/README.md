# apa-inject (Hermes plugin)

Install (next to `apa-core`): `ln -s "$PWD" ~/.hermes/plugins/apa-inject`, then `hermes plugins enable apa-inject`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`.

`pre_llm_call` runs `apa context --session <id> --scope cwd --format inject` and returns the text as
`{"context": ...}` (Hermes puts it in the user message). The kernel owns increments, cursors and the envelope;
the plugin only passes the session through.

- Every call is capped at 2 s; any failure, non-zero exit or missing binary injects nothing and never blocks the message.
- Empty increment: no output.
- Session start, resume (`hermes chat --resume`, a session this process has not seen) and compaction
  (`post_compaction`) start a fresh kernel cursor (`<session>~<ns>`), so the full summary is injected again, plus
  `--instruction <version>` (the kernel adds the update notice if stale; override with `APA_INSTRUCTION_VERSION`)
  and `apa agents check`. Its output is not injected.
- Does not write harness memory.

## Live gate

`APA_REQUIRE_LIVE=1 python3 -m pytest -q plugins/hermes/tests/inject/test_live.py` (Hermes at `HERMES_SRC`, Go installed;
without the flag it skips with the reason).
