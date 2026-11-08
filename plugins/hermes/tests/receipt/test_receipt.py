"""Unit tests: the receipt hooks against a stand-in `apa` executable (test boundary only)."""
import importlib.util
import json
import pathlib
import stat
import sys
import time

import pytest

RECEIPT = pathlib.Path(__file__).resolve().parents[2] / "receipt"
ITEM = "itm_0a1b2c3d4e5f"


def load_receipt():
    """Import plugins/hermes/receipt as package 'apa_receipt' (fresh state each time)."""
    for k in [k for k in sys.modules if k == "apa_receipt" or k.startswith("apa_receipt.")]:
        del sys.modules[k]
    spec = importlib.util.spec_from_file_location("apa_receipt", RECEIPT / "__init__.py", submodule_search_locations=[str(RECEIPT)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_receipt"] = mod
    spec.loader.exec_module(mod)
    return mod


class Ctx:
    def __init__(self):
        self.hooks = {}

    def register_hook(self, name, cb):
        self.hooks[name] = cb


FAKE = """#!/bin/sh
python3 - "$@" <<'PY' >> "$APA_LOG"
import json, os, sys
print(json.dumps({"argv": sys.argv[1:], "session": os.environ.get("APA_SESSION")}))
open(os.environ["APA_LOG"] + ".harness", "a").write(os.environ.get("APA_HARNESS", "") + "\\n")
PY
echo "model must never see this"
# exit codes queued in $APA_LOG.codes are consumed one per call
if [ -s "$APA_LOG.codes" ]; then c=$(head -n1 "$APA_LOG.codes"); tail -n +2 "$APA_LOG.codes" > "$APA_LOG.codes.t"; mv "$APA_LOG.codes.t" "$APA_LOG.codes"; exit $c; fi
exit 0
"""


@pytest.fixture
def apa(tmp_path, monkeypatch):
    p = tmp_path / "apa"
    p.write_text(FAKE)
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    table = tmp_path / "executors.json"
    table.write_text(json.dumps([{"id": "work-host.cli", "kind": "external-cli", "scenarios": ["coding"],
                                  "command": ["/usr/bin/codex", "exec", "{prompt}"]}]))
    for k, v in {"APA_BIN": str(p), "APA_LOG": str(tmp_path / "log"), "APA_RECEIPT_LOG": str(tmp_path / "receipt.log"), "APA_HERMES_EXECUTORS": str(table)}.items():
        monkeypatch.setenv(k, v)
    return tmp_path


@pytest.fixture
def hooks():
    ctx = Ctx()
    load_receipt().register(ctx)
    return ctx.hooks


def calls(tmp, n, wait=5):
    """The reports the detached `apa` processes made (they run asynchronously)."""
    log = tmp / "log"
    end = time.time() + wait
    while time.time() < end:
        got = [json.loads(l) for l in log.read_text().splitlines()] if log.exists() else []
        if len(got) >= n:
            time.sleep(0.2)  # a surplus report would show up now
            return [json.loads(l) for l in log.read_text().splitlines()]
        time.sleep(0.05)
    return [json.loads(l) for l in log.read_text().splitlines()] if log.exists() else []


def flag(argv, name):
    return argv[argv.index(name) + 1]


def test_registers_exactly_the_two_hooks(hooks):
    assert set(hooks) == {"post_tool_call", "subagent_stop"}


def test_delegate_task_with_item_id_reports_each_task_to_its_own_item(apa, hooks):
    other = "itm_ffee00112233"
    result = json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "PR #12 opened"},
                                     {"task_index": 1, "status": "failed", "error": "tests red", "summary": None}]})
    out = hooks["post_tool_call"](tool_name="delegate_task", session_id="main-1", result=result,
                                  args={"tasks": [{"goal": f"fix it\n\nLedger item: {ITEM}"}, {"goal": f"other {other}"}]})
    assert out is None  # nothing for the model
    got = sorted(calls(apa, 2), key=lambda c: c["argv"][1])
    assert [c["argv"][:2] for c in got] == sorted([["report", ITEM], ["report", other]], key=lambda a: a[1])
    by = {c["argv"][1]: c for c in got}
    assert flag(by[ITEM]["argv"], "--outcome") == "done" and flag(by[ITEM]["argv"], "--summary") == "PR #12 opened"
    assert flag(by[other]["argv"], "--outcome") == "blocked" and flag(by[other]["argv"], "--summary") == "tests red"
    assert by[ITEM]["session"] == "main-1" and flag(by[ITEM]["argv"], "--idempotency-key").startswith("hermes-receipt-")


