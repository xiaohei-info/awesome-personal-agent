"""Hermes' own ways of delegating, as data for the ledger (design §6.17). There is no "executor id -> invocation" table:
Hermes decides how to delegate. This module only

- derives a stable executor id, label and handle from the way Hermes delegated (the model names the mech and its
  parameters, never the id);
- recognises Hermes' dispatching tool calls after the fact (capture) and turns them into dispatch records, and
  recognises the calls that bring a dispatched session's result back (completions);
- checks handles against Hermes' own shapes before writing, and turns handles Hermes itself wrote back into resume
  hints for Hermes' own tools. Anything else is shown as reference only.

No per-process memory: which item a dispatch belongs to is the kernel's to know. A dispatch naming a delegated item is
recorded on it; one naming no such item is recorded unattached and shown every turn until the model attaches it; a
result is reported by the dispatch's session handle and the kernel finds the item.
"""
import hashlib
import json
import os
import re
import shlex
import socket

HARNESS = "hermes"
MECHS = ("delegate_task", "paseo")
PASEO_PROGRAMS = ("paseo", "paseo-profile-run")

ITEM_ID = re.compile(r"\bitm_[0-9a-f]+\b")
_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$")      # profile, provider, model
_AGENT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")       # paseo agent id
_TARGET = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$")  # paseo --host value (daemon endpoint or ssh host)
_LOST = re.compile(r"not found|no such agent|unknown agent|does not exist", re.I)


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

def _item(text):
    """The first item id the call names, or None."""
    m = ITEM_ID.search(text if isinstance(text, str) else json.dumps(text, default=str))
    return m.group(0) if m else None


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
        v = json.loads(out[out.index("{"):] if "{" in out else out)
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
    """Parse one paseo command: (verb, opts, positionals) or None."""
    prog = os.path.basename(argv[0])
    rest = argv[1:]
    if prog == "paseo":
        opts = {}
        while rest[:1] and rest[0].startswith("--") and rest[0] != "--":  # global options before the verb (--host)
            key, eq, val = rest[0][2:].partition("=")
            if not eq and len(rest) > 1 and key not in ("json",):
                val, rest = rest[1], rest[1:]
            opts[key], rest = val, rest[1:]
        if not rest or rest[0] not in ("run", "send", "wait", "logs"):
            return None
        verb, rest = rest[0], rest[1:]
    elif prog == "paseo-profile-run":
        verb, opts = "profile-run", {}
    else:
        return None
    pos, i = [], 0
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


def _segments(command):
    """The simple commands of a shell line (split on && || ; |), each as argv; unparsable lines give nothing."""
    try:
        lex = shlex.shlex(command, posix=True, punctuation_chars=";&|")
        lex.whitespace_split = True
        tokens = list(lex)
    except ValueError:
        return []
    out, cur = [], []
    for t in tokens:
        if t and set(t) <= set(";&|"):
            if cur:
                out.append(cur)
            cur = []
        else:
            cur.append(t)
    if cur:
        out.append(cur)
    return out


def _record(item, assignee, enrollment, body):
    """One `apa dispatch add` input: on the item when it names one, else unattached under the executor."""
    return {"item": item, "assignee": assignee, "executor": enrollment, **body}


def capture(tool_name, args, result, session_id=None):
    """Dispatch records for a dispatching tool call: a list of dicts, each either an `apa dispatch add` input
    {item?, assignee, executor, session?, resumable, handle, cwd?} or {"lost": session} for a resumed session that is gone.
    Empty when the call is not one Hermes knows."""
    args = args if isinstance(args, dict) else {}
    if tool_name == "delegate_task":
        tasks = args.get("tasks") if isinstance(args.get("tasks"), list) else [args]
        eid, enroll = executor("delegate_task", {})
        return [_record(_item(t), eid, enroll, {"resumable": False, "handle": {"mech": "delegate_task", "host": local_host()}}) for t in tasks]
    if tool_name != "terminal":
        return []
    command = args.get("command") or ""
    r = _parsed(result)
    output = str(r.get("output") or "")
    ok = r.get("exit_code") == 0 and not r.get("error")
    out = []
    for argv in _segments(command):
        parsed = _paseo(argv)
        if not parsed or parsed[0] not in ("run", "send", "profile-run"):
            continue
        verb, opts, pos = parsed
        target = opts.get("host", "")
        if target and not _TARGET.match(target):
            continue
        host = _host_of(target) if target else local_host()
        handle = {"mech": "paseo", "host": host}
        if target:
            handle["target"] = target
        item = _item(command)
        if verb == "send":
            agent = pos[0] if pos and _AGENT.match(pos[0]) else None
            if not agent:
                continue
            if not ok and _LOST.search(output + str(r.get("error") or "")):
                out.append({"lost": agent})  # the session is gone: mark its record lost, write nothing new
                continue
            # a resumed session: the kernel puts it on the item its earlier dispatch is attached to
            out.append({"session": agent, "resumable": True, "handle": {**handle, "agent_id": agent}})
            continue
        params = {k: opts.get(k) for k in ("profile", "provider", "model")}
        if verb == "profile-run" and pos:
            params["profile"] = pos[0]
        params["host"] = target
        try:
            eid, enroll = executor("paseo", params)
        except Invalid:
            continue
        agent = _agent_id(output) if ok else None
        body = {"resumable": bool(agent), "handle": dict(handle)}
        if agent:
            body["session"], body["handle"]["agent_id"] = agent, agent
        if opts.get("cwd"):
            body["cwd"] = opts["cwd"]
        out.append(_record(item, eid, enroll, body))
    return out


def completions(tool_name, args, result):
    """Results that came back for dispatched sessions: (session handle, output, ok) for each `paseo wait <id>` or
    `paseo logs <id>` the terminal call ran (the waiter line paseo prints is exactly that). The kernel finds the item."""
    if tool_name != "terminal":
        return []
    r = _parsed(result)
    output = str(r.get("output") or "")
    ok = r.get("exit_code") == 0 and not r.get("error")
    seen = []
    for argv in _segments((args if isinstance(args, dict) else {}).get("command") or ""):
        parsed = _paseo(argv)
        if parsed and parsed[0] in ("wait", "logs") and parsed[2] and _AGENT.match(parsed[2][0]) and parsed[2][0] not in seen:
            seen.append(parsed[2][0])
    return [(agent, output, ok) for agent in seen]


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
