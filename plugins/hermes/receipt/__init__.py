"""APA receipt plugin for Hermes: `post_tool_call` / `subagent_stop` forward an executor's result to the
ledger as `apa report`. No decisions here: the kernel decides what a report means.

Only executor-class tools whose arguments carry an item id are captured (delegate_task; terminal running a
configured external-cli executor). No item id, no capture. The report runs detached and prints nothing, so
the model never sees it; any failure is swallowed (written to the plugin-local log `apa-receipt.log` under
HERMES_HOME) and never reaches Hermes.

Evidence ("插件捕获"): the report reuses the session credential and carries
`--evidence "插件捕获：<tool> 对 <item> 的调用结果，会话 <id>"` plus any PR / commit / branch / link found in the
executor output. A success with no such reference is reported as `progress` (item stays open), never `done`.
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
# refs that prove work landed: URLs, PR #n, "commit <sha>", "branch <name>"
REFS = re.compile(r"https?://[^\s)\]>\"']+|\bPR\s*#\d+|\bcommit\s+[0-9a-f]{7,40}\b|\bbranch\s+[\w./-]+", re.I)

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


def _log(msg):
    """Plugin-local failure log; the model never sees it. Logging itself must not raise."""
    try:
        home = pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes"))
        with open(home / "apa-receipt.log", "a") as f:
            f.write(msg.rstrip() + "\n")
    except Exception:
        pass


def _log_file():
    home = pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes"))
    return open(home / "apa-receipt.log", "a")


def _item(text):
    m = ITEM_ID.search(text if isinstance(text, str) else json.dumps(text, ensure_ascii=False, default=str))
    return m.group(0) if m else None


def _report(item, outcome, summary, session_id, tool):
    summary = (summary or "").strip()[-SUMMARY_MAX:]
    refs = list(dict.fromkeys(m.group(0) for m in REFS.finditer(summary)))
    if outcome == "done" and not refs:
        outcome = "progress"  # no PR / commit / link to show: summary only, the child item stays open
    if (item, outcome) in _sent:
        return
    _sent.add((item, outcome))
    evidence = f"插件捕获：{tool} 对 {item} 的调用结果，会话 {session_id}" + ("；" + "；".join(refs) if refs else "")
    key = "hermes-receipt-" + hashlib.sha1(f"{item}|{outcome}|{summary}".encode()).hexdigest()[:16]
    env = dict(os.environ)
    if session_id:
        env["APA_SESSION"] = str(session_id)
    args = [os.environ.get("APA_BIN", "apa"), "report", item, "--outcome", outcome, "--idempotency-key", key,
            "--evidence", evidence]
    if summary:
        args += ["--summary", summary]
    try:
        log = _log_file()
    except OSError:
        log = subprocess.DEVNULL
    try:
        subprocess.Popen(args, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=log,
                         start_new_session=True)
    finally:
        if log is not subprocess.DEVNULL:
            log.close()  # the child keeps its own descriptor


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
            _report(item, "done" if ok else "blocked", e.get("summary") if ok else (e.get("error") or e.get("summary")), session_id, "delegate_task")


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
    _report(item, "done" if ok else "blocked", str(r.get("output") or "") if ok else str(r.get("error") or r.get("output") or ""), session_id, "terminal")


def post_tool_call(tool_name="", args=None, result=None, session_id="", **_):
    try:
        args = args if isinstance(args, dict) else {}
        if tool_name == "delegate_task":
            _delegate(args, result, session_id)
        elif tool_name == "terminal":
            _terminal(args, result, session_id)
    except Exception as e:
        _log(f"post_tool_call {tool_name}: {e!r}")  # fail open: a receipt problem must never reach the conversation


def subagent_stop(parent_session_id="", child_summary="", child_status="", **_):
    try:
        item = _item(child_summary)  # the hook carries no tool arguments; the summary is the only place an id can ride
        if item:
            ok = child_status == "completed"
            _report(item, "done" if ok else "blocked", child_summary, parent_session_id, "subagent_stop")
    except Exception as e:
        _log(f"subagent_stop: {e!r}")


def register(ctx):
    ctx.register_hook("post_tool_call", post_tool_call)
    ctx.register_hook("subagent_stop", subagent_stop)
