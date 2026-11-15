"""Hermes' own ways of delegating, as data for the ledger (design §6.17). There is no "executor id -> invocation" table:
Hermes decides how to delegate. This module only

- derives a stable executor id, label and handle from the way Hermes delegated (the model names the mech and its
  parameters, never the id);
- recognises Hermes' dispatching tool calls after the fact (capture) and turns them into dispatch records;
- checks handles against Hermes' own shapes before writing, and turns handles Hermes itself wrote back into resume
  hints for Hermes' own tools. Anything else is shown as reference only.

Shared by the apa-tools and apa-receipt plugins; the per-process state lives in sys.modules so both see the same one.
"""
import hashlib
import json
import os
import re
import shlex
import socket
import sys
import threading
import types

HARNESS = "hermes"
MECHS = ("delegate_task", "paseo")
PASEO_PROGRAMS = ("paseo", "paseo-profile-run")

ITEM_ID = re.compile(r"\bitm_[0-9a-f]+\b")
_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$")      # profile, provider, model
_AGENT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")       # paseo agent id
_TARGET = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$")  # paseo --host value (daemon endpoint or ssh host)
_LOST = re.compile(r"not found|no such agent|unknown agent|does not exist", re.I)

_state = sys.modules.setdefault("apa_hermes_dispatch_state", types.SimpleNamespace(
    lock=threading.Lock(),
    delegated={},   # Hermes session id -> item ids this session delegated (apa_delegate)
    dispatched={},  # Hermes session id -> item ids this session recorded a dispatch for
    by_session={},  # (item, session) -> latest dispatch id written from this process
))


def _add(table, session_id, item):
    with _state.lock:
        table.setdefault(session_id or "", set()).add(item)


def remember_delegated(session_id, item):
    _add(_state.delegated, session_id, item)


def remember_dispatch(session_id, item, session, dispatch_id):
    _add(_state.dispatched, session_id, item)
    if dispatch_id:
        with _state.lock:
            _state.by_session[(item, session or "")] = dispatch_id


def delegated(session_id):
    with _state.lock:
        return set(_state.delegated.get(session_id or "", ()))


def dispatched(session_id):
    with _state.lock:
        return set(_state.dispatched.get(session_id or "", ()))


def last_dispatch(item, session):
    with _state.lock:
        return _state.by_session.get((item, session or ""))


def _seg(s):
    """One id segment: lower case, [a-z0-9_-] only."""
    return re.sub(r"[^a-z0-9_-]+", "-", str(s).lower()).strip("-") or "x"


def local_host():
    return _seg(socket.gethostname().split(".")[0])


def _host_of(target):
    """Machine name for the ledger from a paseo --host value (user@host:port, ssh alias ...)."""
    t = target.split("@")[-1].split(":")[0].split("/")[0]
    return _seg(t.split(".")[0])


def _executor_id(mech, host, *variant):
    eid = ".".join([HARNESS, mech, host, *(_seg(v) for v in variant if v)])
    if len(eid) > 64:  # keep it stable and within the kernel's limit
        eid = eid[:55] + "." + hashlib.sha1(eid.encode()).hexdigest()[:8]
    return eid


class Invalid(ValueError):
    pass


