"""APA tools plugin for Hermes: the apa CLI as native tools that carry the Hermes session id."""
import json

from . import cli, executors, files

TOOLSET = "apa"


def _s(desc):
    return {"type": "string", "description": desc}


def _schema(name, desc, props, required):
    return {"name": name, "description": desc,
            "parameters": {"type": "object", "properties": props, "required": required}}


_EVIDENCE = _s("The owner's own words, quoted verbatim, that justify acting for them. They must come from this turn: never paraphrase, "
               "reword or invent them. Give this or grant, not both.")
_GRANT = _s("Id of a grant the owner issued in Web (apa grant list) for when they are absent. Give this or evidence, not both.")
_REASON = _s("Short reason, recorded in the ledger event.")

# name -> (description, properties, required, argv builder)
_CLI_TOOLS = {
    "apa_decide": ("Answer a decision on the owner's behalf (apa decide). Exit 20 = web_only: give the owner the Web link and stop.",
                   {"id": _s("Item id"), "option": _s("Chosen option id"), "text": _s("Free-text answer"),
                    "evidence": _EVIDENCE, "grant": _GRANT, "reason": _REASON}, ["id"],
                   lambda a: ["decide", a["id"], *_flags(a, "option", "text", "evidence", "reason", "grant")]),
    "apa_accept": ("Accept a proposed item on the owner's behalf (apa accept).",
                   {"id": _s("Item id"), "evidence": _EVIDENCE, "grant": _GRANT, "reason": _REASON}, ["id"],
                   lambda a: ["accept", a["id"], *_flags(a, "evidence", "reason", "grant")]),
    "apa_dismiss": ("Dismiss a proposed item on the owner's behalf (apa dismiss).",
                    {"id": _s("Item id"), "evidence": _EVIDENCE, "grant": _GRANT, "reason": _REASON}, ["id"],
                    lambda a: ["dismiss", a["id"], *_flags(a, "evidence", "reason", "grant")]),
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
    "apa_item_update": ("Write fields on your own item, e.g. the result in spec (apa item update). spec is a JSON object.",
                        {"id": _s("Item id"), "spec": {"type": "object", "description": "Structured result, e.g. period and sections"},
                         "title": _s("Title"), "summary": _s("Summary"), "body": _s("Body"),
                         "status_label": _s("Status label"), "priority": _s("Priority"), "tags": _s("Comma-separated tags"),
                         "reason": _s("Short reason, recorded in the ledger event"), "evidence": _EVIDENCE, "grant": _GRANT}, ["id", "reason"],
                        lambda a: ["item", "update", a["id"], *_flags(a, "title", "summary", "body", "status_label", "priority",
                                                                      "tags", "spec", "reason", "evidence", "grant")]),
}


def _flags(args, *names):
    out = []
    for n in names:
        v, flag = args.get(n), "--" + n.replace("_", "-")
        if v is True:
            out.append(flag)
        elif isinstance(v, list):
            out += [x for item in v for x in (flag, str(item))]
        elif isinstance(v, dict):
            out += [flag, json.dumps(v, ensure_ascii=False)]
        elif v and v is not False:
            out += [flag, str(v)]
    return out


def _cli_handler(build):
    def handle(args, session_id=None, **_):
        return json.dumps(cli.call(build(args), session_id))
    return handle


def _material_commit(args, session_id=None, **_):
    content = args.get("content") or ""
    for need, ok in (("content", content), ("reason", args.get("reason"))):
        if not ok:
            return json.dumps({"ok": False, "error": f"{need} is required"})
    if not args.get("id") and not (args.get("kind") and args.get("title")):
        return json.dumps({"ok": False, "error": "a new material needs kind and title"})
    argv = ["material", "commit", *([args["id"]] if args.get("id") else []), "--from", "-",
            *_flags(args, "reason", "kind", "title", "project", "based_on", "tag")]
    return json.dumps(cli.call(argv, session_id, stdin=content))


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
    desc = "Read a text file under the run directory (e.g. the {materials_path} file) or the working directory. Read-only."
    ctx.register_tool(name="apa_read_file", toolset=TOOLSET, schema=_schema("apa_read_file", desc, {"path": _s("File path")}, ["path"]),
                      handler=lambda args, **_: json.dumps(files.read_file(args.get("path"))), description=desc)
    desc = "List a directory under the run directory or the working directory. Read-only."
    ctx.register_tool(name="apa_list_dir", toolset=TOOLSET, schema=_schema("apa_list_dir", desc, {"path": _s("Directory, default '.'")}, []),
                      handler=lambda args, **_: json.dumps(files.list_dir(args.get("path"))), description=desc)
    desc = ("Commit a draft as a pending revision the owner must confirm (apa material commit --from -). The text goes in content, "
            "never in a shell. New material: kind and title (e.g. kind=guideline); revision: id.")
    ctx.register_tool(name="apa_material_commit", toolset=TOOLSET, handler=_material_commit, description=desc,
                      schema=_schema("apa_material_commit", desc, {
                          "content": _s("Full text of the draft"), "reason": _REASON, "id": _s("Existing material id (revision)"),
                          "kind": _s("Material kind, e.g. guideline"), "title": _s("Title of a new material"),
                          "project": _s("Project id"), "based_on": _s("Revision this is based on"),
                          "tag": {"type": "array", "items": {"type": "string"}, "description": "Tags"}}, ["content", "reason"]))
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
