"""Unit tests: the capture hooks against a stand-in kernel HTTP endpoint (test boundary only)."""
import importlib.util
import os
import pathlib
import sys
import time

import pytest

from conftest import posts

CAPTURE = pathlib.Path(__file__).resolve().parents[2] / "capture"


def load():
    """Import plugins/hermes/capture as package 'apa_capture' (fresh state each time)."""
    for k in [k for k in sys.modules if k == "apa_capture" or k.startswith("apa_capture.")]:
        del sys.modules[k]
    spec = importlib.util.spec_from_file_location("apa_capture", CAPTURE / "__init__.py", submodule_search_locations=[str(CAPTURE)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_capture"] = mod
    spec.loader.exec_module(mod)
    return mod


class Ctx:
    def __init__(self):
        self.hooks = {}

    def register_hook(self, name, cb):
        self.hooks[name] = cb


@pytest.fixture
def env(kernel, tmp_path, monkeypatch):
    for k in ("APA_RUN", "APA_RUN_CREDENTIAL_FILE"):
        monkeypatch.delenv(k, raising=False)
    monkeypatch.setenv("APA_URL", kernel["url"])
    monkeypatch.setenv("APA_TOKEN", "tok-1")
    monkeypatch.setenv("APA_HOME", str(tmp_path / "apahome"))
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    return monkeypatch


@pytest.fixture
def hooks(env):
    ctx = Ctx()
    load().register(ctx)
    return ctx.hooks


def turn(hooks, sid, user, assistant):
    hooks["post_llm_call"](session_id=sid, user_message=user, assistant_response=assistant, conversation_history=[])


def test_registers_exactly_the_two_hooks(hooks):
    assert set(hooks) == {"post_llm_call", "on_session_end"}


# ACT-CAP-01: the finished turn reaches POST /v1/capture asynchronously, keyed to the session and the working directory.
def test_turn_end_posts_the_delta_with_session_credential_and_location(hooks, kernel):
    turn(hooks, "s1", "we should renew the passport before March", "Noted, I can add that as a task.")
    assert hooks["on_session_end"](session_id="s1", completed=True) is None  # nothing for the model
    (r,) = posts(kernel, 1)
    assert r["path"] == "/v1/capture" and r["session"] == "s1" and r["auth"] == "Bearer tok-1"
    assert r["body"]["text"] == "user: we should renew the passport before March\nassistant: Noted, I can add that as a task."
    assert r["body"]["path"] == os.getcwd() and r["body"]["host"]
    assert "session" not in r["body"]


# Defect dfx-992917373ac0: a delegate credential needs the owner's own words; they ride as X-APA-Evidence (UTF-8 intact).
def test_the_turns_own_user_words_are_sent_as_evidence_and_failures_go_to_the_local_log(hooks, kernel, tmp_path):
    turn(hooks, "s1", "请帮我\n续一下护照", "好的")
    hooks["on_session_end"](session_id="s1", completed=True)
    (r,) = posts(kernel, 1)
    assert r["evidence"] == "请帮我 续一下护照"
    kernel["status"] = 400
    turn(hooks, "s2", "again", "ok")
    hooks["on_session_end"](session_id="s2", completed=True)
    posts(kernel, 2)
    log = tmp_path / "apa-capture.log"
    end = time.time() + 5
    while time.time() < end and not (log.exists() and "400" in log.read_text()):
        time.sleep(0.05)
    assert "400" in log.read_text()


# ACT-CAP-01: the cursor is per session; a turn is sent once, later turns send only what is new.
def test_each_turn_is_sent_once_and_sessions_do_not_mix(hooks, kernel):
    turn(hooks, "a", "first", "one")
    hooks["on_session_end"](session_id="a")
    hooks["on_session_end"](session_id="a")  # nothing new: no second post
    turn(hooks, "b", "other", "two")
    turn(hooks, "a", "second", "three")
    hooks["on_session_end"](session_id="a")
    hooks["on_session_end"](session_id="b")
    got = {(r["session"], r["body"]["text"]) for r in posts(kernel, 3)}
    assert got == {("a", "user: first\nassistant: one"), ("a", "user: second\nassistant: three"), ("b", "user: other\nassistant: two")}
    assert len(kernel["log"]) == 3


# ACT-CAP-02: default off. The kernel answers captured:false for a project without capture; hooks stay silent and harmless.
def test_kernel_declining_capture_is_not_an_error_and_nothing_is_visible(hooks, kernel):
    kernel["reply"] = {"captured": False, "reason": "capture_disabled"}
    turn(hooks, "s", "hello", "hi")
    assert hooks["on_session_end"](session_id="s") is None
    assert len(posts(kernel, 1)) == 1


# ACT-CAP-02: no conversation, nothing to send (no empty posts, no post without a session).
def test_nothing_to_capture_posts_nothing(hooks, kernel):
    hooks["on_session_end"](session_id="s")
    turn(hooks, "s", "  ", None)
    turn(hooks, "", "x", "y")
    hooks["on_session_end"](session_id="s")
    hooks["on_session_end"](session_id="")
    assert posts(kernel, 1, wait=1) == []


# ACT-CAP-02: under a run identity only the run credential is used, never the saved credentials file.
def test_run_identity_never_falls_back_to_the_saved_credentials(hooks, kernel, env, tmp_path):
    home = tmp_path / "apahome"
    home.mkdir()
    (home / "credentials.json").write_text('{"url": "%s", "token": "owner-token"}' % kernel["url"])
    env.delenv("APA_TOKEN")
    env.setenv("APA_RUN", "run_1")
    turn(hooks, "s", "x", "y")
    hooks["on_session_end"](session_id="s")
    assert posts(kernel, 1, wait=1) == []  # no run credential: nothing is sent, the owner token is not used
    cred = tmp_path / "run-cred"
    cred.write_text("run-token\n")
    env.setenv("APA_RUN_CREDENTIAL_FILE", str(cred))
    turn(hooks, "s", "x", "y")
    hooks["on_session_end"](session_id="s")
    (r,) = posts(kernel, 1)
    assert r["auth"] == "Bearer run-token"


def test_saved_credentials_are_used_without_env(hooks, kernel, env, tmp_path):
    home = tmp_path / "apahome"
    home.mkdir()
    (home / "credentials.json").write_text('{"url": "%s", "token": "saved-token"}' % kernel["url"])
    env.delenv("APA_URL")
    env.delenv("APA_TOKEN")
    turn(hooks, "s", "x", "y")
    hooks["on_session_end"](session_id="s")
    (r,) = posts(kernel, 1)
    assert r["auth"] == "Bearer saved-token"


# ACT-CAP-04: fail open. Dead kernel, 5xx, no credentials: hooks return None, raise nothing.
def test_kernel_down_or_failing_never_raises(hooks, kernel, env):
    env.setenv("APA_URL", "http://127.0.0.1:9")
    turn(hooks, "s", "x", "y")
    assert hooks["on_session_end"](session_id="s") is None
    env.setenv("APA_URL", kernel["url"])
    kernel["status"] = 500
    turn(hooks, "t", "x", "y")
    assert hooks["on_session_end"](session_id="t") is None
    assert len(posts(kernel, 1)) == 1


def test_malformed_hook_arguments_are_swallowed(hooks):
    assert hooks["post_llm_call"](session_id="s", user_message=object(), assistant_response=5) is None
    assert hooks["on_session_end"](session_id=object()) is None


def test_oversized_delta_keeps_the_most_recent_text(hooks, kernel):
    turn(hooks, "s", "old " * 10000, "answer")
    hooks["on_session_end"](session_id="s")
    (r,) = posts(kernel, 1)
    assert len(r["body"]["text"]) == 20000 and r["body"]["text"].endswith("assistant: answer")


# The detached helper runs in another process; these exercise its code in-process so coverage sees it.
def test_helper_sends_and_skips_without_credentials(kernel, env):
    mod = load()
    mod._send({"text": "t", "session": "s9", "path": "/p", "host": "h"}, {"APA_URL": kernel["url"], "APA_TOKEN": "k"})
    mod._send({"text": "t", "session": "s9"}, {"APA_RUN": "1", "APA_URL": kernel["url"]})  # run without a credential: no send
    (r,) = posts(kernel, 1)
    assert r["session"] == "s9" and r["auth"] == "Bearer k" and r["body"] == {"text": "t", "path": "/p", "host": "h"}


def test_helper_main_reads_stdin_and_swallows_errors(kernel, env, monkeypatch):
    import io
    import runpy
    monkeypatch.setattr(sys, "stdin", io.StringIO('{"text": "t", "session": "m"}'))
    runpy.run_path(str(CAPTURE / "__init__.py"), run_name="__main__")
    assert posts(kernel, 1)[0]["session"] == "m"
    monkeypatch.setattr(sys, "stdin", io.StringIO("not json"))
    runpy.run_path(str(CAPTURE / "__init__.py"), run_name="__main__")  # must not raise
