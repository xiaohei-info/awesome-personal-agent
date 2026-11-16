"""Live gate: real Hermes (plugin loader + its invoke_hook dispatch for post_llm_call / on_session_end). Only the
kernel HTTP endpoint is a stand-in: it records each request (path, X-APA-Session, Authorization, body).

Skips when Hermes is missing (e.g. CI); APA_REQUIRE_LIVE=1 fails instead (gate hosts: the Mac mini).
"""
import json
import os
import pathlib
import subprocess
import textwrap

import pytest

from conftest import posts

ROOT = pathlib.Path(__file__).resolve().parents[4]
CAPTURE = ROOT / "plugins" / "hermes" / "capture"
HERMES_SRC = pathlib.Path(os.environ.get("HERMES_SRC", pathlib.Path.home() / ".hermes" / "hermes-agent"))
HERMES_PY = HERMES_SRC / "venv" / "bin" / "python"
REQUIRE_LIVE = os.environ.get("APA_REQUIRE_LIVE") == "1"
PROBE = "from hermes_cli import plugins as p; assert hasattr(p, 'discover_plugins') and 'on_session_end' in p.VALID_HOOKS and 'post_llm_call' in p.VALID_HOOKS"


def _live_problem():
    if not HERMES_PY.exists():
        return f"needs a Hermes install (HERMES_SRC={HERMES_SRC})"
    r = subprocess.run([str(HERMES_PY), "-c", PROBE], cwd=HERMES_SRC, capture_output=True, timeout=120)
    return None if r.returncode == 0 else "this Hermes lacks the post_llm_call / on_session_end hooks"


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


@pytest.fixture
def hermes(tmp_path, kernel):
    home = tmp_path / "hermes_home"
    (home / "plugins").mkdir(parents=True)
    (home / "plugins" / "apa-capture").symlink_to(CAPTURE)
    (home / "config.yaml").write_text("plugins:\n  enabled:\n    - apa-capture\n")
    env = {**os.environ, "HERMES_HOME": str(home), "APA_URL": kernel["url"], "APA_TOKEN": "t", "APA_HOME": str(tmp_path / "apahome")}
    env.pop("APA_RUN", None)

    def run(*hooks):
        p = subprocess.run([str(HERMES_PY), "-c", CHILD], cwd=HERMES_SRC, env=env, input=json.dumps(hooks),
                           capture_output=True, text=True, timeout=120, check=True)
        line = next(l for l in p.stdout.splitlines() if l.startswith("RESULT"))
        return json.loads(line[len("RESULT"):])

    run.env = env
    return run


def llm(sid, user, assistant):
    return ("post_llm_call", {"session_id": sid, "task_id": "t", "turn_id": "1", "user_message": user,
                              "assistant_response": assistant, "conversation_history": [], "model": "m", "platform": "cli"})


def end(sid):
    return ("on_session_end", {"session_id": sid, "task_id": "t", "turn_id": "1", "completed": True, "failed": False,
                               "interrupted": False, "turn_exit_reason": "text_response(stop)", "model": "m", "platform": "cli"})


def test_flow_capture_turn_end_submits_the_delta_and_the_model_sees_nothing(hermes, kernel):
    # ACT-CAP-01: the process exits right after the hook; the detached POST still lands.
    out = hermes(llm("main-1", "remind me to file taxes by April", "Added to your list."), end("main-1"))
    assert out == [[], []]  # no hook returned anything the model could see, and nothing forces a continuation
    (r,) = posts(kernel, 1)
    assert r["path"] == "/v1/capture" and r["session"] == "main-1" and r["auth"] == "Bearer t"
    assert r["body"]["text"] == "user: remind me to file taxes by April\nassistant: Added to your list."
    assert r["body"]["path"] == "" and r["body"]["host"]  # no session cwd is bound here, and the process cwd is never sent


def test_flow_capture_project_without_capture_is_a_quiet_no_op(hermes, kernel):
    # ACT-CAP-02: the kernel owns the switch (default off) and answers captured:false; Hermes is unaffected.
    kernel["reply"] = {"captured": False, "reason": "capture_disabled"}
    assert hermes(llm("main-2", "hello", "hi"), end("main-2")) == [[], []]
    assert len(posts(kernel, 1)) == 1
    # a session that said nothing posts nothing
    assert hermes(end("main-3")) == [[]]
    assert len(posts(kernel, 2, wait=1.5)) == 1


def test_flow_capture_kernel_down_never_breaks_hermes(hermes, kernel):
    # ACT-CAP-04
    hermes.env["APA_URL"] = "http://127.0.0.1:9"
    assert hermes(llm("main-4", "x", "y"), end("main-4")) == [[], []]
    assert posts(kernel, 1, wait=1.5) == []
