"""Unit tests: the plugin against a stand-in `apa` executable (test boundary only)."""
import json
import stat

import pytest

from helpers import load_tools


class Ctx:
    def __init__(self):
        self.tools = {}

    def register_tool(self, name, toolset, schema, handler, **kw):
        assert name not in self.tools and toolset == "apa" and schema["name"] == name
        self.tools[name] = (schema, handler)


# Records argv / APA_SESSION / stdin as one JSON line, then answers with the scripted stdout/stderr/exit code.
FAKE = """#!/bin/sh
python3 - "$@" <<'PY' >> "$APA_LOG"
import json, os, sys
print(json.dumps({"argv": sys.argv[1:], "session": os.environ.get("APA_SESSION")}))
PY
cat > "$APA_STDIN" 2>/dev/null || true
echo "$FAKE_OUT"
echo "$FAKE_ERR" >&2
exit ${FAKE_EXIT:-0}
"""


@pytest.fixture
def apa(tmp_path, monkeypatch):
    p = tmp_path / "apa"
    p.write_text(FAKE)
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    for k, v in {"APA_BIN": str(p), "APA_LOG": str(tmp_path / "log"), "APA_STDIN": str(tmp_path / "stdin")}.items():
        monkeypatch.setenv(k, v)
    return tmp_path


def calls(tmp):
    return [json.loads(l) for l in (tmp / "log").read_text().splitlines()]


@pytest.fixture
def tools():
    ctx = Ctx()
    load_tools().register(ctx)
    return {n: h for n, (_, h) in ctx.tools.items()}


TABLE = [{"id": "work-host.cli", "kind": "external-cli", "scenarios": ["coding"], "host": "work-host",
          "command": ["codex", "exec", "{prompt}"]},
         {"id": "hermes.sub", "kind": "native-subagent", "scenarios": ["research"]},
         {"id": "owner", "kind": "human"}]


@pytest.fixture
def table(tmp_path, monkeypatch):
    p = tmp_path / "executors.json"
    p.write_text(json.dumps(TABLE))
    monkeypatch.setenv("APA_HERMES_EXECUTORS", str(p))
    return p


def test_registers_the_apa_tools(tools):
    assert set(tools) == {"apa_read_file", "apa_list_dir", "apa_item_update", "apa_material_commit", "apa_propose", "apa_ask", "apa_decide", "apa_accept", "apa_dismiss", "apa_executors", "apa_delegate",
                          "apa_report", "apa_declare_executors", "apa_executor_call"}


def test_decide_carries_hermes_session_and_evidence(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_OUT", '{"state":"done"}')
    r = json.loads(tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "就选方案 B"}, session_id="sess-9"))
    assert calls(apa) == [{"argv": ["decide", "i1", "--option", "B", "--evidence", "就选方案 B"], "session": "sess-9"}]
    assert r == {"exit_code": 0, "ok": True, "stdout": {"state": "done"}, "stderr": None}


def test_web_only_refusal_is_returned_untouched_and_not_retried(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "20")
    monkeypatch.setenv("FAKE_ERR", '{"code":"web_only","why":"web_only","next":"https://x/web/i1"}')
    r = json.loads(tools["apa_decide"]({"id": "i1", "option": "B"}, session_id="s"))
    assert r["exit_code"] == 20 and not r["ok"] and r["stderr"]["next"] == "https://x/web/i1"
    assert len(calls(apa)) == 1


@pytest.mark.parametrize("tool,args,argv", [
    ("apa_propose", {"kind": "task", "title": "T", "tags": "a,b"}, ["propose", "--kind", "task", "--title", "T", "--tags", "a,b"]),
    ("apa_ask", {"id": "i", "question": "Q?", "option": ["A=yes", "B=no"], "next": ["A=open"], "free_text": True,
                 "default_on_timeout": "A"},
     ["ask", "i", "--question", "Q?", "--option", "A=yes", "--option", "B=no", "--next", "A=open", "--free-text",
      "--default-on-timeout", "A"]),
    ("apa_accept", {"id": "i", "evidence": "ok"}, ["accept", "i", "--evidence", "ok"]),
    ("apa_dismiss", {"id": "i", "reason": "dup"}, ["dismiss", "i", "--reason", "dup"]),
    ("apa_executors", {"project": "p1"}, ["executors", "--project", "p1"]),
    ("apa_delegate", {"parent": "a", "assignee": "hermes.subagent", "idempotency_key": "k"},
     ["delegate", "--parent", "a", "--assignee", "hermes.subagent", "--idempotency-key", "k"]),
    ("apa_report", {"id": "c", "outcome": "done", "summary": "s", "pr": "u", "ack_cursor": "7"},
     ["report", "c", "--outcome", "done", "--summary", "s", "--pr", "u", "--ack-cursor", "7"]),
])
def test_cli_tools_map_args_and_session(tools, apa, tool, args, argv):
    tools[tool](args, session_id="s2")
    assert calls(apa) == [{"argv": argv, "session": "s2"}]


