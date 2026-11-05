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


def test_registers_the_eight_tools(tools):
    assert set(tools) == {"apa_decide", "apa_accept", "apa_dismiss", "apa_executors", "apa_delegate",
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


def test_declare_sends_ids_kinds_scenarios_but_no_call_commands(tools, apa):
    tools["apa_declare_executors"]({}, session_id="s")
    assert calls(apa) == [{"argv": ["declare", "-"], "session": "s"}]
    body = json.loads((apa / "stdin").read_text())
    assert body["harness"] == "hermes"
    assert {e["id"] for e in body["executors"]} == {"hermes.subagent", "hermes.codex"}
    assert all(set(e) == {"id", "kind", "scenarios"} for e in body["executors"])
    assert "codex exec" not in json.dumps(body) and "delegate_task" not in json.dumps(body)


def test_executor_call_maps_ids_to_hermes_invocations(tools):
    sub = json.loads(tools["apa_executor_call"]({"executor": "hermes.subagent", "id": "c1", "task": "fix it"}))
    assert sub["use_tool"] == "delegate_task" and "fix it" in sub["arguments"]["tasks"][0]["goal"]
    assert "apa report c1" in sub["arguments"]["tasks"][0]["goal"]
    cli = json.loads(tools["apa_executor_call"]({"executor": "hermes.codex", "id": "c1", "task": "it's ok"}))
    assert cli["use_tool"] == "terminal" and cli["arguments"]["command"].startswith("codex exec ")
    bad = json.loads(tools["apa_executor_call"]({"executor": "x.y", "id": "c1", "task": "t"}))
    assert "unknown executor" in bad["error"]
