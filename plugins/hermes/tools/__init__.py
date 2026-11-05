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
    "apa_propose": ("Suggest a new item for the owner (apa propose); it lands as proposed.",
                    {"kind": _s("Item kind"), "title": _s("Title"), "summary": _s("Summary"), "body": _s("Body"),
                     "priority": _s("Priority"), "project": _s("Project id"), "tags": _s("Comma-separated tags"),
                     "assignee": _s("Assignee"), "reason": _REASON, "idempotency_key": _s("Idempotency key")},
                    ["kind", "title"],
                    lambda a: ["propose", *_flags(a, "kind", "title", "summary", "body", "priority", "project", "tags",
                                                  "assignee", "reason", "idempotency_key")]),
    "apa_ask": ("Ask the owner for a decision on an item (apa ask).",
                {"id": _s("Item id"), "question": _s("The question"),
                 "option": {"type": "array", "items": {"type": "string"}, "description": "Options, each 'id=label'"},
                 "next": {"type": "array", "items": {"type": "string"}, "description": "Phase after an option, each 'id=phase'"},
                 "recommend": _s("Recommended option id"), "free_text": {"type": "boolean", "description": "Allow a free-text answer"},
                 "deadline": _s("RFC3339 deadline"), "default_on_timeout": _s("Option id applied at the deadline"),
                 "resume_note": _s("Note for resuming after the answer"), "reason": _REASON}, ["id", "question"],
                lambda a: ["ask", a["id"], *_flags(a, "question", "option", "next", "recommend", "free_text", "deadline",
                                                   "default_on_timeout", "resume_note", "reason")]),
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
        v, flag = args.get(n), "--" + n.replace("_", "-")
        if v is True:
            out.append(flag)
        elif isinstance(v, list):
            out += [x for item in v for x in (flag, str(item))]
        elif v and v is not False:
            out += [flag, str(v)]
    return out


def _cli_handler(build):
    def handle(args, session_id=None, **_):
        return json.dumps(cli.call(build(args), session_id))
    return handle


def _declare(args, session_id=None, **_):
    try:
        body = executors.declaration()
    except ValueError as e:
        return json.dumps({"ok": False, "error": str(e)})
    return json.dumps(cli.call(["declare", "-"], session_id, stdin=json.dumps(body)))


def _invoke(args, **_):
    try:
        return json.dumps(executors.invocation(args.get("executor", ""), args.get("id", ""), args.get("task", "")))
    except ValueError as e:
        return json.dumps({"ok": False, "error": str(e)})


def register(ctx):
    for name, (desc, props, req, build) in _CLI_TOOLS.items():
        ctx.register_tool(name=name, toolset=TOOLSET, schema=_schema(name, desc, props, req),
                          handler=_cli_handler(build), description=desc)
    desc = "Submit the executors configured for this harness to the ledger (apa declare); they take effect after the owner confirms in Web."
    ctx.register_tool(name="apa_declare_executors", toolset=TOOLSET, schema=_schema("apa_declare_executors", desc, {}, []),
                      handler=_declare, description=desc)
    desc = "Map a delegated item's executor id to the Hermes tool call that runs it. Call apa_delegate first; run the returned tool call yourself."
    ctx.register_tool(name="apa_executor_call", toolset=TOOLSET, description=desc, handler=_invoke,
                      schema=_schema("apa_executor_call", desc, {"executor": _s("Executor id"), "id": _s("Child item id"),
                                                                 "task": _s("What the executor must do")}, ["executor", "id", "task"]))
