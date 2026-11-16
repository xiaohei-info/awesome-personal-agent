import json
import shutil
import stat
import subprocess

import pytest

from helpers import HEADLESS, ROOT, kernel_argv, load_headless


@pytest.mark.skipif(not shutil.which("go"), reason="needs Go")
def test_declaration_is_accepted_by_the_kernel_parser():
    r = subprocess.run(["go", "test", "./plugins/hermes/headless/"], cwd=ROOT, capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr


def test_kernel_argv_per_tier_and_resume():
    decl = json.loads((HEADLESS / "declaration.json").read_text())
    read = kernel_argv(decl, "do it", "read")
    assert read[:5] == ["hermes", "chat", "--oneshot", "-q", "do it"] and "--format" in read
    assert "terminal" not in read[read.index("-t") + 1] and "--yolo" not in read
    assert "terminal" in kernel_argv(decl, "p", "edit")[-1] and "--yolo" not in kernel_argv(decl, "p", "edit")
    assert "--yolo" in kernel_argv(decl, "p", "full")
    assert kernel_argv(decl, "p", "edit", "sess-1")[-2:] == ["--resume", "sess-1"]


@pytest.fixture
def fake_apa(tmp_path, monkeypatch):
    out = tmp_path / "stdin.json"
    exe = tmp_path / "apa"
    exe.write_text(f"#!/bin/sh\necho \"$@\" > {tmp_path}/argv\necho \"$APA_HARNESS\" > {tmp_path}/harness\ncat > {out}\necho \"$FAKE_STDERR\" >&2\nexit ${{FAKE_EXIT:-0}}\n")
    exe.chmod(exe.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("APA_BIN", str(exe))
    return tmp_path


def test_declare_submits_headless_part_via_apa_declare_stdin(fake_apa):
    out = json.loads(load_headless()._declare({}, session_id="s1"))
    assert out["ok"] and (fake_apa / "argv").read_text().strip() == "declare -"
    assert (fake_apa / "harness").read_text().strip() == "hermes"  # X-APA-Harness source for the kernel audit
    body = json.loads((fake_apa / "stdin.json").read_text())
    assert body["harness"] == "hermes" and "executors" not in body
    assert body["headless"]["session"] == {"extract": "$.session_id"}


def test_declare_offers_wake_only_when_apa_wake_is_on(fake_apa, monkeypatch):
    monkeypatch.delenv("APA_WAKE_SESSION_KEY", raising=False)
    load_headless()._declare({})
    assert json.loads((fake_apa / "stdin.json").read_text())["main_session"] == {"wake": False}
    monkeypatch.setenv("APA_WAKE_SESSION_KEY", "agent:main")
    load_headless()._declare({})
    assert json.loads((fake_apa / "stdin.json").read_text())["main_session"] == {"wake": True}


def test_declare_passes_the_kernel_verdict_through(fake_apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "2")
    monkeypatch.setenv("FAKE_STDERR", "bad declaration")
    out = json.loads(load_headless()._declare({}))
    assert out["ok"] is False and out["exit_code"] == 2 and out["stderr"] == "bad declaration"


def test_declare_reports_a_missing_apa_binary(monkeypatch, tmp_path):
    monkeypatch.setenv("APA_BIN", str(tmp_path / "nope"))
    out = json.loads(load_headless()._declare({}))
    assert out["ok"] is False and "cannot run apa" in out["error"]


def test_register_exposes_one_tool_in_the_apa_toolset():
    seen = []

    class Ctx:
        def register_tool(self, **kw):
            seen.append(kw)

    load_headless().register(Ctx())
    assert [(k["name"], k["toolset"]) for k in seen] == [("apa_declare_headless", "apa")]


# ACT-HCTX-14：自检命令只读、输出附录 A.1 的 JSON，并报告本插件上报会话类型。
def test_selfcheck_reports_session_kind_as_json():
    import json
    import subprocess
    import sys
    out = subprocess.run([sys.executable, str(HEADLESS / "selfcheck.py")], capture_output=True, text=True, timeout=5, check=True).stdout
    r = json.loads(out)
    assert r["reports_session_kind"] is True and r["overlaps"] == [] and r["plugin_version"]
    assert {"name": "tools", "installed": True} in r["hooks"]
