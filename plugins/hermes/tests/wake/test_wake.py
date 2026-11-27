"""Business tests: a claimed wake delivery opens a turn in the named session, carries the run identity into that
turn's apa calls, and every failure goes back to the kernel without touching Hermes. Stand-ins: the kernel HTTP
endpoint (kernel_stub), `apa` (a shell script that records its identity env) and Hermes' ctx."""
import importlib.util
import json
import pathlib
import stat
import sys
import threading
import time
import types

import pytest

from kernel_stub import NOTICE_LINE, NOTICE_TITLE

SESSION = "agent:main:telegram:dm:42"


class Ctx:
    def __init__(self, accept=True):
        self.accept, self.injected, self.tools, self.hooks = accept, [], {}, []

    def inject_message(self, content, role="user", *, session_key=None, origin=None):
        self.injected.append((content, session_key))
        return self.accept

    def register_tool(self, name, toolset, description, handler, schema):
        self.tools[name] = handler


def hermes_host(monkeypatch, gateway):
    """Stand-in hermes_cli.plugins: has_gateway_message_injector is what Hermes' GatewayRunner publishes once it runs
    (never in the dashboard or a CLI client). Returns the manager so a test can publish or clear it later."""
    mgr = types.SimpleNamespace(has_gateway_message_injector=gateway)
    plugins = types.ModuleType("hermes_cli.plugins")
    plugins.get_plugin_manager = lambda: mgr
    monkeypatch.setitem(sys.modules, "hermes_cli", types.ModuleType("hermes_cli"))
    monkeypatch.setitem(sys.modules, "hermes_cli.plugins", plugins)
    return mgr


def ready_loop(wake, monkeypatch):
    """Everything readiness asks for is in place; only the process kind differs between tests."""
    monkeypatch.setenv("APA_WAKE_SESSION_KEY", SESSION)
    monkeypatch.setattr(wake.readiness, "_injection_allowed", lambda key: True)
    monkeypatch.setattr(wake, "LONG_POLL_SECS", 0)
    monkeypatch.setattr(wake, "BACKOFF_SECS", 0.05)
    monkeypatch.setattr(wake, "GATEWAY_POLL_SECS", 0.05)
    wake.stop.clear()


def wait_for(cond, secs=5):
    deadline = time.time() + secs
    while time.time() < deadline and not cond():
        time.sleep(0.05)
    return cond()


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
    assert text.startswith("[APA wake] 后台任务要接着处理「写周报」\n")
    assert _capture_blocks().sub("", text) == ""  # apa-capture drops the whole block
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


def test_loop_does_not_start_while_anything_is_missing(wake, kernel, monkeypatch):
    kernel.wake()
    monkeypatch.setenv("APA_WAKE_SESSION_KEY", SESSION)  # Hermes has not allowed injection (no config here)
    ctx = Ctx()
    wake.register(ctx)
    time.sleep(0.3)
    assert ctx.injected == [] and len(kernel.deliveries) == 1 and "apa_wake_cli" in ctx.tools


def test_registered_loop_claims_in_background_and_survives_errors(wake, kernel, monkeypatch):
    hermes_host(monkeypatch, gateway=True)
    ready_loop(wake, monkeypatch)
    kernel.wake(run="")      # first delivery fails -> loop must keep going
    kernel.wake(did="wake_r2", run="r2")
    ctx = Ctx()
    wake.register(ctx)
    wait_for(lambda: ctx.injected)
    wake.stop.set()
    assert ctx.injected and ctx.injected[0][1] == SESSION and "r2" in ctx.injected[0][0]
    assert [r[0] for r in kernel.reports][:2] == ["fail", "ack"]


@pytest.mark.parametrize("host", ["no_gateway_injector", "no_hermes_runtime"])
def test_non_gateway_process_never_claims(wake, kernel, monkeypatch, host):
    """Dashboard / `hermes chat` / --oneshot load the plugin with the same env and config but host no gateway: they must
    never claim, or they take deliveries only the gateway can inject and fail them into kernel backoff."""
    if host == "no_gateway_injector":
        hermes_host(monkeypatch, gateway=False)
    else:
        monkeypatch.setitem(sys.modules, "hermes_cli", None)  # import fails
    ready_loop(wake, monkeypatch)
    kernel.notice(did="brief:it9", cls="brief")
    kernel.wake()
    ctx = Ctx()
    wake.register(ctx)
    time.sleep(0.5)
    wake.stop.set()
    assert kernel.claims == 0 and kernel.reports == [] and len(kernel.deliveries) == 2
    assert ctx.injected == [] and "apa_wake_cli" in ctx.tools


