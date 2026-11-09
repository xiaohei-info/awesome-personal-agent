"""Live gate: a real `hermes chat --oneshot` run built from declaration.json exactly as the kernel builds it
(command + output + access + resume), with the real `apa` binary from this repo and the real tools plugin.

Only the kernel HTTP endpoint is a stand-in: it records each request (path, Authorization, X-APA-Session, body).
Needs a configured Hermes (model + credentials in $HERMES_HOME) and Go; skips otherwise unless APA_REQUIRE_LIVE=1.
The temp HERMES_HOME is a copy of config.yaml with our plugins enabled; .env/auth.json are symlinked, never copied.
"""
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
import yaml

from helpers import HEADLESS, ROOT, kernel_argv

REAL_HOME = pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes"))
REQUIRE_LIVE = os.environ.get("APA_REQUIRE_LIVE") == "1"
EXPECTED_TERMINAL_MARKER = "MARK_TOKEN_VISIBLE"  # measured on v0.22.5; change with the README row if a Hermes version differs


def _problem():
    if not shutil.which("hermes") or not shutil.which("go"):
        return "needs hermes and go on PATH"
    if not (REAL_HOME / "config.yaml").exists():
        return f"no configured Hermes at {REAL_HOME}"
    return None


_PROBLEM = _problem()
if _PROBLEM and REQUIRE_LIVE:
    pytest.fail("live gate required (APA_REQUIRE_LIVE=1) but cannot run: " + _PROBLEM, pytrace=False)
pytestmark = pytest.mark.skipif(bool(_PROBLEM), reason=_PROBLEM or "")


@pytest.fixture(scope="module")
def apa_bin():
    d = tempfile.mkdtemp()
    out = os.path.join(d, "apa")
    subprocess.run(["go", "build", "-o", out, "./cli/apa"], cwd=ROOT, check=True)
    yield out
    shutil.rmtree(d, ignore_errors=True)


@pytest.fixture
def kernel():
    state = {"log": [], "replies": {"/delegations": {"id": "child1"}}}

    class H(BaseHTTPRequestHandler):
        def _serve(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n) or b"null")
            state["log"].append({"path": self.path, "auth": self.headers.get("Authorization"),
                                 "session": self.headers.get("X-APA-Session"), "body": body})
            data = json.dumps(next((v for k, v in state["replies"].items() if self.path.split("?")[0].endswith(k)),
                                   {"ok": True})).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        do_GET = do_POST = _serve

        def log_message(self, *a):
            pass

    srv = HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    state["url"] = f"http://127.0.0.1:{srv.server_port}"
    yield state
    srv.shutdown()


def _transcript(argv, rc, lines):
    """APA_LIVE_TRANSCRIPT=<file>: append argv, exit code and the stream-json result line for a reviewer without Hermes."""
    path = os.environ.get("APA_LIVE_TRANSCRIPT")
    if path:
        with open(path, "a") as f:
            f.write(json.dumps({"argv": argv, "exit": rc, "last_stdout_line": lines[-1] if lines else None}, ensure_ascii=False) + "\n")


@pytest.fixture
def run_hermes(apa_bin, kernel):
    home = pathlib.Path(tempfile.mkdtemp())
    (home / "plugins").mkdir()
    for p in (REAL_HOME / "plugins").iterdir():
        (home / "plugins" / p.name).symlink_to(p)
    (home / "plugins" / "apa-tools").symlink_to(ROOT / "plugins" / "hermes" / "tools")
    (home / "plugins" / "apa-headless").symlink_to(HEADLESS)
    for name in (".env", "auth.json"):
        if (REAL_HOME / name).exists():
            (home / name).symlink_to(REAL_HOME / name)
    cfg = yaml.safe_load((REAL_HOME / "config.yaml").read_text())
    cfg.setdefault("plugins", {}).setdefault("enabled", []).extend(["apa-tools", "apa-headless"])
    cfg.setdefault("terminal", {}).setdefault("client_env_passthrough", []).append("APA_RUN_CONTEXT")
    (home / "config.yaml").write_text(yaml.safe_dump(cfg))
    work = home / "run_dir"
    work.mkdir()
    context = home / "run_live1.context"  # what the kernel writes at launch: tools run in the gateway, not this process
    context.write_text(json.dumps({"run": "run_live1", "url": kernel["url"], "token": "run-credential-xyz", "run_dir": str(work), "work_dir": str(work)}))
    env = {**os.environ, "HERMES_HOME": str(home), "APA_BIN": apa_bin, "APA_URL": kernel["url"], "APA_RUN": "run_live1",
           "APA_TOKEN": "run-credential-xyz", "APA_HOME": str(home / "apahome"), "APA_RUN_CONTEXT": str(context)}
    decl = json.loads((HEADLESS / "declaration.json").read_text())

    def run(prompt, tier="edit", session_id=None):
        argv = kernel_argv(decl, prompt, tier, session_id)
        p = subprocess.run(argv, cwd=work, env=env, capture_output=True, text=True, timeout=280)
        lines = p.stdout.strip().splitlines()
        _transcript(argv, p.returncode, lines)
        return p.returncode, json.loads(lines[-1]), p.stdout

    yield run
    shutil.rmtree(home, ignore_errors=True)