def test_delegate_not_allowed_exit_21_is_exposed(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "21")
    r = json.loads(tools["apa_delegate"]({"parent": "a", "assignee": "nope"}, session_id="s"))
    assert r["exit_code"] == 21 and r["ok"] is False


def test_missing_apa_is_a_real_error_not_fake_success(tools, monkeypatch):
    monkeypatch.setenv("APA_BIN", "/nonexistent/apa")
    r = json.loads(tools["apa_decide"]({"id": "i"}, session_id="s"))
    assert r["ok"] is False and "cannot run apa" in r["error"]


def test_declare_sends_configured_executors_without_call_commands(tools, apa, table):
    tools["apa_declare_executors"]({}, session_id="s")
    assert calls(apa) == [{"argv": ["declare", "-"], "session": "s"}]
    body = json.loads((apa / "stdin").read_text())
    assert body["harness"] == "hermes"
    assert body["executors"] == [
        {"id": "work-host.cli", "kind": "external-cli", "scenarios": ["coding"], "host": "work-host"},
        {"id": "hermes.sub", "kind": "native-subagent", "scenarios": ["research"]},
        {"id": "owner", "kind": "human", "scenarios": []}]
    assert "codex" not in json.dumps(body)


def test_declare_without_configured_executors_is_an_error_and_sends_nothing(tools, apa, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_HERMES_EXECUTORS", str(tmp_path / "missing.json"))
    r = json.loads(tools["apa_declare_executors"]({}, session_id="s"))
    assert r["ok"] is False and "no executors configured" in r["error"]
    assert not (apa / "log").exists()


def test_malformed_table_is_reported(tools, tmp_path, monkeypatch):
    p = tmp_path / "bad.json"
    p.write_text('[{"id":"x","kind":"external-cli"}]')
    monkeypatch.setenv("APA_HERMES_EXECUTORS", str(p))
    r = json.loads(tools["apa_executor_call"]({"executor": "x", "id": "c", "task": "t"}))
    assert r["ok"] is False and "needs a command" in r["error"]


def test_executor_call_maps_configured_ids_to_hermes_invocations(tools, table):
    sub = json.loads(tools["apa_executor_call"]({"executor": "hermes.sub", "id": "c1", "task": "fix it"}))
    assert sub["use_tool"] == "delegate_task" and "apa report c1" in sub["arguments"]["tasks"][0]["goal"]
    cli = json.loads(tools["apa_executor_call"]({"executor": "work-host.cli", "id": "c1", "task": "it's ok"}))
    assert cli["use_tool"] == "terminal" and cli["arguments"]["command"].startswith("codex exec ")
    assert "unknown executor" in json.loads(tools["apa_executor_call"]({"executor": "x.y", "id": "c", "task": "t"}))["error"]
    assert "human" in json.loads(tools["apa_executor_call"]({"executor": "owner", "id": "c", "task": "t"}))["error"]


def test_item_update_writes_spec_json_with_session(tools, apa):
    spec = {"period": "2026-W45", "sections": [{"project": "p", "text": "ok"}]}
    tools["apa_item_update"]({"id": "i1", "spec": spec, "reason": "brief done"}, session_id="s3")
    [c] = calls(apa)
    assert c["session"] == "s3" and c["argv"][:2] == ["item", "update"] and c["argv"][2] == "i1"
    assert json.loads(c["argv"][c["argv"].index("--spec") + 1]) == spec
    assert c["argv"][-2:] == ["--reason", "brief done"]


def test_item_update_kernel_refusal_is_exposed(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "4")
    monkeypatch.setenv("FAKE_ERR", '{"code":"forbidden"}')
    r = json.loads(tools["apa_item_update"]({"id": "other", "spec": {}, "reason": "x"}, session_id="s"))
    assert r["exit_code"] == 4 and r["ok"] is False and r["stderr"] == {"code": "forbidden"}


def test_material_commit_passes_body_on_stdin_not_argv(tools, apa):
    body = "# 规范\n- 用 make test; $(rm -rf /) 只是文字\n"
    tools["apa_material_commit"]({"kind": "guideline", "title": "G", "project": "p1", "reason": "draft", "content": body},
                                 session_id="s4")
    assert calls(apa) == [{"argv": ["material", "commit", "--from", "-", "--reason", "draft", "--kind", "guideline",
                                    "--title", "G", "--project", "p1"], "session": "s4"}]
    assert (apa / "stdin").read_text() == body


def test_material_commit_revision_of_existing_id(tools, apa):
    tools["apa_material_commit"]({"id": "m1", "reason": "r", "content": "x"}, session_id="s")
    assert calls(apa)[0]["argv"] == ["material", "commit", "m1", "--from", "-", "--reason", "r"]


@pytest.mark.parametrize("args,why", [({"reason": "r"}, "content"), ({"content": "x"}, "reason"),
                                      ({"reason": "r", "content": "x"}, "kind and title")])
def test_material_commit_rejects_incomplete_calls_without_running_apa(tools, apa, args, why):
    r = json.loads(tools["apa_material_commit"](args, session_id="s"))
    assert r["ok"] is False and why in r["error"] and not (apa / "log").exists()
