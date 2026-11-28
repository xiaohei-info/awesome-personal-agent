"""APA receipt plugin for Hermes: after a dispatching tool call it records the dispatch (`apa dispatch add`, design §6.17)
and forwards results as `apa report`; `subagent_stop` forwards a sub-agent's result. No decisions here: the kernel
decides what a dispatch or a report means and which item it belongs to.

delegate_task and the paseo commands Hermes knows (tools/dispatch.py) record a dispatch: on the delegated item the call
names, or, naming none, unattached under its executor (the kernel then reminds the session every turn until the model
attaches it). A result comes back two ways: delegate_task's own result and subagent_stop report to the item they name;
`paseo wait <id>` / `paseo logs <id>` report by the session handle, and the kernel finds the item from the dispatch
record. `apa` itself is never captured. Everything runs detached and prints nothing, so the model never sees it; any
failure is swallowed and never reaches Hermes.

`done` carries result evidence (PR, commit, branch or link) taken from the executor's output when there is any;
without it the summary is the result (the kernel accepts it on a delegated item). The exit code of `apa report` is watched off the hook thread: every failure goes to a
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

def _load(name, file):
    spec = importlib.util.spec_from_file_location(name, pathlib.Path(__file__).resolve().parents[1] / "tools" / file)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


run_context = _load("apa_run_context", "run_context.py")
dispatch = _load("apa_dispatch", "dispatch.py")

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
_sent = set()  # (item or session, outcome): delegate_task fires subagent_stop and then post_tool_call for the same result


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


def _log(item, args, code, reason, env=None):
    """One line: command (no summary text), item id, exit code, reason. The path comes from the environment the call was
    made with (env), not from whatever the process has when a detached thread gets here."""
    env = env if env is not None else os.environ
    cmd = " ".join(a for a in args[:5])
    path = env.get("APA_RECEIPT_LOG") or str(pathlib.Path(env.get("APA_HOME") or pathlib.Path.home() / ".apa") / "hermes-receipt.log")
    try:
        pathlib.Path(path).parent.mkdir(parents=True, exist_ok=True)
        with open(path, "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} cmd={cmd!r} item={item} exit={code} reason={reason}\n")
    except OSError:
        pass


def _run(item, args, env, attempts, stdin=None, after=None):
    """One `apa` call off the hook thread. after(stdout) runs on success (a dispatch id to remember)."""
    try:
        p = subprocess.run(args, env=env, input=stdin, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                           timeout=RUN_TIMEOUT, start_new_session=True, text=True)
        code = p.returncode
        reason = "kernel unreachable" if code == UNREACHABLE else "rejected by kernel"
    except subprocess.TimeoutExpired:
        code, reason = "-", "apa timed out"
    except OSError as e:
        code, reason = "-", f"cannot run apa: {e.__class__.__name__}"
    if code == 0:
        if after:
            try:
                after(p.stdout)
            except Exception:
                pass
        return
    if code == UNREACHABLE:
        with _lock:
            keep = attempts < MAX_RESENDS and len(_pending) < MAX_PENDING
            if keep:
                _pending.append((item, args, env, attempts + 1, stdin, after))
        if not keep:
            _log(item, args, code, reason + " (gave up)", env)
        elif attempts == 0:  # logged once, not on every failed re-send
            _log(item, args, code, reason + " (kept, re-sent at later hooks until the kernel answers)", env)
        return
    _log(item, args, code, reason + (" (re-send)" if attempts else ""), env)


def _spawn(item, args, env, attempts=0, stdin=None, after=None):
    threading.Thread(target=_run, args=(item, args, env, attempts, stdin, after), daemon=True).start()


def _sequence(calls):
    """Run several `apa` calls in order on one background thread (a dispatch is recorded before its report)."""
    def go():
        for c in calls:
            _run(*c)
    threading.Thread(target=go, daemon=True).start()


def _resend():
    with _lock:
        todo, _pending[:] = list(_pending), []
    for item, args, env, attempts, stdin, after in todo:
        _spawn(item, args, env, attempts, stdin, after)  # same args, so the same idempotency key


def _env(item, args, session_id):
    try:
        env = run_context.apa_env()
    except ValueError as e:
        _log(item, args, "-", str(e).splitlines()[0])
        return None
    if session_id:
        env["APA_SESSION"] = str(session_id)
    return env


def _dispatch_calls(tool_name, args, result, session_id):
    """`apa dispatch add` / `update --state lost` calls for a dispatching tool call (tools/dispatch.py decides what it is)."""
    out = []
    apa = os.environ.get("APA_BIN", "apa")
    for rec in dispatch.capture(tool_name, args, result, session_id):
        if "lost" in rec:
            argv = [apa, "dispatch", "update", "--session", rec["lost"], "--state", "lost"]
            env = _env(rec["lost"], argv, session_id)
            if env is not None:
                out.append((rec["lost"], argv, env, 0, None, None))
            continue
        item = rec.get("item")
        key = "hermes-dispatch-" + hashlib.sha1(json.dumps({**rec, "by": session_id}, sort_keys=True).encode()).hexdigest()[:16]
        argv = [apa, "dispatch", "add", *([item] if item else []), "--idempotency-key", key]
        for flag in ("session", "cwd", "assignee"):
            if rec.get(flag):
                argv += ["--" + flag, rec[flag]]
        if rec.get("executor"):
            argv += ["--executor", json.dumps(rec["executor"], sort_keys=True)]
        if rec.get("resumable"):
            argv.append("--resumable")
        argv += ["--handle", "-"]
        env = _env(item, argv, session_id)
        if env is not None:
            out.append((item or rec.get("session"), argv, env, 0, json.dumps(rec["handle"]), None))
    return out


def _report(target, outcome, summary, session_id, before=(), by_session=False):
    """Report a result to an item, or by_session to the dispatch with that session handle; before are calls (dispatch
    records) that must reach the kernel first."""
    if (target, outcome) in _sent:
        if before:
            _sequence(before)
        return
    _sent.add((target, outcome))
    summary = (summary or "").strip()[-SUMMARY_MAX:]
    ev = _evidence(summary) if outcome == "done" else {}
    key = "hermes-receipt-" + hashlib.sha1(f"{target}|{outcome}|{summary}".encode()).hexdigest()[:16]
    where = ["--session", target] if by_session else [target]
    args = [os.environ.get("APA_BIN", "apa"), "report", *where, "--outcome", outcome, "--idempotency-key", key]
    env = _env(target, args, session_id)
    if env is None:
        return
    if summary:
        args += ["--summary", summary]
    for k, v in ev.items():
        args += [k, v]
    _sequence([*before, (target, args, env, 0, None, None)])


def _parsed(result):
    try:
        v = json.loads(result) if isinstance(result, str) else result
    except ValueError:
        return {}
    return v if isinstance(v, dict) else {}


def _split(calls):
    by = {}
    for c in calls:
        by.setdefault(c[0], []).append(c)
    return by


def _delegate(args, result, session_id):
    before = _split(_dispatch_calls("delegate_task", args, result, session_id))
    tasks = args.get("tasks") if isinstance(args.get("tasks"), list) else [args]
    entries = {e.get("task_index", i): e for i, e in enumerate(_parsed(result).get("results") or []) if isinstance(e, dict)}
    for i, t in enumerate(tasks):
        item = _item(t) if isinstance(t, dict) else None
        e = entries.get(i)
        if item and e:
            ok = e.get("status") == "completed"
            _report(item, "done" if ok else "blocked", e.get("summary") if ok else (e.get("error") or e.get("summary")), session_id,
                    before.pop(item, ()))
    for calls in before.values():
        _sequence(calls)


def _terminal(args, result, session_id):
    command = args.get("command") or ""
    try:
        first = os.path.basename(shlex.split(command)[0])
    except (ValueError, IndexError):
        return
    if first == "apa":
        return
    calls = _dispatch_calls("terminal", args, result, session_id)
    if calls:
        _sequence(calls)
    for agent, output, ok in dispatch.completions("terminal", args, result):
        if ok:  # a wait that timed out or failed says nothing about the work
            _report(agent, "done", output, session_id, by_session=True)


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
