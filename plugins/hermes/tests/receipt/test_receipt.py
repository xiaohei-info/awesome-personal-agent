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
    for k, v in {"APA_BIN": str(p), "APA_LOG": str(tmp_path / "log"), "APA_RECEIPT_LOG": str(tmp_path / "receipt.log")}.items():
        monkeypatch.setenv(k, v)
    return tmp_path


OTHER = "itm_ffee00112233"


@pytest.fixture(autouse=True)
def delegated():
    """What apa_delegate would have remembered: the items these Hermes sessions delegated (shared plugin state)."""
    import types
    st = sys.modules.setdefault("apa_hermes_dispatch_state", types.SimpleNamespace())
    st.lock, st.dispatched, st.by_session = __import__("threading").Lock(), {}, {}
    st.delegated = {s: {ITEM, OTHER} for s in ("main-1", "s", "parent-9")}
    return st


@pytest.fixture
def hooks():
    ctx = Ctx()
    load_receipt().register(ctx)
    return ctx.hooks


def all_calls(tmp, n, wait=5, keep=lambda c: True):
    """The `apa` calls the detached processes made (they run asynchronously)."""
    log = tmp / "log"
    read = lambda: [c for c in (json.loads(l) for l in log.read_text().splitlines()) if keep(c)] if log.exists() else []
    end = time.time() + wait
    while time.time() < end:
        if len(read()) >= n:
            time.sleep(0.2)  # a surplus call would show up now
            return read()
        time.sleep(0.05)
    return read()


def calls(tmp, n, wait=5):
    """The reports (dispatch records are checked separately)."""
    return all_calls(tmp, n, wait, keep=lambda c: c["argv"][0] == "report")


def flag(argv, name):
    return argv[argv.index(name) + 1]


def test_registers_exactly_the_two_hooks(hooks):
    assert set(hooks) == {"post_tool_call", "subagent_stop"}


def test_delegate_task_with_item_id_reports_each_task_to_its_own_item(apa, hooks):
    other = OTHER
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
    assert set((apa / "log.harness").read_text().split()) == {"hermes"}  # the dispatch record and the report


def test_no_item_id_means_no_capture(apa, hooks):
    hooks["post_tool_call"](tool_name="delegate_task", session_id="s", args={"goal": "research X"},
                            result=json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "ok"}]}))
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": "codex exec 'no id here'"},
                            result=json.dumps({"output": "x", "exit_code": 0}))
    hooks["subagent_stop"](parent_session_id="s", child_summary="found nothing", child_status="completed")
    assert calls(apa, 1, wait=0.5) == []


def test_non_executor_tools_are_ignored_even_with_an_item_id(apa, hooks):
    hooks["post_tool_call"](tool_name="read_file", session_id="s", args={"path": f"/tmp/{ITEM}"}, result="{}")
    # the parent's own apa call, and an item this session did not delegate
    for cmd in (f"apa delegate --parent {ITEM}", "ls itm_0000aaaa1111"):
        hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": cmd},
                                result=json.dumps({"output": "x", "exit_code": 0}))
    hooks["post_tool_call"](tool_name="terminal", session_id="other-session", args={"command": f"codex exec 'x {ITEM}'"},
                            result=json.dumps({"output": "x", "exit_code": 0}))
    assert all_calls(apa, 1, wait=0.5) == []


def test_terminal_naming_a_delegated_item_reports_by_exit_code(apa, hooks):
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


def test_resend_survives_a_still_down_kernel_and_stops_after_success(apa, hooks):
    (apa / "log.codes").write_text("7\n7\n0\n")
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    log_lines(apa, 1)
    for n in (2, 3):  # second hook: still down; third hook: kernel is back
        hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
        assert len(calls(apa, n)) == n
    got = calls(apa, 3)
    assert len({flag(c["argv"], "--idempotency-key") for c in got}) == 1
    assert len(log_lines(apa, 1, wait=0.5)) == 1  # logged once, not per failed re-send
    hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
    assert len(calls(apa, 4, wait=1)) == 3  # delivered once, never again


def test_resend_gives_up_after_a_bounded_number_of_tries(apa, hooks):
    (apa / "log.codes").write_text("7\n" * 50)
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    for n in range(2, 8):
        log_lines(apa, 1)
        calls(apa, n - 1)
        hooks["subagent_stop"](parent_session_id="s", child_summary="nothing", child_status="completed")
    time.sleep(1)
    assert len(calls(apa, 99, wait=0.5)) == 6  # 1 send + 5 re-sends
    assert "gave up" in (apa / "receipt.log").read_text()


