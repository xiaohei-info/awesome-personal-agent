"""Business tests: a claimed wake delivery opens a turn in the named session, carries the run identity into that
turn's apa calls, and every failure goes back to the kernel without touching Hermes. Stand-ins: the kernel HTTP
endpoint (kernel_stub), `apa` (a shell script that records its identity env) and Hermes' ctx."""
import json
import stat
import threading
import time

import pytest

SESSION = "agent:main:telegram:dm:42"


class Ctx:
    def __init__(self, accept=True):
        self.accept, self.injected, self.tools, self.hooks = accept, [], {}, []

    def inject_message(self, content, role="user", *, session_key=None, origin=None):
        self.injected.append((content, session_key))
        return self.accept

    def register_tool(self, name, toolset, description, handler, schema):
        self.tools[name] = handler


def fake_apa(tmp_path, monkeypatch, body='echo "$APA_RUN|$APA_TOKEN|$*"'):
    p = tmp_path / "apa"
    p.write_text("#!/bin/sh\n" + body + "\n")
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("APA_BIN", str(p))


def test_wake_delivery_opens_turn_with_run_id_and_no_credential(wake, kernel):
    kernel.wake()
    ctx = Ctx()
    d = wake.poll_once(ctx, SESSION, wait=0)
    assert d["run"] == "r1"
    (text, key), = ctx.injected
    assert key == SESSION and "r1" in text and "it1" in text and "apa_wake_cli" in text
    assert "apa_run_" not in text  # the credential never enters the session history
    assert kernel.reports == [("ack", "wake_r1", {"version": 1, "external_ref": "r1"})]


def test_empty_queue_injects_nothing(wake, kernel):
    ctx = Ctx()
    assert wake.poll_once(ctx, SESSION, wait=0) is None
    assert ctx.injected == [] and kernel.reports == []


def test_turn_cli_calls_carry_run_identity(wake, kernel, tmp_path, monkeypatch):
    fake_apa(tmp_path, monkeypatch)
    kernel.wake()
    ctx = Ctx()
    wake.register(ctx)
    wake.poll_once(ctx, SESSION, wait=0)
    out = json.loads(ctx.tools["apa_wake_cli"]({"args": ["context"], "run_id": "r1"}))
    assert out["ok"] and out["stdout"] == "r1|apa_run_r1_1|context"
    # run_id may be omitted while exactly one wake run is active
    assert json.loads(ctx.tools["apa_wake_cli"]({"args": ["report"]}))["stdout"].startswith("r1|apa_run_r1_1|report")


def test_kernel_verdict_passes_through(wake, kernel, tmp_path, monkeypatch):
    fake_apa(tmp_path, monkeypatch, 'echo "web_only" >&2; exit 4')
    kernel.wake()
    ctx = Ctx()
    wake.register(ctx)
    wake.poll_once(ctx, SESSION, wait=0)
    out = json.loads(ctx.tools["apa_wake_cli"]({"args": ["decide", "x"], "run_id": "r1"}))
    assert (out["ok"], out["exit_code"], out["stderr"]) == (False, 4, "web_only")


def test_cli_without_wake_credential_is_refused(wake, tmp_path, monkeypatch):
    fake_apa(tmp_path, monkeypatch)
    ctx = Ctx()
    wake.register(ctx)
    out = json.loads(ctx.tools["apa_wake_cli"]({"args": ["context"], "run_id": "nope"}))
    assert out["ok"] is False and "no wake credential" in out["error"]
    assert "no wake run is active" in json.loads(ctx.tools["apa_wake_cli"]({"args": ["context"]}))["error"]


def test_expiring_credential_is_exchanged_again(wake, kernel, tmp_path, monkeypatch):
    fake_apa(tmp_path, monkeypatch)
    kernel.ttl = "2000-01-01T00:00:00Z"  # already expired when issued
    kernel.wake()
    ctx = Ctx()
    wake.register(ctx)
    wake.poll_once(ctx, SESSION, wait=0)
    kernel.ttl = "2099-01-01T00:00:00Z"
    out = json.loads(ctx.tools["apa_wake_cli"]({"args": ["context"], "run_id": "r1"}))
    assert out["stdout"] == "r1|apa_run_r1_2|context" and kernel.exchanges == 2


