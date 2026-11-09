"""Every plugin that runs `apa` from inside the gateway: the call carries this session's run identity and cwd,
never what the gateway process inherited from whichever client started it."""
import importlib.util
import json
import os
import stat
import sys
import threading

import pytest

from helpers import ROOT, fake_gateway, load_tools
from test_tools import Ctx

HERMES = ROOT / "plugins" / "hermes"
STALE_MARK = "/first-run/run-credentials/.apa-run"  # the marker a gateway started by a run's client keeps in its own PATH
RUN_PATH = "/usr/bin:/home/o/.apa/run-credentials/.apa-run"  # the PATH a kernel-launched client sends

# Records what `apa` was started with, one JSON line each.
FAKE = """#!/bin/sh
exec python3 - "$@" <<'PY2'
import json, os, sys
keys = ("APA_RUN_CONTEXT", "APA_RUN", "APA_TOKEN", "APA_PROJECT", "APA_RUN_CREDENTIAL_FILE", "APA_SESSION", "APA_HARNESS", "PATH")
line = {"argv": sys.argv[1:], "cwd": os.getcwd(), **{k: os.environ.get(k) for k in keys}}
open(os.environ["APA_LOG"], "a").write(json.dumps(line) + "\\n")
PY2
"""


@pytest.fixture
def gateway(tmp_path, monkeypatch):
    """A gateway a run's client started: its cwd is not the session's and its env still names that run."""
    gateway_cwd, work = tmp_path / "hermes-src", tmp_path / "checkout"
    gateway_cwd.mkdir()
    work.mkdir()
    monkeypatch.chdir(gateway_cwd)
    fake = tmp_path / "apa"
    fake.write_text(FAKE)
    fake.chmod(fake.stat().st_mode | stat.S_IEXEC)
    # the plugins' default paths must never reach the real home
    for k, v in {"HERMES_HOME": str(tmp_path / "hermes-home"), "APA_HOME": str(tmp_path / "apa-home"), "APA_BIN": str(fake),
                 "APA_LOG": str(tmp_path / "log"), "PATH": os.environ["PATH"] + os.pathsep + STALE_MARK, "APA_RUN": "run_first", "APA_TOKEN": "apa_run_stale",
                 "APA_PROJECT": "stale-project", "APA_RUN_CONTEXT": "/stale/run_first",
                 "APA_RUN_CREDENTIAL_FILE": "/stale/cred"}.items():
        monkeypatch.setenv(k, v)
    context = tmp_path / "run_now"
    context.write_text("{}")
    return {"session": fake_gateway(monkeypatch), "context": context, "work": work, "log": tmp_path / "log"}


def calls(gateway):
    return [json.loads(l) for l in gateway["log"].read_text().splitlines()]


def assert_run_identity(call, gateway):
    assert call["APA_RUN_CONTEXT"] == str(gateway["context"])
    assert (call["APA_RUN"], call["APA_TOKEN"], call["APA_PROJECT"], call["APA_RUN_CREDENTIAL_FILE"]) == (None, None, None, None)
    assert call["APA_HARNESS"] == "hermes" and ".apa-run" not in call["PATH"]
    assert os.path.realpath(call["cwd"]) == os.path.realpath(gateway["work"])


