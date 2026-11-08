"""APA receipt plugin for Hermes: `post_tool_call` / `subagent_stop` forward an executor's result to the
ledger as `apa report`. No decisions here: the kernel decides what a report means.

Only executor-class tools whose arguments carry an item id are captured (delegate_task; terminal running a
configured external-cli executor). No item id, no capture. The report runs detached and prints nothing, so
the model never sees it; any failure is swallowed and never reaches Hermes.
"""
import hashlib
import json
import os
import pathlib
import re
import shlex
import subprocess

ITEM_ID = re.compile(r"\bitm_[0-9a-f]+\b")
SUMMARY_MAX = 2000

_sent = set()  # (item, outcome): delegate_task fires subagent_stop and then post_tool_call for the same result


def _executor_commands():
    """argv[0] of every external-cli executor in the owner's table (same file the tools plugin reads)."""
    p = os.environ.get("APA_HERMES_EXECUTORS")
    p = pathlib.Path(p) if p else pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes")) / "apa-executors.json"
    try:
        items = json.loads(p.read_text())
        return {os.path.basename(e["command"][0]) for e in items if e.get("kind") == "external-cli" and e.get("command")}
    except (OSError, ValueError, TypeError, KeyError, IndexError, AttributeError):
        return set()


def _item(text):
    m = ITEM_ID.search(text if isinstance(text, str) else json.dumps(text, ensure_ascii=False, default=str))
    return m.group(0) if m else None


def _report(item, outcome, summary, session_id):
    if (item, outcome) in _sent:
        return
    _sent.add((item, outcome))
    summary = (summary or "").strip()[-SUMMARY_MAX:]
    key = "hermes-receipt-" + hashlib.sha1(f"{item}|{outcome}|{summary}".encode()).hexdigest()[:16]
    env = dict(os.environ, APA_HARNESS="hermes")
    if session_id:
        env["APA_SESSION"] = str(session_id)
    args = [os.environ.get("APA_BIN", "apa"), "report", item, "--outcome", outcome, "--idempotency-key", key]
    if summary:
        args += ["--summary", summary]
    subprocess.Popen(args, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                     start_new_session=True)


def _parsed(result):
    try:
        v = json.loads(result) if isinstance(result, str) else result
    except ValueError:
        return {}
    return v if isinstance(v, dict) else {}


def _delegate(args, result, session_id):
    tasks = args.get("tasks") if isinstance(args.get("tasks"), list) else [args]
    entries = {e.get("task_index", i): e for i, e in enumerate(_parsed(result).get("results") or []) if isinstance(e, dict)}
    for i, t in enumerate(tasks):
        item = _item(t) if isinstance(t, dict) else None
        e = entries.get(i)
        if item and e:
            ok = e.get("status") == "completed"
            _report(item, "done" if ok else "blocked", e.get("summary") if ok else (e.get("error") or e.get("summary")), session_id)


def _terminal(args, result, session_id):
    command = args.get("command") or ""
    item = _item(command)
    try:
        first = os.path.basename(shlex.split(command)[0])
    except (ValueError, IndexError):
        return
    if not item or first not in _executor_commands():
        return
    r = _parsed(result)
    ok = r.get("exit_code") == 0 and not r.get("error")
    _report(item, "done" if ok else "blocked", str(r.get("output") or "") if ok else str(r.get("error") or r.get("output") or ""), session_id)


def post_tool_call(tool_name="", args=None, result=None, session_id="", **_):
    try:
        args = args if isinstance(args, dict) else {}
        if tool_name == "delegate_task":
            _delegate(args, result, session_id)
        elif tool_name == "terminal":
            _terminal(args, result, session_id)
    except Exception:
        pass  # fail open: a receipt problem must never reach the conversation


def subagent_stop(parent_session_id="", child_summary="", child_status="", **_):
    try:
        item = _item(child_summary)  # the hook carries no tool arguments; the summary is the only place an id can ride
        if item:
            ok = child_status == "completed"
            _report(item, "done" if ok else "blocked", child_summary, parent_session_id)
    except Exception:
        pass


def register(ctx):
    ctx.register_hook("post_tool_call", post_tool_call)
    ctx.register_hook("subagent_stop", subagent_stop)