def executor(mech, params):
    """(assignee id, enrollment) for one way of delegating. The same mech and parameters always give the same id."""
    if mech == "delegate_task":
        host = local_host()
        return _executor_id(mech, host), {"kind": "native-subagent", "host": host, "label": f"Hermes 子 agent @ {host}",
                                          "handle": {"mech": mech, "host": host}}
    if mech != "paseo":
        raise Invalid(f"unknown mech {mech!r}; Hermes knows {', '.join(MECHS)}")
    target = params.get("host") or ""
    if target and not _TARGET.match(target):
        raise Invalid("host must be a paseo host name or endpoint without spaces")
    host = _host_of(target) if target else local_host()
    profile, provider, model = (params.get(k) or "" for k in ("profile", "provider", "model"))
    for k, v in (("profile", profile), ("provider", provider), ("model", model)):
        if v and not _NAME.match(v):
            raise Invalid(f"{k} has characters a paseo {k} cannot have")
    handle = {"mech": mech, "host": host}
    if target:
        handle["target"] = target
    if profile:
        handle["profile"] = profile
        variant, what = [profile], profile
    elif provider:
        handle["provider"] = provider
        if model:
            handle["model"] = model
        variant, what = [provider, model], provider + (f"/{model}" if model else "")
    else:
        raise Invalid("paseo needs profile, or provider (and optionally model)")
    return _executor_id(mech, host, *variant), {"kind": "remote-agent", "host": host, "label": f"Paseo · {what} @ {host}", "handle": handle}


_SHAPES = {
    "delegate_task": {"mech", "host"},
    "paseo": {"mech", "host", "target", "agent_id", "profile", "provider", "model"},
}


def check_handle(h):
    """Raise Invalid unless h is one of Hermes' own handle shapes with well-formed values."""
    if not isinstance(h, dict) or h.get("mech") not in _SHAPES:
        raise Invalid(f"handle.mech must be one of {', '.join(MECHS)}")
    extra = set(h) - _SHAPES[h["mech"]]
    if extra:
        raise Invalid(f"handle has fields Hermes does not use for {h['mech']}: {', '.join(sorted(extra))}")
    for k, v in h.items():
        if not isinstance(v, str):
            raise Invalid(f"handle.{k} must be a string")
    if "agent_id" in h and not _AGENT.match(h["agent_id"]):
        raise Invalid("handle.agent_id is not a paseo agent id")
    if "target" in h and not _TARGET.match(h["target"]):
        raise Invalid("handle.target is not a paseo host")
    for k in ("host", "profile", "provider", "model"):
        if k in h and not _NAME.match(h[k]):
            raise Invalid(f"handle.{k} is malformed")


# ---- capture after a dispatching tool call ----

def _items(text, session_id):
    mine = delegated(session_id)
    return [i for i in dict.fromkeys(ITEM_ID.findall(text if isinstance(text, str) else json.dumps(text, default=str))) if i in mine]


def _parsed(result):
    try:
        v = json.loads(result) if isinstance(result, str) else result
    except ValueError:
        return {}
    return v if isinstance(v, dict) else {}


def _agent_id(output):
    """The agent id `paseo run` printed: JSON {agentId} or the first column under the AGENT ID header."""
    out = output or ""
    try:
        v = json.loads(out)
        v = v[0] if isinstance(v, list) and v else v
        if isinstance(v, dict) and _AGENT.match(str(v.get("agentId", ""))):
            return v["agentId"]
    except ValueError:
        pass
    lines = [l for l in out.splitlines() if l.strip()]
    for i, l in enumerate(lines):
        if l.strip().upper().startswith("AGENT ID") and i + 1 < len(lines):
            first = lines[i + 1].split()[0]
            return first if _AGENT.match(first) else None
    return None


def _paseo(argv):
    """Parse a paseo dispatch command: (verb, opts, positionals) or None."""
    prog = os.path.basename(argv[0])
    rest = argv[1:]
    if prog == "paseo":
        if not rest or rest[0] not in ("run", "send"):
            return None
        verb, rest = rest[0], rest[1:]
    elif prog == "paseo-profile-run":
        verb = "profile-run"
    else:
        return None
    opts, pos, i = {}, [], 0
    while i < len(rest):
        a = rest[i]
        if a.startswith("--"):
            key, eq, val = a[2:].partition("=")
            if not eq and i + 1 < len(rest) and not rest[i + 1].startswith("--") and key not in ("no-wait", "json"):
                val, i = rest[i + 1], i + 1
            if key == "label" and val.startswith("apa.profile="):
                opts["profile"] = val.split("=", 1)[1]
            else:
                opts[key] = val
        else:
            pos.append(a)
        i += 1
    return verb, opts, pos


