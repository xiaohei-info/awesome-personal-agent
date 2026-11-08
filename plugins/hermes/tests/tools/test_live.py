"""Live gate: real Hermes plugin loader + tool dispatch, real `apa` binary built from this repo.

Only the kernel HTTP endpoint is a stand-in: it records each request (path, X-APA-Session, body) and
answers with the kernel's documented error bodies. Skips when Hermes or Go is missing (e.g. CI).
"""
import json
import os
import pathlib
import shutil
import subprocess
import textwrap
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from helpers import ROOT, TOOLS

HERMES_SRC = pathlib.Path(os.environ.get("HERMES_SRC", pathlib.Path.home() / ".hermes" / "hermes-agent"))
HERMES_PY = HERMES_SRC / "venv" / "bin" / "python"
REQUIRE_LIVE = os.environ.get("APA_REQUIRE_LIVE") == "1"  # reviewer/gate hosts: never skip silently
PROBE = "from model_tools import handle_function_call; from hermes_cli import plugins as p; assert hasattr(p.PluginContext, 'register_tool')"


def _live_problem():
    if not HERMES_PY.exists() or not shutil.which("go"):
        return f"needs a Hermes install (HERMES_SRC={HERMES_SRC}) and Go"
    r = subprocess.run([str(HERMES_PY), "-c", PROBE], cwd=HERMES_SRC, capture_output=True, timeout=120)
    return None if r.returncode == 0 else "this Hermes lacks PluginContext.register_tool / model_tools.handle_function_call"


_PROBLEM = _live_problem()
if _PROBLEM and REQUIRE_LIVE:
    pytest.fail("live gate required (APA_REQUIRE_LIVE=1) but cannot run: " + _PROBLEM, pytrace=False)
pytestmark = pytest.mark.skipif(bool(_PROBLEM), reason=_PROBLEM or "")

# One Hermes process: load the plugin the normal way, then call tools through Hermes' own dispatcher
# with a session id, exactly as an agent turn would. stdin: JSON [[tool, args, session_id], ...].
CHILD = textwrap.dedent("""
    import json, sys
    from hermes_cli import plugins
    plugins.discover_plugins()
    from model_tools import handle_function_call
    from tools.registry import registry
    out = {"registered": sorted(n for n in registry.get_all_tool_names() if n.startswith("apa_")), "results": []}
    for tool, args, sid in json.load(sys.stdin):
        out["results"].append(json.loads(handle_function_call(tool, args, task_id="t", session_id=sid)))
    print("RESULT" + json.dumps(out))
""")


@pytest.fixture(scope="module")
def apa_bin(tmp_path_factory):
    out = tmp_path_factory.mktemp("bin") / "apa"
    subprocess.run(["go", "build", "-o", str(out), "./cli/apa"], cwd=ROOT, check=True)
    return str(out)


@pytest.fixture
def kernel():
    state = {"log": [], "replies": {}}  # replies: path suffix -> (status, body)

    class H(BaseHTTPRequestHandler):
        def _serve(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n) or b"null")
            state["log"].append({"method": self.command, "path": self.path,
                                 "session": self.headers.get("X-APA-Session"),
                                 "evidence": (self.headers.get("X-APA-Evidence") or "").encode("latin-1").decode("utf-8"), "body": body})
            status, out = next((r for k, r in state["replies"].items() if self.path.split("?")[0].endswith(k)),
                               (200, {"ok": True}))
            data = json.dumps(out).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        do_GET = do_POST = do_PATCH = _serve

        def log_message(self, *a):
            pass

    srv = HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    state["url"] = f"http://127.0.0.1:{srv.server_port}"
    yield state
    srv.shutdown()