@pytest.mark.parametrize("prose", [
    "I could not create a branch because tests failed",
    "nothing changed; no branch was created",
    "see the commit decade-old notes",
    "the commit added nothing",
    "no PR yet",
])
def test_prose_mentioning_branch_commit_or_pr_stays_progress(apa, prose):
    assert flag(report_of(apa, prose), "--outcome") == "progress"


@pytest.mark.parametrize("prose,value", [
    ("pushed to branch `feat-x`", "feat-x"),
    ("on branch: fix_y", "fix_y"),
    ("branch named hotfix", "hotfix"),
    ("changes are on branch agent/x1", "agent/x1"),
])
def test_recognisable_branch_forms_are_extracted(apa, prose, value):
    assert flag(report_of(apa, prose), "--branch") == value


def test_missing_binary_is_logged(apa, hooks, monkeypatch):
    monkeypatch.setenv("APA_BIN", "/nonexistent/apa")
    hooks["subagent_stop"](parent_session_id="s", child_summary=f"{ITEM} PR #1", child_status="completed")
    (line,) = log_lines(apa, 1)
    assert f"item={ITEM}" in line and "cannot run apa" in line


# ---- 委派记录的自动捕获（§6.17）：派出工具调用后先记委派，再回执；认不出的工具不记 ----

def dispatches(tmp, n, wait=5):
    return all_calls(tmp, n, wait, keep=lambda c: c["argv"][0] == "dispatch")


def test_paseo_run_records_a_dispatch_then_the_report(apa, hooks):
    cmd = f"paseo-profile-run --host mbp team-quick 'fix login. Ledger item: {ITEM}'"
    out = "AGENT ID      STATUS   PROVIDER  CWD        TITLE\nagt_42        idle     codex     /w/app     fix login\n"
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": cmd}, result=json.dumps({"output": out, "exit_code": 0}))
    got = all_calls(apa, 2)
    assert [c["argv"][:2] for c in got] == [["dispatch", "add"], ["report", ITEM]]  # dispatch first
    d = got[0]["argv"]
    assert d[2] == ITEM and flag(d, "--session") == "agt_42" and "--resumable" in d and flag(d, "--idempotency-key").startswith("hermes-dispatch-")
    assert (apa / "log.harness").read_text().split() == ["hermes", "hermes"]


def test_paseo_send_continues_the_last_dispatch_and_lost_marks_it(apa, hooks, delegated):
    delegated.by_session[(ITEM, "agt_42")] = "dsp_9"
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": f"paseo send --host mbp agt_42 'review comment for {ITEM}'"},
                            result=json.dumps({"output": "sent", "exit_code": 0}))
    (d,) = dispatches(apa, 1)
    assert flag(d["argv"], "--continues") == "dsp_9" and flag(d["argv"], "--session") == "agt_42"
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": f"paseo send agt_42 'again {ITEM}'"},
                            result=json.dumps({"output": "", "exit_code": 1, "error": "Agent agt_42 not found"}))
    got = dispatches(apa, 2)
    assert got[1]["argv"] == ["dispatch", "update", "dsp_9", "--state", "lost"]


def test_delegate_task_records_a_non_resumable_dispatch(apa, hooks):
    hooks["post_tool_call"](tool_name="delegate_task", session_id="s", args={"goal": f"go {ITEM}"},
                            result=json.dumps({"results": [{"task_index": 0, "status": "completed", "summary": "PR #5"}]}))
    (d,) = dispatches(apa, 1)
    assert d["argv"][:3] == ["dispatch", "add", ITEM] and "--resumable" not in d["argv"] and "--session" not in d["argv"]


def test_unrecognised_tools_record_no_dispatch(apa, hooks):
    hooks["post_tool_call"](tool_name="terminal", session_id="s", args={"command": f"codex exec 'x {ITEM}'"},
                            result=json.dumps({"output": "ok", "exit_code": 0}))
    hooks["post_tool_call"](tool_name="web_search", session_id="s", args={"query": ITEM}, result="{}")
    assert len(calls(apa, 1)) == 1 and dispatches(apa, 1, wait=0.5) == []