def capture(tool_name, args, result, session_id):
    """Dispatch records for a dispatching tool call: a list of (item, body, lost) where body is the `apa dispatch add`
    input and lost names an earlier dispatch to mark lost. Empty when the call is not one Hermes knows, or names no item
    this session delegated."""
    args = args if isinstance(args, dict) else {}
    if tool_name == "delegate_task":
        tasks = args.get("tasks") if isinstance(args.get("tasks"), list) else [args]
        out = []
        for t in tasks:
            for item in _items(t, session_id):
                out.append((item, {"resumable": False, "handle": {"mech": "delegate_task", "host": local_host()}}, None))
        return out
    if tool_name != "terminal":
        return []
    command = args.get("command") or ""
    try:
        argv = shlex.split(command)
    except ValueError:
        return []
    parsed = _paseo(argv) if argv else None
    items = _items(command, session_id) if parsed else []
    if not items:
        return []
    verb, opts, pos = parsed
    target = opts.get("host", "")
    if target and not _TARGET.match(target):
        return []
    host = _host_of(target) if target else local_host()
    r = _parsed(result)
    output = str(r.get("output") or "")
    ok = r.get("exit_code") == 0 and not r.get("error")
    handle = {"mech": "paseo", "host": host}
    if target:
        handle["target"] = target
    cwd = opts.get("cwd", "")
    out = []
    for item in items:
        if verb == "send":
            agent = pos[0] if pos and _AGENT.match(pos[0]) else None
            if not agent:
                continue
            prev = last_dispatch(item, agent)
            if not ok and _LOST.search(output + str(r.get("error") or "")):
                out.append((item, None, prev))  # the session is gone: mark the record lost, write nothing new
                continue
            body = {"session": agent, "resumable": True, "handle": {**handle, "agent_id": agent}}
            if prev:
                body["continues"] = prev
            out.append((item, body, None))
            continue
        agent = _agent_id(output) if ok else None
        body = {"resumable": bool(agent), "handle": dict(handle)}
        if agent:
            body["session"], body["handle"]["agent_id"] = agent, agent
        if cwd:
            body["cwd"] = cwd
        out.append((item, body, None))
    return out


# ---- resume hints for handles that come back ----

def resume_hint(entry):
    """How Hermes could pick this dispatch up again, or why it is reference only. entry is one history dispatch."""
    h, eh = entry.get("handle") or {}, (entry.get("executor") or {}).get("handle") or {}
    if entry.get("harness") != HARNESS:
        return {"use": "reference only", "why": f"written by {entry.get('harness') or 'another host'}"}
    try:
        check_handle(h) if h else None
        check_handle(eh) if eh else None
    except Invalid as e:
        return {"use": "reference only", "why": f"handle does not match Hermes' shapes: {e}"}
    mech = h.get("mech") or eh.get("mech")
    target = h.get("target") or eh.get("target")
    host = f" --host {shlex.quote(target)}" if target else ""
    item = entry.get("item", "<item>")
    if mech == "paseo":
        new = None
        if eh.get("profile"):
            new = f"paseo-profile-run{host} {shlex.quote(eh['profile'])} \"<task; name {item}>\""
        elif eh.get("provider"):
            pm = eh["provider"] + (f"/{eh['model']}" if eh.get("model") else "")
            new = f"paseo run{host} --provider {shlex.quote(pm)} \"<task; name {item}>\""
        if entry.get("resumable") and entry.get("state") != "lost" and h.get("agent_id"):
            return {"use": "terminal", "resume": f"paseo send{host} {h['agent_id']} \"<message; name {item}>\"", "or_new": new}
        return {"use": "terminal", "new": new, "why": "the session cannot be resumed"}
    if mech == "delegate_task":
        return {"use": "delegate_task", "new": f"delegate_task with the item's context (name {item} in the goal)", "why": "sub-agents are not resumable"}
    return {"use": "reference only", "why": "no handle"}
