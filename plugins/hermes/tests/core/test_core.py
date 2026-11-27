"""Unit tests: the plugin against a stand-in `apa` executable (test boundary only)."""
import os
import stat
import time

import pytest

from conftest import load_core


class Ctx:
    def __init__(self):
        self.sections = {}

    def register_system_prompt_section(self, id, content, **kw):
        assert id not in self.sections
        self.sections[id] = (content, kw)


def fake_apa(tmp_path, body):
    p = tmp_path / "apa"
    p.write_text("#!/bin/sh\n" + body + "\n")
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    return str(p)


@pytest.fixture
def core():
    return load_core()


def sections(core):
    ctx = Ctx()
    core.register(ctx)
    return ctx.sections


def test_register_adds_the_instruction_section(core):
    s = sections(core)
    assert set(s) == {"apa.instructions"}
    assert callable(s["apa.instructions"][0])


def test_section_renders_apa_stdout(core, tmp_path, monkeypatch):
    monkeypatch.setattr(core.apa, "TIMEOUT_SECS", 20)  # timeout itself is tested below; avoid load flakes
    monkeypatch.setenv("APA_BIN", fake_apa(
        tmp_path, 'echo "GUIDE $APA_SESSION $APA_HARNESS"'))
    s = sections(core)
    assert s["apa.instructions"][0]({"session_id": "s1"}) == "GUIDE s1 hermes"


def test_nonzero_exit_yields_no_section(core, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_BIN", fake_apa(tmp_path, "echo partial; exit 7"))
    s = sections(core)
    assert s["apa.instructions"][0]({}) == ""


def test_missing_binary_yields_no_section(core, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_BIN", str(tmp_path / "nope"))
    assert sections(core)["apa.instructions"][0]({}) == ""


def test_slow_apa_is_abandoned_after_two_seconds(core, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_BIN", fake_apa(tmp_path, "sleep 10; echo late"))
    start = time.monotonic()
    assert sections(core)["apa.instructions"][0]({}) == ""
    assert time.monotonic() - start < 4


def test_empty_output_means_no_section_content(core, tmp_path, monkeypatch):
    monkeypatch.setenv("APA_BIN", fake_apa(tmp_path, "exit 0"))
    assert sections(core)["apa.instructions"][0]({}) == ""


# ACT-HCTX-09：钩子发出的 apa 调用带 APA_SESSION、APA_HARNESS；会话类型不参与判定（§14.3），插件不从会话绑定推算它。
def test_hook_calls_carry_session_identity(core, tmp_path, monkeypatch):
    import sys
    import types
    monkeypatch.setenv("APA_BIN", fake_apa(tmp_path, 'echo "$APA_SESSION $APA_HARNESS [$APA_SESSION_KIND]"'))
    monkeypatch.delenv("APA_SESSION_KIND", raising=False)
    bound = {"HERMES_SESSION_PLATFORM": "telegram", "HERMES_SESSION_CHAT_TYPE": "group", "HERMES_SESSION_USER_ID": "7"}
    mod = types.ModuleType("gateway.session_context")
    mod.get_session_env = lambda name, default="": bound.get(name, default)
    monkeypatch.setitem(sys.modules, "gateway", types.ModuleType("gateway"))
    monkeypatch.setitem(sys.modules, "gateway.session_context", mod)
    render = sections(core)["apa.instructions"][0]
    assert render({"session_id": "s1"}) == "s1 hermes []"