def test_flow_bg_main_session_and_resume_headless_run_delegates_asks_and_resumes_same_session(run_hermes, kernel):
    rc, result, _ = run_hermes(
        "You are a background runner. Do these in order, using tools, no questions back: "
        "1) call apa_delegate with parent=P1 assignee=hermes.subagent title=fix. "
        "2) call apa_ask with id=P1 question='Renew?' option=['yes=Renew','no=Lapse'] resume_note='continue after answer'. "
        "3) run the shell command: printenv APA_RUN; test -n \"$APA_TOKEN\" && echo TOKEN_VISIBLE. "
        "4) finish with one line containing the printenv output and TOKEN_VISIBLE if it printed.")
    # the kernel reads the LAST stdout line for the session id and exit code
    assert rc == 0 and result["type"] == "result" and result["session_id"] and result["exit_code"] == 0
    sid = result["session_id"]
    by_path = {r["path"]: r for r in kernel["log"]}
    # ACT-BGM-04: the run's own credential and the Hermes session id reach the delegate call
    assert by_path["/v1/delegations"]["auth"] == "Bearer run-credential-xyz"
    assert by_path["/v1/delegations"]["session"] == sid
    assert by_path["/v1/delegations"]["body"]["assignee"] == "hermes.subagent"
    # ACT-RES-01: the runner submits the decision request with resume_note, then ends (process exited rc 0)
    ask = by_path["/v1/items/P1/ask"]["body"]
    assert ask["ask"]["question"] == "Renew?" and ask["ask"]["resume_note"] == "continue after answer"
    assert "run_live1" in result["text"]

    # ACT-RES-02: --resume keeps the same session and the runner still remembers the earlier turn
    _transcript(["kernel_request_log"], 0, [json.dumps([{**r, "auth": "<redacted>" if r["auth"] else None} for r in kernel["log"]])])
    kernel["log"].clear()
    rc2, result2, _ = run_hermes(
        "The owner answered yes. Call apa_report with id=child1 outcome=done summary=ok, then reply with the parent item id you used earlier.",
        session_id=sid)
    assert rc2 == 0 and result2["session_id"] == sid and "P1" in result2["text"]
    (rep,) = [r for r in kernel["log"] if r["path"] == "/v1/items/child1/report"]
    _transcript(["kernel_request_log"], 0, [json.dumps([{**r, "auth": "<redacted>" if r["auth"] else None} for r in kernel["log"]])])
    assert rep["session"] == sid and rep["auth"] == "Bearer run-credential-xyz" and rep["body"]["outcome"] == "done"


def test_unknown_session_fails_with_exit_1_and_an_error_result(run_hermes):
    rc, result, _ = run_hermes("x", session_id="no-such-session")
    assert rc == 1 and result["exit_code"] == 1 and "No session found" in result["error"]


def test_read_tier_has_no_terminal_or_file_tools(run_hermes):
    rc, result, _ = run_hermes("Try to run the shell command `echo hi` with a terminal tool. If you have no terminal tool, reply exactly NO_TERMINAL.", tier="read")
    assert rc == 0 and "NO_TERMINAL" in result["text"]


def test_terminal_tool_credential_visibility_is_measured(run_hermes):
    """§21.3: does the run credential survive into Hermes' `terminal` subprocess? Asserts the marker the shell really printed."""
    rc, result, _ = run_hermes(
        "Use the terminal tool exactly once with this command and nothing else: "
        "sh -c 'if [ -n \"$APA_TOKEN\" ]; then echo MARK_TOKEN_VISIBLE; else echo MARK_TOKEN_SCRUBBED; fi'. "
        "Then reply with only the marker word it printed.")
    assert rc == 0
    seen = [m for m in ("MARK_TOKEN_VISIBLE", "MARK_TOKEN_SCRUBBED") if m in result["text"]]
    assert seen == [EXPECTED_TERMINAL_MARKER], result["text"]
