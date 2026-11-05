"""Executor id -> how Hermes invokes it. The ledger only ever sees {id, kind, scenarios, host?}, never the call.

The table is the owner's: a JSON list in $APA_HERMES_EXECUTORS, default $HERMES_HOME/apa-executors.json
(no file = nothing configured, nothing declared). Entry: {id, kind, scenarios[], host?, command?[]};
`command` (external-cli only) is an argv template where "{prompt}" is replaced.
"""
import json
import os
import pathlib
import shlex

KINDS = ("native-subagent", "external-cli", "human")


def _path():
    p = os.environ.get("APA_HERMES_EXECUTORS")
    return pathlib.Path(p) if p else pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes")) / "apa-executors.json"


def load():
    """{id: entry}; raises ValueError on a malformed table so the tool reports it instead of guessing."""
    p = _path()
    if not p.exists():
        return {}
    try:
        items = json.loads(p.read_text())
    except (OSError, ValueError) as e:
        raise ValueError(f"cannot read executor table {p}: {e}")
    table = {}
    for e in items if isinstance(items, list) else [None]:
        if not isinstance(e, dict) or not e.get("id") or e.get("kind") not in KINDS:
            raise ValueError(f"{p}: each entry needs id and kind in {KINDS}")
        if e["kind"] == "external-cli" and not (isinstance(e.get("command"), list) and e["command"]):
            raise ValueError(f"{p}: external-cli executor {e['id']!r} needs a command list")
        table[e["id"]] = e
    return table


def declaration():
    """Body for `apa declare`: no commands, the owner confirms it in Web."""
    table = load()
    if not table:
        raise ValueError(f"no executors configured; write them to {_path()}")
    keep = ("id", "kind", "scenarios", "host")
    return {"harness": "hermes", "executors": [{k: e[k] for k in keep if k in e} | {"scenarios": e.get("scenarios", [])} for e in table.values()]}


def invocation(executor, item_id, task):
    """The Hermes tool call that runs `executor` on a delegated item, or an error."""
    e = load().get(executor)
    if e is None:
        return {"error": f"unknown executor {executor!r}; configured: {sorted(load())}"}
    if e["kind"] == "human":
        return {"error": f"executor {executor!r} is a human: hand the item to the owner, nothing to run"}
    prompt = f"{task}\n\nLedger item: {item_id}. When done run: apa report {item_id} --outcome done --summary <text> [--pr <url>]"
    if e["kind"] == "native-subagent":
        return {"use_tool": "delegate_task", "arguments": {"tasks": [{"goal": prompt, "context": f"APA item {item_id}"}]}}
    cmd = " ".join(shlex.quote(a.replace("{prompt}", prompt)) for a in e["command"])
    return {"use_tool": "terminal", "arguments": {"command": cmd}}
