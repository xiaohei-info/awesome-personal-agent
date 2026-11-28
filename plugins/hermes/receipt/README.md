# apa-receipt (Hermes plugin)

Install (next to `apa-core`): `ln -s "$PWD" ~/.hermes/plugins/apa-receipt`, then `hermes plugins enable apa-receipt`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`.

Records dispatches and forwards results for executors that do not run `apa report` themselves (design §6.17). It keeps no
per-process memory of what the session delegated: which item a dispatch belongs to is the kernel's to know.

- `post_tool_call` on `delegate_task` and on the paseo commands in `tools/README.md` records every dispatch it recognises
  (`apa dispatch add`): on the delegated item the call names, or, naming none (or naming the parent or another item), as an
  unattached dispatch under its executor id with that item as `--about`. The kernel shows unattached dispatches to the
  session every turn until the model attaches them (`apa dispatch attach`) or marks them untracked. `paseo send <agent>`
  records `--session <agent>` and the kernel continues the item that session's earlier dispatch is on; when paseo says the
  agent is gone it sends `apa dispatch update --session <agent> --state lost` instead.
- Results: `delegate_task`'s own result and `subagent_stop` (when the child summary names an item id) report to the item
  they name. A `terminal` command that runs `paseo wait <id>` or `paseo logs <id>` and exits 0 reports by the session
  handle (`apa report --session <id>`); the kernel finds the item from the dispatch record and refuses (logged, not retried)
  while the dispatch is still unattached. Other terminal commands are not reports; `apa ...` itself is never captured.

`completed` / exit 0 reports `done` with the result as summary and the evidence found in it (`--pr`, `--commit`, `--branch`, `--link`);
with none found the summary itself is the result (still `done`). Anything else reports `blocked` with the error.
The same target and outcome is reported once per process; the kernel also gets an idempotency key. The kernel writes each report
on a delegated item as progress on its parent too.

Failures: every non-zero `apa` exit is written to `$APA_RECEIPT_LOG` (default `$APA_HOME/hermes-receipt.log`, `~/.apa` when unset),
read from the environment of that call, so a detached retry after the caller's environment changed (tests included) never writes
elsewhere: command, item id, exit code, reason; no credentials or conversation text. Exit 7 (kernel unreachable) keeps the request
and its idempotency key and re-sends it at later hooks until the kernel answers (at most 5 re-sends, 50 pending); a refusal
(2, 4, 6, 23 ...) is only logged and not retried, also on a re-send. A success is never repeated. Pending requests live in memory.

No recognised call, no capture. The `apa` calls run in their own session on a background thread with all output discarded, so the
model sees nothing; any failure, missing binary or exception is swallowed. The kernel judges each call (credential binding, harness of
the session handle, exit 4 ...).

## Live gate

`APA_REQUIRE_LIVE=1 python3 -m pytest -q plugins/hermes/tests/receipt/test_receipt_live.py` (Hermes at `HERMES_SRC`, Go installed).
