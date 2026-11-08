# apa-receipt (Hermes plugin)

Install (next to `apa-core`): `ln -s "$PWD" ~/.hermes/plugins/apa-receipt`, then `hermes plugins enable apa-receipt`.
Needs `apa` on PATH (or `APA_BIN`) plus `APA_URL` / `APA_TOKEN`.

Fallback for executors that do not run `apa report` themselves. Captured only when the arguments carry an item id (`itm_…`):

- `post_tool_call` on `delegate_task` (one report per task whose goal/context holds an id) and on `terminal` when the command
  starts with an `external-cli` executor from the owner's table (`$APA_HERMES_EXECUTORS`).
- `subagent_stop` when the child summary names an item id (the hook carries no arguments).

`completed` / exit 0 reports `done` with the result as summary and the evidence found in it (`--pr`, `--commit`, `--branch`, `--link`);
with none found it reports `progress` instead. Anything else reports `blocked` with the error.
The same item and outcome is reported once per process; the kernel also gets an idempotency key.

Failures: every non-zero `apa report` exit is written to `$APA_RECEIPT_LOG` (default `$APA_HOME/hermes-receipt.log`): command, item id,
exit code, reason; no credentials or conversation text. Exit 7 (kernel unreachable) keeps the request and its idempotency key and re-sends
it once at the next hook; a refusal (2, 4, 23 ...) is only logged, and so is a refused re-send. Pending requests live in memory.

No item id, no capture. The `apa report` runs detached with all output discarded, so the model sees nothing;
any failure, missing binary or exception is swallowed. The kernel judges the receipt (credential binding, exit 4 ...).

## Live gate

`APA_REQUIRE_LIVE=1 python3 -m pytest -q plugins/hermes/tests/receipt/test_live.py` (Hermes at `HERMES_SRC`, Go installed).
