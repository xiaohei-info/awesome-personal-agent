"""APA receipt plugin for Hermes: `post_tool_call` / `subagent_stop` forward an executor's result to the
ledger as `apa report`. No decisions here: the kernel decides what a report means.

Only executor-class tools whose arguments carry an item id are captured (delegate_task; terminal running a
configured external-cli executor). No item id, no capture. The report runs detached and prints nothing, so
the model never sees it; any failure is swallowed and never reaches Hermes.

`done` needs result evidence (PR, commit, branch or link) taken from the executor's output; without any it is
reported as `progress`. The exit code of `apa report` is watched off the hook thread: every failure goes to a
plugin-local log (command, item id, exit code, reason; no credentials, no conversation text). Exit 7 (kernel
unreachable) keeps the request and its idempotency key and re-sends it at later hooks until the kernel answers
(success, or a refusal which is only logged); it is given up after MAX_RESENDS tries. Once sent successfully it is never sent again.
"""
import hashlib
import importlib.util
import json
import os
import pathlib
import re
import shlex
import subprocess
import threading
import time

_spec = importlib.util.spec_from_file_location("apa_run_context", pathlib.Path(__file__).resolve().parents[1] / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)

ITEM_ID = re.compile(r"\bitm_[0-9a-f]+\b")
SUMMARY_MAX = 2000

PR_URL = re.compile(r"https?://[^\s)>\]]+/pull/\d+")
PR_REF = re.compile(r"\bPR\s*(#\d+)", re.I)
COMMIT = re.compile(r"\bcommit\s+`?((?=[0-9a-f]*\d)[0-9a-f]{7,40})\b", re.I)  # needs a digit: "commit decade" is prose
# a branch only when it is recognisably a ref: quoted, after "branch:" / "branch named", or containing "/"
BRANCH = re.compile(r"\bbranch\s*(?::\s*|named\s+|\s)(?:[`'\"]([\w./-]+)[`'\"]|(?:(?<=:\s)|(?<=named\s))([\w][\w./-]*)|([\w.-]+/[\w./-]*))", re.I)
LINK = re.compile(r"https?://[^\s)>\]]+")
UNREACHABLE = 7
MAX_RESENDS = 5  # hooks that may find the kernel still down before the request is given up
MAX_PENDING = 50
RUN_TIMEOUT = 60  # off the hook thread; only bounds a hung apa

_lock = threading.Lock()
_pending = []  # (item, args, env, attempts) kept after exit 7 until the kernel answers; in memory: lost when Hermes exits
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


def _evidence(text):
    ev = {}
    m = PR_URL.search(text) or PR_REF.search(text)
    if m:
        ev["--pr"] = m.group(m.lastindex or 0)
    m = COMMIT.search(text)
    if m:
        ev["--commit"] = m.group(1)
    m = BRANCH.search(text)
    if m:
        ev["--branch"] = next(g for g in m.groups() if g).rstrip(".")
    links = [u for u in LINK.findall(text) if not PR_URL.fullmatch(u)]
    if links:
        ev["--link"] = links[0]
    return ev


def _log(item, args, code, reason):
    """One line: command (no summary text), item id, exit code, reason."""
    cmd = " ".join(a for a in args[:5])
    path = os.environ.get("APA_RECEIPT_LOG") or str(pathlib.Path(os.environ.get("APA_HOME") or pathlib.Path.home() / ".apa") / "hermes-receipt.log")
    try:
        pathlib.Path(path).parent.mkdir(parents=True, exist_ok=True)
        with open(path, "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} cmd={cmd!r} item={item} exit={code} reason={reason}\n")
    except OSError:
        pass


def _run(item, args, env, attempts):
    try:
        code = subprocess.run(args, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL, timeout=RUN_TIMEOUT, start_new_session=True).returncode
        reason = "kernel unreachable" if code == UNREACHABLE else "rejected by kernel"
    except subprocess.TimeoutExpired:
        code, reason = "-", "apa timed out"
    except OSError as e:
        code, reason = "-", f"cannot run apa: {e.__class__.__name__}"
    if code == 0:
        return
    if code == UNREACHABLE:
        with _lock:
            keep = attempts < MAX_RESENDS and len(_pending) < MAX_PENDING
            if keep:
                _pending.append((item, args, env, attempts + 1))
        if not keep:
            _log(item, args, code, reason + " (gave up)")
        elif attempts == 0:  # logged once, not on every failed re-send
            _log(item, args, code, reason + " (kept, re-sent at later hooks until the kernel answers)")
        return
    _log(item, args, code, reason + (" (re-send)" if attempts else ""))


def _spawn(item, args, env, attempts=0):
    threading.Thread(target=_run, args=(item, args, env, attempts), daemon=True).start()


def _resend():
    with _lock:
        todo, _pending[:] = list(_pending), []
    for item, args, env, attempts in todo:
        _spawn(item, args, env, attempts)  # same args, so the same idempotency key


def _report(item, outcome, summary, session_id):
    if (item, outcome) in _sent:
        return
    _sent.add((item, outcome))
    summary = (summary or "").strip()[-SUMMARY_MAX:]
    ev = _evidence(summary) if outcome == "done" else {}
    if outcome == "done" and not ev:
        outcome = "progress"  # the kernel refuses done without result evidence
    key = "hermes-receipt-" + hashlib.sha1(f"{item}|{outcome}|{summary}".encode()).hexdigest()[:16]
    env = run_context.apa_env()
    if session_id:
        env["APA_SESSION"] = str(session_id)
    args = [os.environ.get("APA_BIN", "apa"), "report", item, "--outcome", outcome, "--idempotency-key", key]
    if summary:
        args += ["--summary", summary]
    for k, v in ev.items():
        args += [k, v]
    _spawn(item, args, env)


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
        _resend()
        args = args if isinstance(args, dict) else {}
        if tool_name == "delegate_task":
            _delegate(args, result, session_id)
        elif tool_name == "terminal":
            _terminal(args, result, session_id)
    except Exception:
        pass  # fail open: a receipt problem must never reach the conversation


def subagent_stop(parent_session_id="", child_summary="", child_status="", **_):
    try:
        _resend()
        item = _item(child_summary)  # the hook carries no tool arguments; the summary is the only place an id can ride
        if item:
            ok = child_status == "completed"
            _report(item, "done" if ok else "blocked", child_summary, parent_session_id)
    except Exception:
        pass


def register(ctx):
    ctx.register_hook("post_tool_call", post_tool_call)
    ctx.register_hook("subagent_stop", subagent_stop)
