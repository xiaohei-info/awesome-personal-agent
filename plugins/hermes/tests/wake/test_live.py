"""Live gate: real Hermes (plugin loader, PluginContext.inject_message with its allow_gateway_injection check, tool
registry) and the real `apa` binary built from this repo. Stand-ins: the kernel HTTP endpoint (kernel_stub) and the
gateway's injector callback (a live gateway needs chat platforms; its contract is PluginManager.set_gateway_message_injector).

Measured (§21.3): the wake turn's tool call reaches `apa` with APA_RUN + the exchanged credential, and Hermes refuses
the injection unless the owner set allow_gateway_injection. Skips without Hermes/Go; APA_REQUIRE_LIVE=1 fails instead.
"""
import json
import os
import pathlib
import shutil
import subprocess
import textwrap

import pytest

from kernel_stub import PLUGIN_TOKEN, Kernel

ROOT = pathlib.Path(__file__).resolve().parents[4]
WAKE = ROOT / "plugins" / "hermes" / "wake"
HERMES_SRC = pathlib.Path(os.environ.get("HERMES_SRC", pathlib.Path.home() / ".hermes" / "hermes-agent"))
HERMES_PY = HERMES_SRC / "venv" / "bin" / "python"
REQUIRE_LIVE = os.environ.get("APA_REQUIRE_LIVE") == "1"
NEEDS = "PluginContext.inject_message, PluginManager.set_gateway_message_injector; tested with Hermes v0.22.5"
PROBE = ("from hermes_cli import plugins as p; assert hasattr(p.PluginContext, 'inject_message') "
         "and hasattr(p.PluginManager, 'set_gateway_message_injector')")


def _live_problem():
    if not HERMES_PY.exists() or not shutil.which("go"):
        return f"needs a Hermes install (HERMES_SRC={HERMES_SRC}) and Go"
    r = subprocess.run([str(HERMES_PY), "-c", PROBE], cwd=HERMES_SRC, capture_output=True, timeout=120)
    return None if r.returncode == 0 else f"this Hermes lacks the plugin APIs; need {NEEDS}"


_PROBLEM = _live_problem()
if _PROBLEM and REQUIRE_LIVE:
    pytest.fail("live gate required (APA_REQUIRE_LIVE=1) but cannot run: " + _PROBLEM, pytrace=False)
pytestmark = pytest.mark.skipif(bool(_PROBLEM), reason=_PROBLEM or "")

CHILD = textwrap.dedent("""
    import json, os, time
    from hermes_cli import plugins
    mgr = plugins.get_plugin_manager()
    seen = []
    mgr.set_gateway_message_injector(object(), lambda **kw: seen.append(kw) or True)
    plugins.discover_plugins()
    deadline = time.time() + float(os.environ['APA_TEST_WAIT'])
    while time.time() < deadline and not seen:
        time.sleep(0.1)
    from tools.registry import registry
    out = None
    if seen:
        out = registry.dispatch("apa_wake_cli", {"args": ["events"], "run_id": "r1"})
    print(json.dumps(default=str, obj={"seen": seen, "tool": out, "registered": "apa_wake_cli" in registry.get_all_tool_names()}))
""")


@pytest.fixture(scope="module")
def apa_bin(tmp_path_factory):
    out = tmp_path_factory.mktemp("bin") / "apa"
    subprocess.run(["go", "build", "-o", str(out), "./cli/apa"], cwd=ROOT, check=True)
    return str(out)


@pytest.fixture
def run_hermes(tmp_path, apa_bin):
    k = Kernel()
    k.wake()

    def run(allow, session_key="agent:main:telegram:dm:42", wait="20"):
        home = tmp_path / f"home_{allow}"
        (home / "plugins").mkdir(parents=True)
        (home / "plugins" / "apa-wake").symlink_to(WAKE)
        (home / "config.yaml").write_text("plugins:\n  enabled:\n    - apa-wake\n" + (
            "  entries:\n    apa-wake:\n      allow_gateway_injection: true\n" if allow else ""))
        env = {**os.environ, "HERMES_HOME": str(home), "APA_BIN": apa_bin, "APA_URL": k.url, "APA_TOKEN": PLUGIN_TOKEN,
               "APA_HOME": str(tmp_path / "apahome"), "APA_TEST_WAIT": wait}
        env.pop("APA_RUN", None)
        if session_key:
            env["APA_WAKE_SESSION_KEY"] = session_key
        p = subprocess.run([str(HERMES_PY), "-c", CHILD], cwd=HERMES_SRC, env=env, capture_output=True, text=True,
                           timeout=120)
        assert p.returncode == 0, p.stderr
        return json.loads(p.stdout.strip().splitlines()[-1])

    yield k, run
    k.stop()


def test_wake_opens_hermes_turn_and_cli_runs_as_the_run(run_hermes):
    k, run = run_hermes
    out = run(allow=True)
    (call,) = out["seen"]
    assert call["session_key"] == "agent:main:telegram:dm:42" and "r1" in call["content"]
    assert "apa_run_" not in call["content"]
    assert [r[0] for r in k.reports] == ["ack"]
    # The real apa ran as the run: APA_RUN made it use the exchanged credential (not the plugin token or a local file).
    assert json.loads(out["tool"])["ok"] is True, out["tool"]
    assert [b for _, b in k.cli_calls] == ["Bearer apa_run_r1_1"] and k.cli_calls[0][0].startswith("/v1/events")


def test_hermes_refuses_injection_without_owner_permission(run_hermes):
    k, run = run_hermes
    out = run(allow=False)
    assert out["seen"] == [] and out["registered"] is True
    assert k.reports and k.reports[0][0] == "fail" and "refused" in k.reports[0][2]["reason"]


def test_default_stays_headless_without_session_key(run_hermes):
    k, run = run_hermes
    out = run(allow=True, session_key=None, wait="2")  # allowed by Hermes, but never declared: no poller starts
    assert out["seen"] == [] and out["registered"] is True
    assert len(k.deliveries) == 1 and k.reports == []