def test_receipt_report_carries_harness_hermes(apa, hooks):
    hooks["post_tool_call"](tool_name="delegate_task", session_id="main-1",
                            result=json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "ok"}]}),
                            args={"tasks": [{"goal": f"x\n\nLedger item: {ITEM}"}]})
    assert len(calls(apa, 1)) == 1
    assert (apa / "log.harness").read_text().split() == ["hermes"]


def test_no_item_id_means_no_capture(apa, hooks):
    hooks["post_tool_call"](tool_name="delegate_task", session_id="s", args={"goal": "research X"},
                            result=json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "ok"}]}))
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": "codex exec 'no id here'"},
                            result=json.dumps({"output": "x", "exit_code": 0}))
    hooks["subagent_stop"](parent_session_id="s", child_summary="found nothing", child_status="completed")
    assert calls(apa, 1, wait=0.5) == []


def test_non_executor_tools_are_ignored_even_with_an_item_id(apa, hooks):
    hooks["post_tool_call"](tool_name="read_file", session_id="s", args={"path": f"/tmp/{ITEM}"}, result="{}")
    # the parent's own apa call and a command that is not a configured executor
    for cmd in (f"apa delegate --parent {ITEM}", f"ls {ITEM}"):
        hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": cmd},
                                result=json.dumps({"output": "x", "exit_code": 0}))
    assert calls(apa, 1, wait=0.5) == []


def test_terminal_running_a_configured_executor_reports_by_exit_code(apa, hooks):
    cmd = f"codex exec 'work on it. Ledger item: {ITEM}'"
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": cmd},
                            result=json.dumps({"output": "all green, PR #7", "exit_code": 0, "error": None}))
    (c,) = calls(apa, 1)
    assert c["argv"][:2] == ["report", ITEM] and flag(c["argv"], "--outcome") == "done" and flag(c["argv"], "--summary") == "all green, PR #7"


def test_terminal_failure_reports_blocked(apa, hooks):
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": f"codex exec 'x {ITEM}'"},
                            result=json.dumps({"output": "", "exit_code": 2, "error": "boom"}))
    (c,) = calls(apa, 1)
    assert flag(c["argv"], "--outcome") == "blocked" and flag(c["argv"], "--summary") == "boom"


def test_subagent_stop_reports_to_the_parent_session(apa, hooks):
    hooks["subagent_stop"](parent_session_id="parent-9", child_summary=f"{ITEM} finished, PR #3", child_status="completed")
    (c,) = calls(apa, 1)
    assert c["argv"][:2] == ["report", ITEM] and c["session"] == "parent-9" and flag(c["argv"], "--outcome") == "done"


def test_same_result_seen_by_both_hooks_is_reported_once(apa, hooks):
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} done", child_status="completed")
    hooks["post_tool_call"](tool_name="delegate_task", session_id="s", args={"goal": f"go {ITEM}"},
                            result=json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "done"}]}))
    assert len(calls(apa, 2, wait=1)) == 1


def test_hook_failures_are_swallowed(apa, hooks, monkeypatch):
    monkeypatch.setenv("APA_BIN", "/nonexistent/apa")
    args = {"goal": f"go {ITEM}"}
    ok = json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "x"}]})
    assert hooks["post_tool_call"](tool_name="delegate_task", session_id="s", args=args, result=ok) is None  # missing binary
    for bad in (None, "not json", 5):
        assert hooks["post_tool_call"](tool_name="delegate_task", session_id="s", args=args, result=bad) is None
        assert hooks["post_tool_call"](tool_name="terminal", session_id="s", args="junk", result=bad) is None
    assert hooks["subagent_stop"](parent_session_id=None, child_summary=None, child_status=None) is None