def load_plugin(name):
    """Import plugins/hermes/<name> the way Hermes does, under a private name."""
    mod_name = f"apa_test_{name}"
    for k in [k for k in sys.modules if k == mod_name or k.startswith(mod_name + ".")]:
        del sys.modules[k]
    spec = importlib.util.spec_from_file_location(mod_name, HERMES / name / "__init__.py", submodule_search_locations=[str(HERMES / name)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules[mod_name] = mod
    spec.loader.exec_module(mod)
    return mod


def test_cli_tool_in_a_run_session_carries_the_runs_identity_and_cwd(gateway):
    ctx = Ctx()
    load_tools().register(ctx)
    with gateway["session"](gateway["context"], gateway["work"]):
        ctx.tools["apa_item_update"][1]({"id": "i1", "reason": "r"}, session_id="s1")
    [call] = calls(gateway)
    assert_run_identity(call, gateway)
    assert call["APA_SESSION"] == "s1" and call["argv"][:3] == ["item", "update", "i1"]


def test_session_without_a_run_drops_the_inherited_run_and_keeps_no_stale_context(gateway):
    ctx = Ctx()
    load_tools().register(ctx)
    with gateway["session"](None, gateway["work"]):
        ctx.tools["apa_executors"][1]({}, session_id="chat")
    [call] = calls(gateway)
    assert (call["APA_RUN_CONTEXT"], call["APA_RUN"], call["APA_TOKEN"]) == (None, None, None)
    assert call["APA_SESSION"] == "chat"


def test_operator_token_of_a_gateway_that_was_not_started_by_a_run_is_kept(gateway, monkeypatch):
    monkeypatch.delenv("APA_RUN")
    monkeypatch.setenv("APA_TOKEN", "worker-token")
    ctx = Ctx()
    load_tools().register(ctx)
    with gateway["session"](None, ""):
        ctx.tools["apa_executors"][1]({}, session_id="chat")
    [call] = calls(gateway)
    assert call["APA_TOKEN"] == "worker-token"
    assert os.path.basename(call["cwd"]) == "hermes-src"  # a session with no cwd (messaging, cron) keeps the process default


def test_parallel_sessions_each_carry_their_own_identity(gateway, tmp_path):
    ctx = Ctx()
    load_tools().register(ctx)
    handler = ctx.tools["apa_executors"][1]
    barrier = threading.Barrier(2)

    def turn(name):
        with gateway["session"](tmp_path / f"ctx_{name}", gateway["work"]):
            barrier.wait(timeout=10)
            handler({}, session_id=name)

    threads = [threading.Thread(target=turn, args=(n,)) for n in ("a", "b")]
    for t in threads:
        t.start()
    for t in threads:
        t.join(30)
    assert {c["APA_SESSION"]: c["APA_RUN_CONTEXT"] for c in calls(gateway)} == {
        "a": str(tmp_path / "ctx_a"), "b": str(tmp_path / "ctx_b")}


def test_core_hook_cli_carries_the_runs_identity(gateway):
    load_plugin("core")
    apa = sys.modules["apa_test_core.apa"]
    with gateway["session"](gateway["context"], gateway["work"]):
        apa.run(["persona"], "s1")
    assert_run_identity(calls(gateway)[0], gateway)


def test_inject_reads_the_session_scope_before_its_worker_threads(gateway):
    """Its bare threads start with an empty context, so the scope must be read on the hook's thread."""
    inject = load_plugin("inject")
    with gateway["session"](gateway["context"], gateway["work"]):
        inject.pre_llm_call(session_id="s1", is_first_turn=True)
    log = calls(gateway)
    assert len(log) == 2
    for call in log:
        assert_run_identity(call, gateway)
        assert call["APA_SESSION"] == "s1"


def test_receipt_report_carries_the_runs_identity(gateway):
    receipt = load_plugin("receipt")
    with gateway["session"](gateway["context"], gateway["work"]):
        receipt._report("itm_1", "progress", "working", "s1")
    for t in [t for t in threading.enumerate() if t is not threading.current_thread() and t.daemon]:
        t.join(10)
    [call] = calls(gateway)
    assert call["APA_RUN_CONTEXT"] == str(gateway["context"]) and call["APA_RUN"] is None and call["APA_TOKEN"] is None
    assert call["argv"][:2] == ["report", "itm_1"]


def test_headless_declare_carries_the_runs_identity_so_the_kernel_can_refuse_it(gateway):
    headless = load_plugin("headless")
    with gateway["session"](gateway["context"], gateway["work"]):
        headless._declare({}, session_id="s1")
    [call] = calls(gateway)
    assert call["APA_RUN_CONTEXT"] == str(gateway["context"]) and call["APA_RUN"] is None


def test_wake_cli_identity_is_explicit_and_ignores_an_inherited_context(gateway):
    wake = load_plugin("wake")
    wake._runs["run_w"] = {"token": "wake-token", "expires": 9e12}
    json.loads(wake._cli({"args": ["context"], "run_id": "run_w"}))
    [call] = calls(gateway)
    assert (call["APA_RUN"], call["APA_TOKEN"], call["APA_RUN_CONTEXT"]) == ("run_w", "wake-token", None)
    assert ".apa-run" not in call["PATH"]  # the CLI would read an inherited marker as a run without its context


def test_capture_uses_the_session_cwd_and_the_runs_credential_never_the_saved_file(gateway, tmp_path, monkeypatch):
    capture = load_plugin("capture")
    sent = []
    monkeypatch.setattr(capture, "_spawn", sent.append)
    monkeypatch.setattr(capture, "_has_kept", lambda: False)
    with gateway["session"](gateway["context"], gateway["work"]):
        capture.post_llm_call("s1", "hi", "there")
        capture.on_session_end("s1")
    [payload] = sent
    assert payload["path"] == str(gateway["work"]) and payload["context"] == str(gateway["context"])

    state = tmp_path / "state"
    monkeypatch.setenv("APA_HERMES_CAPTURE_DIR", str(state))
    monkeypatch.setattr(capture, "_post", lambda item, env: "unreachable")  # keep it so the stored form can be checked
    capture._send(dict(payload), dict(os.environ))
    [kept] = [json.loads(p.read_text()) for p in (state / "pending").glob("*.json")]
    assert kept["context"] == str(gateway["context"]) and "context" not in kept["body"]

    gateway["context"].write_text(json.dumps({"url": "http://k", "token": "run-token"}))
    assert capture._credentials({}, str(gateway["context"])) == ("http://k", "run-token")
    gateway["context"].unlink()
    with pytest.raises(OSError):  # run over: _post logs it and sends nothing
        capture._credentials({"APA_URL": "http://k", "APA_TOKEN": "gateway"}, str(gateway["context"]))


def test_capture_of_a_session_without_cwd_sends_no_gateway_path(gateway, monkeypatch):
    capture = load_plugin("capture")
    sent = []
    monkeypatch.setattr(capture, "_spawn", sent.append)
    monkeypatch.setattr(capture, "_has_kept", lambda: False)
    with gateway["session"](None, ""):
        capture.post_llm_call("s1", "hi", "there")
        capture.on_session_end("s1")
    assert sent[0]["path"] == "" and sent[0]["context"] is None


# ---- the passthrough line is missing: a run must fail loudly, never use the gateway's credential

def missing_line_session(gateway):
    return gateway["session"](None, gateway["work"], client_path=RUN_PATH)


def assert_no_apa(gateway):
    assert not gateway["log"].exists()


def test_cli_tool_in_a_run_without_its_context_errors_and_never_runs_apa(gateway):
    ctx = Ctx()
    load_tools().register(ctx)
    with missing_line_session(gateway):
        r = json.loads(ctx.tools["apa_decide"][1]({"id": "i1", "option": "A", "evidence": "x"}, session_id="s1"))
    assert r["ok"] is False and "client_env_passthrough: [APA_RUN_CONTEXT]" in r["error"] and "gateway's own credential" in r["error"]
    assert_no_apa(gateway)


def test_file_tool_in_a_run_without_its_context_names_the_missing_line(gateway):
    ctx = Ctx()
    load_tools().register(ctx)
    with missing_line_session(gateway):
        r = json.loads(ctx.tools["apa_read_file"][1]({"path": "README.md"}))
    assert r["ok"] is False and "did not reach Hermes" in r["error"] and "client_env_passthrough: [APA_RUN_CONTEXT]" in r["error"]


def test_hooks_in_a_run_without_its_context_never_run_apa(gateway, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_RECEIPT_LOG", str(tmp_path / "receipt.log"))
    monkeypatch.setenv("APA_HERMES_CAPTURE_DIR", str(tmp_path / "capture"))
    load_plugin("core")
    inject, receipt, capture = load_plugin("inject"), load_plugin("receipt"), load_plugin("capture")
    popen = []
    monkeypatch.setattr(capture.subprocess, "Popen", lambda *a, **k: popen.append(a))
    with missing_line_session(gateway):
        assert sys.modules["apa_test_core.apa"].run(["persona"], "s1") == ""
        assert inject.pre_llm_call(session_id="s1", is_first_turn=True) is None
        receipt._report("itm_1", "progress", "working", "s1")
        capture.post_llm_call("s1", "hi", "there")
        capture.on_session_end("s1")
    assert_no_apa(gateway)
    assert not popen  # capture's helper would have posted with the gateway's credential
    assert "did not reach Hermes" in (tmp_path / "receipt.log").read_text()
    assert "did not reach Hermes" in (tmp_path / "capture" / "capture.log").read_text()


def test_run_with_its_context_works_normally_whatever_the_path_says(gateway):
    ctx = Ctx()
    load_tools().register(ctx)
    with gateway["session"](gateway["context"], gateway["work"], client_path=RUN_PATH):
        ctx.tools["apa_item_update"][1]({"id": "i1", "reason": "r"}, session_id="s1")
    assert_run_identity(calls(gateway)[0], gateway)


def test_ordinary_session_is_not_refused_because_the_gateway_inherited_a_marker(gateway):
    """Only the session's own PATH counts; an ordinary client's has no marker, an unbound session has no PATH at all."""
    ctx = Ctx()
    load_tools().register(ctx)
    for client_path in ("/usr/bin:/bin", None):
        with gateway["session"](None, gateway["work"], client_path=client_path):
            r = json.loads(ctx.tools["apa_executors"][1]({}, session_id="chat"))
        assert r["ok"] is True
    for call in calls(gateway):
        assert (call["APA_RUN_CONTEXT"], call["APA_RUN"], call["APA_TOKEN"]) == (None, None, None)
        assert ".apa-run" not in call["PATH"]


# ---- declaring: the profile must allow the context

def test_declaring_without_the_line_is_refused_and_says_what_to_add(gateway):
    headless = load_plugin("headless")
    for config in ({}, {"terminal": {}}, {"terminal": {"client_env_passthrough": ["FOO"]}}, {"terminal": {"client_env_passthrough": "APA_RUN_CONTEXT"}}):
        gateway["session"].config = config
        with gateway["session"](None, gateway["work"]):
            r = json.loads(headless._declare({}, session_id="s1"))
        assert r["ok"] is False and "terminal:\n  client_env_passthrough: [APA_RUN_CONTEXT]" in r["error"], config
    assert_no_apa(gateway)


def test_declaring_with_an_unreadable_config_is_refused(gateway):
    headless = load_plugin("headless")
    gateway["session"].config = RuntimeError("bad yaml")
    with gateway["session"](None, gateway["work"]):
        r = json.loads(headless._declare({}, session_id="s1"))
    assert r["ok"] is False and "cannot confirm" in r["error"] and "RuntimeError" in r["error"]
    assert_no_apa(gateway)
    with pytest.raises(ValueError, match="cannot confirm"):
        headless.declaration()


def test_declaring_with_the_line_goes_through(gateway):
    headless = load_plugin("headless")
    gateway["session"].config = {"terminal": {"client_env_passthrough": ["GEM_HOME", "APA_RUN_CONTEXT"]}}
    with gateway["session"](None, gateway["work"]):
        r = json.loads(headless._declare({}, session_id="owner"))
    assert r["ok"] is True
    [call] = calls(gateway)
    assert call["argv"] == ["declare", "-"] and call["APA_SESSION"] == "owner"


def test_the_run_marker_name_is_the_kernels_and_the_clis():
    assert 'RunMark = ".apa-run"' in (ROOT / "kernel" / "outcall" / "proc" / "dispatcher.go").read_text()
    assert 'runMark = ".apa-run"' in (ROOT / "cli" / "client" / "client.go").read_text()


def test_the_variable_name_survives_hermes_client_env_secret_filter():
    """Hermes drops client_env names containing these (tools/code_execution_env.py:_SECRET_SUBSTRINGS, v0.23.0)."""
    secret = ("KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN", "WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE")
    from_kernel = 'EnvRunContext = "APA_RUN_CONTEXT"'
    assert from_kernel in (ROOT / "kernel" / "outcall" / "proc" / "dispatcher.go").read_text()
    assert not [s for s in secret if s in "APA_RUN_CONTEXT"]
