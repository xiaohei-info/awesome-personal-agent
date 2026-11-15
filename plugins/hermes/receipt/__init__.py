"""APA receipt plugin for Hermes: after a dispatching tool call it records the dispatch (`apa dispatch add`, design §6.17)
and forwards the result as `apa report`; `subagent_stop` forwards a sub-agent's result. No decisions here: the kernel
decides what a dispatch or a report means.

Captured only when the arguments carry the id of an item this Hermes session delegated (apa_delegate) or dispatched:
delegate_task and the paseo commands Hermes knows (tools/dispatch.py) record a dispatch, then report; any other terminal
command naming such an item only reports, and `apa` itself is never captured. No such item id, no capture. Everything
runs detached and prints nothing, so the model never sees it; any failure is swallowed and never reaches Hermes.

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

def _load(name, file):
    spec = importlib.util.spec_from_file_location(name, pathlib.Path(__file__).resolve().parents[1] / "tools" / file)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


run_context = _load("apa_run_context", "run_context.py")
dispatch = _load("apa_dispatch", "dispatch.py")  # its per-process state is shared with apa-tools through sys.modules

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
            _log(item, args, code, reason + " (gave up)")
        elif attempts == 0:  # logged once, not on every failed re-send
            _log(item, args, code, reason + " (kept, re-sent at later hooks until the kernel answers)")
        return
    _log(item, args, code, reason + (" (re-send)" if attempts else ""))


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
    for item, body, lost in dispatch.capture(tool_name, args, result, session_id):
        if body is None:
            if lost:
                argv = [apa, "dispatch", "update", lost, "--state", "lost"]
                env = _env(item, argv, session_id)
                if env is not None:
                    out.append((item, argv, env, 0, None, None))
            continue
        key = "hermes-dispatch-" + hashlib.sha1(f"{item}|{json.dumps(body, sort_keys=True)}".encode()).hexdigest()[:16]
        argv = [apa, "dispatch", "add", item, "--idempotency-key", key]
        for flag in ("session", "cwd", "continues"):
            if body.get(flag):
                argv += ["--" + flag, body[flag]]
        if body.get("resumable"):
            argv.append("--resumable")
        argv += ["--handle", "-"]
        env = _env(item, argv, session_id)
        if env is None:
            continue
        session = body.get("session")

        def remember(stdout, item=item, session=session):
            d = json.loads(stdout)
            dispatch.remember_dispatch(session_id, item, session, d.get("id"))

        dispatch.remember_dispatch(session_id, item, session, None)
        out.append((item, argv, env, 0, json.dumps(body["handle"]), remember))
    return out


def _report(item, outcome, summary, session_id, before=()):
    """Report a result; before are calls (dispatch records) that must reach the kernel first."""
    if (item, outcome) in _sent:
        if before:
            _sequence(before)
        return
    _sent.add((item, outcome))
    summary = (summary or "").strip()[-SUMMARY_MAX:]
    ev = _evidence(summary) if outcome == "done" else {}
    if outcome == "done" and not ev:
        outcome = "progress"  # the kernel refuses done without result evidence
    key = "hermes-receipt-" + hashlib.sha1(f"{item}|{outcome}|{summary}".encode()).hexdigest()[:16]
    args = [os.environ.get("APA_BIN", "apa"), "report", item, "--outcome", outcome, "--idempotency-key", key]
    try:
        env = run_context.apa_env()
    except ValueError as e:
        _log(item, args, "-", str(e).splitlines()[0])
        return
    if session_id:
        env["APA_SESSION"] = str(session_id)
    if summary:
        args += ["--summary", summary]
    for k, v in ev.items():
        args += [k, v]
    _sequence([*before, (item, args, env, 0, None, None)])


def _parsed(result):
    try:
        v = json.loads(result) if isinstance(result, str) else result
    except ValueError:
        return {}
    return v if isinstance(v, dict) else {}


def _mine(session_id):
    return dispatch.delegated(session_id) | dispatch.dispatched(session_id)


def _split(calls):
    by = {}
    for c in calls:
        by.setdefault(c[0], []).append(c)
    return by


def _delegate(args, result, session_id):
    before = _split(_dispatch_calls("delegate_task", args, result, session_id))
    tasks = args.get("tasks") if isinstance(args.get("tasks"), list) else [args]
    entries = {e.get("task_index", i): e for i, e in enumerate(_parsed(result).get("results") or []) if isinstance(e, dict)}
    mine = _mine(session_id)
    for i, t in enumerate(tasks):
        item = _item(t) if isinstance(t, dict) else None
        e = entries.get(i)
        if item and e and item in mine:
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
    before = _split(_dispatch_calls("terminal", args, result, session_id))
    mine = _mine(session_id)
    r = _parsed(result)
    ok = r.get("exit_code") == 0 and not r.get("error")
    for item in dict.fromkeys(ITEM_ID.findall(command)):
        if item in mine:
            _report(item, "done" if ok else "blocked", str(r.get("output") or "") if ok else str(r.get("error") or r.get("output") or ""),
                    session_id, before.pop(item, ()))
    for calls in before.values():
        _sequence(calls)


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
