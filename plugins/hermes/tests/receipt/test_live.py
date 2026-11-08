"""Live gate: real Hermes (plugin loader + its invoke_hook dispatch for post_tool_call / subagent_stop) and the real
`apa` binary built from this repo. Only the kernel HTTP endpoint is a stand-in: it records each request
(path, X-APA-Session, body).

Skips when Hermes or Go is missing (e.g. CI); APA_REQUIRE_LIVE=1 fails instead (gate hosts: the Mac mini).
"""
import json
import os
import pathlib
import shutil
import subprocess
import textwrap
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[4]
RECEIPT = ROOT / "plugins" / "hermes" / "receipt"
HERMES_SRC = pathlib.Path(os.environ.get("HERMES_SRC", pathlib.Path.home() / ".hermes" / "hermes-agent"))
HERMES_PY = HERMES_SRC / "venv" / "bin" / "python"
REQUIRE_LIVE = os.environ.get("APA_REQUIRE_LIVE") == "1"
PROBE = "from hermes_cli import plugins as p; assert hasattr(p, 'discover_plugins') and 'subagent_stop' in p.VALID_HOOKS and 'post_tool_call' in p.VALID_HOOKS"
ITEM = "itm_0a1b2c3d4e5f"


def _live_problem():
    if not HERMES_PY.exists() or not shutil.which("go"):
        return f"needs a Hermes install (HERMES_SRC={HERMES_SRC}) and Go"
    r = subprocess.run([str(HERMES_PY), "-c", PROBE], cwd=HERMES_SRC, capture_output=True, timeout=120)
    return None if r.returncode == 0 else "this Hermes lacks the post_tool_call / subagent_stop hooks"


_PROBLEM = _live_problem()
if _PROBLEM and REQUIRE_LIVE:
    pytest.fail("live gate required (APA_REQUIRE_LIVE=1) but cannot run: " + _PROBLEM, pytrace=False)
pytestmark = pytest.mark.skipif(bool(_PROBLEM), reason=_PROBLEM or "")

# One Hermes process loads the plugin the normal way and fires the hooks exactly as the agent loop does.
CHILD = textwrap.dedent("""
    import json, sys
    from hermes_cli import plugins
    plugins.discover_plugins()
    out = []
    for hook, kw in json.load(sys.stdin):
        out.append(plugins.invoke_hook(hook, **kw))
    print("RESULT" + json.dumps(out))
""")


@pytest.fixture(scope="module")
def apa_bin(tmp_path_factory):
    out = tmp_path_factory.mktemp("bin") / "apa"
    subprocess.run(["go", "build", "-o", str(out), "./cli/apa"], cwd=ROOT, check=True)
    return str(out)


@pytest.fixture
def kernel():
    state = {"log": []}

    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            state["log"].append({"path": self.path, "session": self.headers.get("X-APA-Session"),
                                 "evidence": (self.headers.get("X-APA-Evidence") or "").encode("latin-1").decode("utf-8"),
                                 "body": json.loads(self.rfile.read(n) or b"null")})
            data = b'{"ok": true}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *a):
            pass

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    state["url"] = f"http://127.0.0.1:{srv.server_port}"
    yield state
    srv.shutdown()


@pytest.fixture
def hermes(tmp_path, apa_bin, kernel):
    home = tmp_path / "hermes_home"
    (home / "plugins").mkdir(parents=True)
    (home / "plugins" / "apa-receipt").symlink_to(RECEIPT)
    (home / "config.yaml").write_text("plugins:\n  enabled:\n    - apa-receipt\n")
    table = tmp_path / "apa-executors.json"
    table.write_text(json.dumps([{"id": "work-host.cli", "kind": "external-cli", "scenarios": ["coding"], "command": ["codex", "exec", "{prompt}"]}]))
    env = {**os.environ, "HERMES_HOME": str(home), "APA_BIN": apa_bin, "APA_URL": kernel["url"], "APA_TOKEN": "t",
           "APA_HOME": str(tmp_path / "apahome"), "APA_HERMES_EXECUTORS": str(table)}

    def run(*hooks):
        p = subprocess.run([str(HERMES_PY), "-c", CHILD], cwd=HERMES_SRC, env=env, input=json.dumps(hooks),
                           capture_output=True, text=True, timeout=120, check=True)
        line = next(l for l in p.stdout.splitlines() if l.startswith("RESULT"))
        return json.loads(line[len("RESULT"):])

    run.env = env
    return run