@pytest.fixture
def hermes(tmp_path, apa_bin, kernel):
    home = tmp_path / "hermes_home"
    (home / "plugins").mkdir(parents=True)
    (home / "plugins" / "apa-tools").symlink_to(TOOLS)
    (home / "config.yaml").write_text("plugins:\n  enabled:\n    - apa-tools\n")
    table = tmp_path / "apa-executors.json"
    table.write_text(json.dumps([
        {"id": "work-host.cli", "kind": "external-cli", "scenarios": ["coding"], "host": "work-host", "command": ["codex", "exec", "{prompt}"]},
        {"id": "hermes.subagent", "kind": "native-subagent", "scenarios": ["research"]}]))
    env = {**os.environ, "APA_HERMES_EXECUTORS": str(table), "HERMES_HOME": str(home), "APA_BIN": apa_bin, "APA_URL": kernel["url"],
           "APA_TOKEN": "t", "APA_HOME": str(tmp_path / "apahome")}

    def run(*calls):
        p = subprocess.run([str(HERMES_PY), "-c", CHILD], cwd=HERMES_SRC, env=env, input=json.dumps(calls),
                           capture_output=True, text=True, timeout=120, check=True)
        line = next(l for l in p.stdout.splitlines() if l.startswith("RESULT"))
        return json.loads(line[len("RESULT"):])

    return run


def test_flow_hermes_context_session_identity_reaches_propose_and_all_tools(hermes, kernel):
    out = hermes(("apa_propose", {"kind": "task", "title": "Renew domain", "tags": "x,y"}, "sess-A"),
                 ("apa_executors", {}, "sess-A"), ("apa_report", {"id": "c1", "outcome": "progress"}, "sess-B"))
    assert out["registered"] == sorted(["apa_propose", "apa_ask", "apa_accept", "apa_declare_executors", "apa_decide",
                                        "apa_delegate", "apa_dismiss", "apa_executor_call", "apa_executors", "apa_report", "apa_read_file",
                                        "apa_list_dir", "apa_item_update", "apa_material_commit"])
    assert [(r["path"], r["session"]) for r in kernel["log"]] == [
        ("/v1/items", "sess-A"), ("/v1/executors", "sess-A"), ("/v1/items/c1/report", "sess-B")]
    assert kernel["log"][0]["body"]["title"] == "Renew domain" and kernel["log"][0]["body"]["kind"] == "task"


def test_flow_chat_answer_decide_is_recorded_with_the_owners_words(hermes, kernel):
    out = hermes(("apa_decide", {"id": "i1", "option": "B", "evidence": "就选方案 B"}, "s1"))
    assert out["results"][0]["exit_code"] == 0
    (req,) = kernel["log"]
    assert req["path"] == "/v1/items/i1/decide" and req["session"] == "s1"
    assert req["body"]["choice"] == "B" and req["evidence"] == "就选方案 B"  # evidence travels as X-APA-Evidence


def test_flow_chat_answer_web_only_refusal_stops_and_nothing_else_is_sent(hermes, kernel):
    kernel["replies"]["/i1/decide"] = (403, {"code": "web_only", "why": "this decision is web_only", "next": "open the Web link"})
    out = hermes(("apa_decide", {"id": "i1", "option": "B", "evidence": "B"}, "s1"))
    r = out["results"][0]
    assert r["exit_code"] == 20 and r["ok"] is False and r["stderr"]["code"] == "web_only"
    assert len(kernel["log"]) == 1  # the plugin neither retried nor followed up


def test_flow_delegate_recover_delegate_then_report_and_disallowed_executor(hermes, kernel):
    kernel["replies"]["/delegations"] = (200, {"id": "child1"})
    out = hermes(("apa_delegate", {"parent": "p1", "assignee": "hermes.subagent", "title": "t"}, "s1"),
                 ("apa_executor_call", {"executor": "hermes.subagent", "id": "child1", "task": "do it"}, "s1"),
                 ("apa_report", {"id": "child1", "outcome": "done", "summary": "ok", "pr": "https://x/pr/1"}, "s1"))
    assert out["results"][0]["stdout"] == {"id": "child1"}
    assert out["results"][1]["use_tool"] == "delegate_task"
    assert kernel["log"][-1]["body"]["evidence"] == {"pr": "https://x/pr/1"} and kernel["log"][-1]["body"]["outcome"] == "done"
    kernel["replies"]["/delegations"] = (403, {"code": "executor_not_allowed", "why": "not allowed", "next": "pick another"})
    out = hermes(("apa_delegate", {"parent": "p1", "assignee": "work-host.cli"}, "s1"))
    assert out["results"][0]["exit_code"] == 21