def test_refused_injection_fails_delivery_and_drops_credential(wake, kernel, tmp_path, monkeypatch):
    fake_apa(tmp_path, monkeypatch)
    kernel.wake()
    ctx = Ctx(accept=False)
    wake.register(ctx)
    with pytest.raises(wake.WakeError):
        wake.poll_once(ctx, SESSION, wait=0)
    (kind, did, body), = kernel.reports
    assert (kind, did, body["version"]) == ("fail", "wake_r1", 1) and "refused" in body["reason"]
    assert "no wake credential" in json.loads(ctx.tools["apa_wake_cli"]({"args": ["context"], "run_id": "r1"}))["error"]


def test_failed_exchange_fails_delivery_without_injecting(wake, kernel):
    kernel.fail_exchange = True
    kernel.wake()
    ctx = Ctx()
    with pytest.raises(wake.WakeError):
        wake.poll_once(ctx, SESSION, wait=0)
    assert ctx.injected == [] and kernel.reports[0][0] == "fail"


def test_delivery_without_run_id_is_failed(wake, kernel):
    kernel.wake(run="")
    with pytest.raises(wake.WakeError, match="no run id"):
        wake.poll_once(Ctx(), SESSION, wait=0)
    assert kernel.reports[0][0] == "fail"


def test_bad_plugin_token_surfaces_kernel_error(wake, monkeypatch):
    monkeypatch.setenv("APA_TOKEN", "wrong")
    with pytest.raises(wake.WakeError, match="401"):
        wake.poll_once(Ctx(), SESSION, wait=0)


def test_missing_kernel_settings_is_an_error(wake, monkeypatch):
    monkeypatch.delenv("APA_URL")
    with pytest.raises(wake.WakeError, match="APA_URL"):
        wake.poll_once(Ctx(), SESSION, wait=0)


def test_unreachable_kernel_is_wake_error(wake, kernel, monkeypatch):
    kernel.stop()
    with pytest.raises(wake.WakeError):
        wake.poll_once(Ctx(), SESSION, wait=0)


def test_wake_is_opt_in(wake, kernel, monkeypatch):
    kernel.wake()
    ctx = Ctx()
    wake.register(ctx)  # no APA_WAKE_SESSION_KEY: only the tool exists, nothing is claimed
    time.sleep(0.3)
    assert ctx.injected == [] and len(kernel.deliveries) == 1 and "apa_wake_cli" in ctx.tools


def test_registered_loop_claims_in_background_and_survives_errors(wake, kernel, monkeypatch):
    monkeypatch.setenv("APA_WAKE_SESSION_KEY", SESSION)
    monkeypatch.setattr(wake, "LONG_POLL_SECS", 0)
    monkeypatch.setattr(wake, "BACKOFF_SECS", 0.05)
    kernel.wake(run="")      # first delivery fails -> loop must keep going
    kernel.wake(did="wake_r2", run="r2")
    ctx = Ctx()
    wake.stop.clear()
    wake.register(ctx)
    deadline = time.time() + 5
    while time.time() < deadline and not ctx.injected:
        time.sleep(0.05)
    wake.stop.set()
    assert ctx.injected and ctx.injected[0][1] == SESSION and "r2" in ctx.injected[0][0]
    assert [r[0] for r in kernel.reports][:2] == ["fail", "ack"]


def test_cli_rejects_malformed_args(wake):
    ctx = Ctx()
    wake.register(ctx)
    assert "list of strings" in json.loads(ctx.tools["apa_wake_cli"]({"args": "context"}))["error"]


# 唤醒回合的 apa 调用用运行凭据，没有代答能力，不带会话类型，也不继承网关环境里的旧值（附录 A.5）。
def test_wake_cli_does_not_report_or_inherit_a_session_kind(wake, kernel, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_SESSION_KIND", "direct")
    fake_apa(tmp_path, monkeypatch, body='echo "kind=[$APA_SESSION_KIND]"')
    kernel.wake()
    ctx = Ctx()
    wake.register(ctx)
    wake.poll_once(ctx, SESSION, wait=0)
    out = json.loads(ctx.tools["apa_wake_cli"]({"args": ["context"], "run_id": "r1"}))
    assert out["stdout"] == "kind=[]"
