"""Executor id -> how Hermes invokes it. The ledger only ever sees {id, kind, scenarios}, never the call."""
import shlex

# shortcut: static table, add an entry here to support another executor; upgrade to a config file if users must edit it.
EXECUTORS = {
    "hermes.subagent": {"kind": "native-subagent", "scenarios": ["research", "coding"]},
    "hermes.codex": {"kind": "external-cli", "scenarios": ["coding"], "command": ["codex", "exec", "{prompt}"]},
}


def declaration():
    """Body for `apa declare`: no commands, the owner confirms it in Web."""
    return {"harness": "hermes",
            "executors": [{"id": i, "kind": e["kind"], "scenarios": e["scenarios"]} for i, e in EXECUTORS.items()]}


def invocation(executor, item_id, task):
    """The Hermes tool call that runs `executor` on a delegated item, or an error for unknown ids."""
    e = EXECUTORS.get(executor)
    if e is None:
        return {"error": f"unknown executor {executor!r}; known here: {sorted(EXECUTORS)}"}
    prompt = f"{task}\n\nLedger item: {item_id}. When done run: apa report {item_id} --outcome done --summary <text> [--pr <url>]"
    if e["kind"] == "native-subagent":
        return {"use_tool": "delegate_task", "arguments": {"tasks": [{"goal": prompt, "context": f"APA item {item_id}"}]}}
    cmd = " ".join(shlex.quote(a.replace("{prompt}", prompt)) for a in e["command"])
    return {"use_tool": "terminal", "arguments": {"command": cmd}}
