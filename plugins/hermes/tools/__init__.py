"""APA tools plugin for Hermes: the apa CLI as native tools that carry the Hermes session id."""
import json

from . import cli, executors

TOOLSET = "apa"


def _s(desc):
    return {"type": "string", "description": desc}


def _schema(name, desc, props, required):
    return {"name": name, "description": desc,
            "parameters": {"type": "object", "properties": props, "required": required}}


_EVIDENCE = _s("The owner's own words that justify acting for them; required for chat answers.")
_REASON = _s("Short reason, recorded in the ledger event.")

# name -> (description, properties, required, argv builder)
_CLI_TOOLS = {
    "apa_decide": ("Answer a decision on the owner's behalf (apa decide). Exit 20 = web_only: give the owner the Web link and stop.",
                   {"id": _s("Item id"), "option": _s("Chosen option id"), "text": _s("Free-text answer"),
                    "evidence": _EVIDENCE, "reason": _REASON}, ["id"],
                   lambda a: ["decide", a["id"], *_flags(a, "option", "text", "evidence", "reason")]),
    "apa_accept": ("Accept a proposed item on the owner's behalf (apa accept).",
                   {"id": _s("Item id"), "evidence": _EVIDENCE, "reason": _REASON}, ["id"],
                   lambda a: ["accept", a["id"], *_flags(a, "evidence", "reason")]),
    "apa_dismiss": ("Dismiss a proposed item on the owner's behalf (apa dismiss).",
                    {"id": _s("Item id"), "evidence": _EVIDENCE, "reason": _REASON}, ["id"],
                    lambda a: ["dismiss", a["id"], *_flags(a, "evidence", "reason")]),
    "apa_executors": ("List the executors the project allows (apa executors).",
                      {"project": _s("Project id")}, [],
                      lambda a: ["executors", *_flags(a, "project")]),
    "apa_delegate": ("Create a child item for an executor and put the parent in waiting (apa delegate). Run it BEFORE handing work to anyone; exit 21 = executor not allowed.",
                     {"parent": _s("Parent item id"), "assignee": _s("Executor id"), "title": _s("Child title"),
                      "summary": _s("Summary"), "body": _s("Body"), "priority": _s("Priority"),
                      "project": _s("Project id"), "reason": _REASON,
                      "idempotency_key": _s("Idempotency key")}, ["parent", "assignee"],
                     lambda a: ["delegate", *_flags(a, "parent", "assignee", "title", "summary", "body", "priority",
                                                    "project", "reason", "idempotency_key")]),
    "apa_report": ("Report an executor's result on a child item (apa report).",
                   {"id": _s("Child item id"), "outcome": _s("progress | done | blocked"), "summary": _s("Summary"),
                    "pr": _s("PR url"), "commit": _s("Commit"), "branch": _s("Branch"), "link": _s("Other link"),
                    "ack_cursor": _s("Follow-up message cursor being acknowledged"), "reason": _REASON,
                    "idempotency_key": _s("Idempotency key")}, ["id", "outcome"],
                   lambda a: ["report", a["id"], *_flags(a, "outcome", "summary", "pr", "commit", "branch", "link",
                                                         "ack_cursor", "reason", "idempotency_key")]),
}


def _flags(args, *names):
    out = []
    for n in names:
        if args.get(n):
            out += ["--" + n.replace("_", "-"), str(args[n])]
    return out


def _cli_handler(build):
    def handle(args, session_id=None, **_):
        return json.dumps(cli.call(build(args), session_id))
    return handle


def _declare(args, session_id=None, **_):
    return json.dumps(cli.call(["declare", "-"], session_id, stdin=json.dumps(executors.declaration())))


def _invoke(args, **_):
    return json.dumps(executors.invocation(args.get("executor", ""), args.get("id", ""), args.get("task", "")))


def register(ctx):
    for name, (desc, props, req, build) in _CLI_TOOLS.items():
        ctx.register_tool(name=name, toolset=TOOLSET, schema=_schema(name, desc, props, req),
                          handler=_cli_handler(build), description=desc)
    desc = "Submit this harness's executors to the ledger (apa declare); they take effect after the owner confirms in Web."
    ctx.register_tool(name="apa_declare_executors", toolset=TOOLSET, schema=_schema("apa_declare_executors", desc, {}, []),
                      handler=_declare, description=desc)
    desc = "Map a delegated item's executor id to the Hermes tool call that runs it. Call apa_delegate first; run the returned tool call yourself."
    ctx.register_tool(name="apa_executor_call", toolset=TOOLSET, description=desc, handler=_invoke,
                      schema=_schema("apa_executor_call", desc, {"executor": _s("Executor id"), "id": _s("Child item id"),
                                                                 "task": _s("What the executor must do")}, ["executor", "id", "task"]))