def reports(kernel, n, wait=15):
    end = time.time() + wait
    while time.time() < end and len(kernel["log"]) < n:
        time.sleep(0.1)
    time.sleep(0.5)  # a surplus report would arrive now
    return kernel["log"]


def delegate(goal, summary, status="completed"):
    return ("post_tool_call", {"tool_name": "delegate_task", "session_id": "main-1", "args": {"goal": goal},
                               "result": json.dumps({"results": [{"task_index": 0, "status": status, "summary": summary}]})})


def test_flow_delegate_recover_receipt_reaches_the_child_item_and_is_invisible_to_the_model(hermes, kernel):
    # ACT-DLG-06
    out = hermes(delegate(f"fix the bug. Ledger item: {ITEM}", "PR #12 opened"))
    assert out == [[]]  # no hook returned anything the model could see (invoke_hook collects non-None values)
    (r,) = reports(kernel, 1)
    assert r["path"] == f"/v1/items/{ITEM}/report" and r["session"] == "main-1"
    assert r["body"]["outcome"] == "done" and r["body"]["summary"] == "PR #12 opened"
    assert r["evidence"] == f"插件捕获：delegate_task 对 {ITEM} 的调用结果，会话 main-1；PR #12"  # kernel needs it under a delegate credential


def test_flow_delegate_recover_subagent_stop_and_terminal_executor_are_captured(hermes, kernel):
    other = "itm_ffee00112233"
    hermes(("subagent_stop", {"parent_session_id": "main-2", "child_summary": f"{ITEM} finished, PR #3", "child_status": "completed",
                              "child_role": None, "tool_call_history": [], "duration_ms": 1}),
           ("post_tool_call", {"tool_name": "terminal", "session_id": "main-2", "args": {"command": f"codex exec 'x {other}'"},
                               "result": json.dumps({"output": "", "exit_code": 1, "error": "crashed"})}))
    got = {r["path"]: r for r in reports(kernel, 2)}
    assert got[f"/v1/items/{ITEM}/report"]["body"]["outcome"] == "done"
    assert got[f"/v1/items/{ITEM}/report"]["evidence"].startswith("插件捕获：subagent_stop")
    assert got[f"/v1/items/{other}/report"]["body"]["outcome"] == "blocked" and got[f"/v1/items/{other}/report"]["session"] == "main-2"


def test_flow_delegate_recover_success_without_a_reference_stays_progress(hermes, kernel):
    hermes(delegate(f"go {ITEM}", "looks fine"))
    (r,) = reports(kernel, 1)
    assert r["body"]["outcome"] == "progress" and r["body"]["summary"] == "looks fine"


def test_flow_delegate_recover_no_item_id_no_report_and_hermes_unaffected(hermes, kernel):
    out = hermes(delegate("research the market", "done"),
                 ("post_tool_call", {"tool_name": "read_file", "session_id": "s", "args": {"path": ITEM}, "result": "{}"}))
    assert out == [[], []]
    assert reports(kernel, 1, wait=1.5) == []


def test_flow_delegate_recover_kernel_down_never_breaks_hermes(hermes, kernel):
    hermes.env["APA_URL"] = "http://127.0.0.1:9"
    assert hermes(delegate(f"go {ITEM}", "x")) == [[]]
    assert reports(kernel, 1, wait=1.5) == []