def test_hook_returns_without_waiting_for_apa(apa, hooks, monkeypatch):
    slow = apa / "slow"
    slow.write_text("#!/bin/sh\nsleep 5\n")
    slow.chmod(slow.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("APA_BIN", str(slow))
    t = time.monotonic()
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} done", child_status="completed")
    assert time.monotonic() - t < 1


def report_of(tmp, summary):
    ctx = Ctx()
    load_receipt().register(ctx)
    ctx.hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} {summary}", child_status="completed")
    (c,) = calls(tmp, 1)
    return c["argv"]


@pytest.mark.parametrize("summary,flag_name,value", [
    ("opened https://github.com/o/r/pull/9", "--pr", "https://github.com/o/r/pull/9"),
    ("pushed commit 3f2a9c1d", "--commit", "3f2a9c1d"),
    ("work is on branch feat/receipt-x.", "--branch", "feat/receipt-x"),
    ("see https://example.com/report/1", "--link", "https://example.com/report/1"),
])
def test_done_carries_the_extracted_result_evidence(apa, summary, flag_name, value):
    argv = report_of(apa, summary)
    assert flag(argv, "--outcome") == "done" and flag(argv, flag_name) == value


def test_done_without_any_result_is_reported_as_progress(apa):
    argv = report_of(apa, "finished, everything looks good")
    assert flag(argv, "--outcome") == "progress"
    assert not {"--pr", "--commit", "--branch", "--link"} & set(argv)


def test_failed_executor_is_blocked_without_evidence(apa):
    ctx = Ctx()
    load_receipt().register(ctx)
    ctx.hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} crashed, see PR #1", child_status="failed")
    (c,) = calls(apa, 1)
    assert flag(c["argv"], "--outcome") == "blocked" and "--pr" not in c["argv"]


def log_lines(tmp, n, wait=5):
    f = tmp / "receipt.log"
    end = time.time() + wait
    while time.time() < end:
        if f.exists() and len(f.read_text().splitlines()) >= n:
            return f.read_text().splitlines()
        time.sleep(0.05)
    return f.read_text().splitlines() if f.exists() else []


@pytest.mark.parametrize("code", [2, 4, 23])
def test_kernel_rejection_is_logged_and_never_resent(apa, hooks, code):
    (apa / "log.codes").write_text(f"{code}\n")
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    (line,) = log_lines(apa, 1)
    assert f"item={ITEM}" in line and f"exit={code}" in line and "report" in line
    assert "PR #1" not in line and "TOKEN" not in line  # no conversation text
    hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
    assert len(calls(apa, 2, wait=1)) == 1


def test_unreachable_kernel_keeps_the_request_and_resends_once_with_the_same_key(apa, hooks):
    (apa / "log.codes").write_text("7\n")
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    log_lines(apa, 1)
    other = "itm_00aa00bb00cc"
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{other} PR #2", child_status="completed")  # next hook
    got = calls(apa, 3)
    assert len(got) == 3
    by_item = [c for c in got if c["argv"][1] == ITEM]
    assert len(by_item) == 2 and flag(by_item[0]["argv"], "--idempotency-key") == flag(by_item[1]["argv"], "--idempotency-key")
    hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
    assert len(calls(apa, 4, wait=1)) == 3  # never a second re-send


def test_resend_that_is_unreachable_again_is_logged_and_dropped(apa, hooks):
    (apa / "log.codes").write_text("7\n7\n")
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    log_lines(apa, 1)
    hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
    assert len(log_lines(apa, 2)) == 2
    hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
    assert len(calls(apa, 3, wait=1)) == 2


def test_missing_binary_is_logged(apa, hooks, monkeypatch):
    monkeypatch.setenv("APA_BIN", "/nonexistent/apa")
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    (line,) = log_lines(apa, 1)
    assert f"item={ITEM}" in line and "cannot run apa" in line