def test_loop_claims_only_while_the_gateway_is_live(wake, kernel, monkeypatch):
    """Hermes loads plugins before GatewayRunner publishes its injector: nothing is claimed until then, and nothing
    after the gateway clears it on shutdown."""
    mgr = hermes_host(monkeypatch, gateway=False)
    ready_loop(wake, monkeypatch)
    kernel.notice(did="brief:it9", cls="brief")
    ctx = Ctx()
    wake.register(ctx)
    time.sleep(0.3)
    assert kernel.claims == 0 and len(kernel.deliveries) == 1
    mgr.has_gateway_message_injector = True  # gateway is running
    assert wait_for(lambda: ctx.injected)
    assert kernel.reports == [("ack", "brief:it9", {"version": 1, "external_ref": "main-session"})]
    mgr.has_gateway_message_injector = False  # gateway stopping
    time.sleep(0.3)  # let an in-flight poll return
    seen = kernel.claims
    kernel.wake()
    time.sleep(0.3)
    wake.stop.set()
    assert kernel.claims == seen and len(kernel.deliveries) == 1 and len(ctx.injected) == 1


def test_cli_rejects_malformed_args(wake):
    ctx = Ctx()
    wake.register(ctx)
    assert "list of strings" in json.loads(ctx.tools["apa_wake_cli"]({"args": "context"}))["error"]


def test_notice_is_injected_as_facts_without_credential(wake, kernel):
    """A proactive reminder (no run) reaches the main session as one short message; no run credential is exchanged."""
    kernel.notice()
    ctx = Ctx()
    wake.poll_once(ctx, SESSION, wait=0)
    (text, key), = ctx.injected
    assert key == SESSION
    person, agent = text.split("\n———\n")
    assert person.splitlines()[:2] == [f"[APA 提醒] {NOTICE_TITLE}", NOTICE_LINE]
    assert "it1" not in person.replace(NOTICE_LINE, "") and agent.endswith("不必回复 APA。")
    assert kernel.exchanges == 0 and "apa_wake_cli" not in text and "apa_run_" not in text
    assert kernel.reports == [("ack", "schedbatch:1", {"version": 1, "external_ref": "main-session"})]


def test_brief_notice_names_its_kind(wake, kernel):
    kernel.notice(did="brief:it9", cls="brief", title="简报 2026-11-24 08:00", summary="## 今天\n- 写周报")
    ctx = Ctx()
    wake.poll_once(ctx, SESSION, wait=0)
    assert ctx.injected[0][0].startswith("[APA 简报] 简报 2026-11-24 08:00\n## 今天\n- 写周报\n")


def test_refused_notice_is_failed_back_for_retry(wake, kernel):
    kernel.notice()
    ctx = Ctx(accept=False)
    with pytest.raises(wake.WakeError, match="refused"):
        wake.poll_once(ctx, SESSION, wait=0)
    assert [r[0] for r in kernel.reports] == ["fail"] and kernel.exchanges == 0


def test_notice_with_lost_ack_is_not_injected_twice(wake, kernel):
    """The kernel re-offers a delivery whose ack never arrived; this process re-acks it instead of injecting again."""
    kernel.fail_ack = True
    kernel.notice()
    ctx = Ctx()
    with pytest.raises(wake.WakeError, match="500"):
        wake.poll_once(ctx, SESSION, wait=0)
    kernel.fail_ack = False
    kernel.notice()  # the same delivery comes back after its lease
    wake.poll_once(ctx, SESSION, wait=0)
    assert len(ctx.injected) == 1
    assert [r[0] for r in kernel.reports] == ["ack", "ack"]


def _capture_blocks():
    spec = importlib.util.spec_from_file_location("apa_capture_blocks", pathlib.Path(__file__).resolve().parents[2] / "capture" / "__init__.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.APA_BLOCKS


def test_decision_notice_carries_options_recommendation_and_link(wake):
    text = wake.notice({"class": "decision", "item": "itm_1", "content": {
        "title": "PR 评审改完再合，怎么处理？", "summary": "有 5 个阻断项", "options": ["fix: 让作者改（小改）", "hold: 整条暂缓"],
        "recommend": "fix: 改动最小", "web_url": "https://apa.example/items/itm_1"}})
    person, agent = text.split("\n———\n")
    assert person.splitlines() == ["[APA 提醒] 要你定：PR 评审改完再合，怎么处理？", "有 5 个阻断项", "- 1 让作者改（小改）（推荐）",
                                   "- 2 整条暂缓", "推荐理由：改动最小", "要你做：选一个，或说别的想法",
                                   "详情 → https://apa.example/items/itm_1"]
    assert "事项 itm_1" in agent and "1=fix、2=hold" in agent
    assert _capture_blocks().sub("", text) == ""
