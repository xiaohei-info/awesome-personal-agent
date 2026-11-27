import json
import shutil
import stat
import subprocess

import pytest

from headless_helpers import HEADLESS, ROOT, kernel_argv, load_headless


@pytest.mark.skipif(not shutil.which("go"), reason="needs Go")
def test_declaration_is_accepted_by_the_kernel_parser():
    r = subprocess.run(["go", "test", "./plugins/hermes/headless/"], cwd=ROOT, capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr


def test_kernel_argv_per_tier_and_resume():
    decl = json.loads((HEADLESS / "declaration.json").read_text())
    read = kernel_argv(decl, "do it", "read")
    assert read[:7] == ["hermes", "chat", "--oneshot", "--reasoning", "medium", "-q", "do it"] and "--format" in read
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


def test_declaration_follows_apa_wake_readiness(fake_apa, monkeypatch):
    h = load_headless()
    monkeypatch.setattr(h.readiness, "current", lambda: ["injection_denied"])
    h._declare({})
    ms = json.loads((fake_apa / "stdin.json").read_text())["main_session"]
    assert "wake" not in ms and [g["zh"] for g in ms["wake_missing"]] == ["Hermes 还没允许插件在对话里开始一轮"]
    assert "allow_gateway_injection" in ms["wake_missing"][0]["how"]["zh"]
    monkeypatch.setattr(h.readiness, "current", lambda: [])
    h._declare({})
    assert json.loads((fake_apa / "stdin.json").read_text())["main_session"] == {"wake": True}


def test_start_up_sync_submits_only_inside_a_live_gateway(fake_apa, monkeypatch):
    h = load_headless()
    monkeypatch.setattr(h.readiness, "current", lambda: [])
    monkeypatch.setattr(h, "_in_gateway", lambda: False)
    assert h._sync(sleep=lambda s: None) is None and not (fake_apa / "stdin.json").exists()
    seen = iter([False, True])
    monkeypatch.setattr(h, "_in_gateway", lambda: next(seen))
    assert h._sync(sleep=lambda s: None)["ok"] and (fake_apa / "argv").read_text().strip() == "declare -"


def test_start_up_sync_retries_only_while_the_kernel_is_unreachable(fake_apa, monkeypatch):
    h = load_headless()
    monkeypatch.setattr(h.readiness, "current", lambda: [])
    monkeypatch.setattr(h, "_in_gateway", lambda: True)
    waits = []
    monkeypatch.setenv("FAKE_EXIT", "7")
    assert h._sync(sleep=waits.append)["exit_code"] == 7 and waits == [0, *h.RETRY_SECS]
    waits.clear()
    monkeypatch.setenv("FAKE_EXIT", "4")
    assert h._sync(sleep=waits.append)["exit_code"] == 4 and waits == [0]


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

        def register_hook(self, name, cb):
            pass

    h = load_headless()
    h._sync = lambda: None  # the start-up thread is covered above
    h.register(Ctx())
    assert [(k["name"], k["toolset"]) for k in seen] == [("apa_declare_headless", "apa")]


# ACT-HCTX-14：自检命令只读、输出附录 A.1 的 JSON；会话类型已取消，不再报告。
def test_selfcheck_reports_json():
    import json
    import subprocess
    import sys
    out = subprocess.run([sys.executable, str(HEADLESS / "selfcheck.py")], capture_output=True, text=True, timeout=5, check=True).stdout
    r = json.loads(out)
    assert "reports_session_kind" not in r and r["overlaps"] == [] and r["plugin_version"]
    assert {"name": "tools", "installed": True} in r["hooks"]


def test_run_usage_is_this_runs_own_calls(tmp_path, monkeypatch):
    h = load_headless()
    context, bound = tmp_path / "context", {}
    monkeypatch.setattr(h.run_context, "_client_env",
                        lambda: type("Env", (), {"bound_client_env": staticmethod(lambda: bound)}))
    hooks = {}
    monkeypatch.setattr(h, "_sync", lambda: None)
    h.register(type("Ctx", (), {"register_tool": lambda *a, **k: None,
                                "register_hook": lambda self, name, cb: hooks.setdefault(name, cb)})())
    call = lambda: hooks["post_api_request"](usage={"prompt_tokens": 100, "output_tokens": 7, "cache_read_tokens": 60,
                                                    "cache_write_tokens": 10, "input_tokens": 30}, model="m", session_id="s")
    call()  # not an APA run: nothing written
    assert not list(tmp_path.rglob("usage.json"))
    bound[h.run_context.VAR] = str(context)
    # A resume keeps the session and the context path; the kernel rewrites the file with the new run.
    for run, calls in (("run_a", 2), ("run_b", 1)):
        (tmp_path / run).mkdir()
        context.write_text(json.dumps({"run": run, "run_dir": str(tmp_path / run), "work_dir": str(tmp_path)}))
        for _ in range(calls):
            call()
        hooks["post_api_request"](usage=None)
        declared = json.loads((HEADLESS / "declaration.json").read_text())["headless"]["usage"]
        total = json.loads((tmp_path / run / declared["file"]).read_text())
        assert {k: total[declared[k][2:]] for k in ("input", "output", "cache_read", "cache_write", "model")} == {
            "input": 100 * calls, "output": 7 * calls, "cache_read": 60 * calls, "cache_write": 10 * calls, "model": "m"}