def test_flow_first_day_declare_submits_configured_executors_and_they_resolve(hermes, kernel):
    out = hermes(("apa_declare_executors", {}, "s1"),
                 ("apa_executor_call", {"executor": "work-host.cli", "id": "child1", "task": "build"}, "s1"))
    assert out["results"][0]["exit_code"] == 0
    (req,) = kernel["log"]
    assert req["path"] == "/v1/declarations" and req["session"] == "s1" and req["body"]["harness"] == "hermes"
    assert req["body"]["executors"] == [
        {"id": "work-host.cli", "kind": "external-cli", "scenarios": ["coding"], "host": "work-host"},
        {"id": "hermes.subagent", "kind": "native-subagent", "scenarios": ["research"]}]
    assert "codex" not in json.dumps(req["body"])
    call = out["results"][1]
    assert call["use_tool"] == "terminal" and call["arguments"]["command"].startswith("codex exec ")


def test_flow_life_renewal_ask_carries_session_question_and_options(hermes, kernel):
    out = hermes(("apa_ask", {"id": "r1", "question": "Renew?", "option": ["yes=Renew", "no=Let lapse"], "recommend": "yes",
                              "deadline": "2030-01-02T15:04:05Z", "default_on_timeout": "no"}, "s1"))
    assert out["results"][0]["exit_code"] == 0
    (req,) = kernel["log"]
    assert req["path"] == "/v1/items/r1/ask" and req["session"] == "s1"
    ask = req["body"]["ask"]
    assert ask["question"] == "Renew?" and ask["options"] == [{"id": "yes", "label": "Renew"}, {"id": "no", "label": "Let lapse"}]
    assert ask["recommend"] == "yes" and ask["default_on_timeout"] == "no"


def test_accept_and_dismiss_carry_session(hermes, kernel):
    hermes(("apa_accept", {"id": "r1", "evidence": "yes renew"}, "s1"), ("apa_dismiss", {"id": "r2", "reason": "no"}, "s1"))
    assert [(r["path"], r["session"]) for r in kernel["log"]] == [("/v1/items/r1/accept", "s1"), ("/v1/items/r2/dismiss", "s1")]


def test_flow_brief_and_guideline_read_tier_run_reads_materials_and_writes_back_via_apa_tools(hermes, kernel):
    out = hermes(("apa_list_dir", {"path": "."}, "s1"),
                 ("apa_item_update", {"id": "i1", "spec": {"period": "2026-W45", "sections": [{"project": "p", "text": "ok"}]},
                                      "reason": "brief"}, "s1"),
                 ("apa_material_commit", {"kind": "guideline", "title": "G", "project": "p", "reason": "draft",
                                          "content": "# 规范\n只写仓库里看得到的事实"}, "s1"))
    assert out["results"][0]["ok"] is True  # file tools are registered and dispatched by Hermes (cwd is HERMES_SRC)
    upd, com = kernel["log"]
    assert upd["method"] == "PATCH" and upd["path"] == "/v1/items/i1" and upd["session"] == "s1"
    assert upd["body"]["spec"] == {"period": "2026-W45", "sections": [{"project": "p", "text": "ok"}]}
    assert com["path"] == "/v1/materials" and com["body"]["content"].startswith("# 规范")
    assert com["body"]["kind"] == "guideline" and com["body"]["project"] == "p"
