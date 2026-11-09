"""apa_read_file / apa_list_dir run in the shared gateway process: the directories come from the kernel's run context
for this session, never from the gateway's cwd or environment (ACT-DRF-01)."""
import json
import threading

import pytest

from helpers import fake_gateway, load_tools
from test_tools import Ctx


def make_run(root, run_id, work):
    """What the kernel lays out at launch: a run directory and a context file naming both directories."""
    run_dir = root / "home" / "runs" / run_id
    run_dir.mkdir(parents=True)
    (run_dir / "materials.json").write_text(json.dumps({"target": {"id": run_id}}))
    creds = root / "home" / "run-credentials"
    creds.mkdir(exist_ok=True)
    path = creds / run_id
    path.write_text(json.dumps({"run": run_id, "url": "http://k", "token": "tok", "run_dir": str(run_dir), "work_dir": str(work)}))
    return path


@pytest.fixture
def env(tmp_path, monkeypatch):
    gateway_cwd, checkout = tmp_path / "hermes-src", tmp_path / "checkout"
    gateway_cwd.mkdir()
    checkout.mkdir()
    (gateway_cwd / "README.md").write_text("hermes itself")
    (checkout / "README.md").write_text("hello")
    (checkout / "src").mkdir()
    (tmp_path / "secret.txt").write_text("nope")
    (checkout / "link").symlink_to(tmp_path / "secret.txt")
    monkeypatch.chdir(gateway_cwd)
    monkeypatch.setenv("APA_RUN", "run_other")  # what a gateway started by another run's client keeps for life
    session = fake_gateway(monkeypatch)
    context = make_run(tmp_path, "run1", checkout)
    with session(context, checkout):
        yield tmp_path


@pytest.fixture
def tools():
    ctx = Ctx()
    load_tools().register(ctx)
    return {n: h for n, (_, h) in ctx.tools.items()}


def read(tools, path, **kw):
    return json.loads(tools["apa_read_file"]({"path": path, **kw}))


def test_reads_the_materials_file_in_the_run_directory(tools, env):
    r = read(tools, str(env / "home" / "runs" / "run1" / "materials.json"))
    assert r["ok"] and json.loads(r["content"]) == {"target": {"id": "run1"}}


def test_relative_paths_start_at_the_tasks_working_directory_not_the_gateways(tools, env):
    assert read(tools, "README.md")["content"] == "hello"


def test_the_gateways_own_directory_is_refused(tools, env):
    r = read(tools, str(env / "hermes-src" / "README.md"))
    assert r["ok"] is False and "outside" in r["error"] and "content" not in r


def test_lists_the_working_directory_marking_subdirectories(tools, env):
    r = json.loads(tools["apa_list_dir"]({"path": "."}))
    assert r["ok"] and "README.md" in r["entries"] and "src/" in r["entries"]


@pytest.mark.parametrize("path", ["../secret.txt", "link", "/etc/passwd", "~/x"])
def test_paths_outside_the_two_roots_are_refused(tools, env, path):
    r = read(tools, path)
    assert r["ok"] is False and "outside" in r["error"] and "content" not in r


def test_listing_outside_the_roots_is_refused(tools, env):
    assert json.loads(tools["apa_list_dir"]({"path": ".."}))["ok"] is False


def test_sibling_run_directory_is_not_readable_even_if_the_gateway_env_names_it(tools, env, monkeypatch):
    make_run(env, "run2", env / "checkout")
    monkeypatch.setenv("APA_RUN", "run2")
    assert read(tools, str(env / "home" / "runs" / "run2" / "materials.json"))["ok"] is False


def test_large_files_are_truncated_and_flagged(tools, env):
    (env / "checkout" / "big.txt").write_text("a" * 300_000)
    r = read(tools, "big.txt")
    assert r["ok"] and r["truncated"] is True and len(r["content"]) == 200_000


def test_missing_file_reports_the_real_error(tools, env):
    r = read(tools, "nope.md")
    assert r["ok"] is False and "nope.md" in r["error"]


def test_no_write_tool_exists(tools):
    assert not [n for n in tools if "write" in n]


def test_ended_run_is_refused_with_the_reason(tools, env):
    (env / "home" / "run-credentials" / "run1").unlink()
    r = read(tools, "README.md")
    assert r["ok"] is False and "ended" in r["error"]
    assert json.loads(tools["apa_list_dir"]({}))["ok"] is False


def test_unreadable_context_says_so_without_echoing_the_credential(tools, env):
    (env / "home" / "run-credentials" / "run1").write_text('{"token": "apa_run_secret"')
    r = read(tools, "README.md")
    assert r["ok"] is False and "cannot read" in r["error"] and "apa_run_secret" not in r["error"]


def test_context_without_directories_is_refused(tools, env):
    (env / "home" / "run-credentials" / "run1").write_text('{"run": "run1", "work_dir": "/"}')
    r = read(tools, "/etc/passwd")
    assert r["ok"] is False and "no directories" in r["error"]


def test_session_without_a_run_is_refused_and_never_falls_back_to_the_process_cwd(tools, tmp_path, monkeypatch):
    (tmp_path / "README.md").write_text("gateway file")
    monkeypatch.chdir(tmp_path)
    session = fake_gateway(monkeypatch)
    with session():
        r = read(tools, "README.md")
    assert r["ok"] is False and "client_env_passthrough" in r["error"] and "content" not in r


def test_hermes_without_session_scope_is_refused_with_the_reason(tools, tmp_path, monkeypatch):
    (tmp_path / "README.md").write_text("process cwd")
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("APA_RUN", "run1")
    r = read(tools, "README.md")
    assert r["ok"] is False and "v0.23.0" in r["error"]


def test_parallel_runs_only_see_their_own_directories(tools, tmp_path, monkeypatch):
    """Two sessions in flight at once in one gateway, each tool call on its own thread (as Hermes' executor runs them)."""
    session = fake_gateway(monkeypatch)
    runs = {}
    for name in ("a", "b"):
        work = tmp_path / f"checkout-{name}"
        work.mkdir()
        (work / "README.md").write_text(f"repo {name}")
        runs[name] = {"context": make_run(tmp_path, f"run_{name}", work), "work": work}
    barrier, out, failed = threading.Barrier(2), {}, []

    def turn(me, other):
        try:
            with session(runs[me]["context"], runs[me]["work"]):
                for _ in range(20):
                    barrier.wait(timeout=10)
                    mine = read(tools, "README.md")
                    theirs_repo = read(tools, str(runs[other]["work"] / "README.md"))
                    theirs_run = read(tools, str(tmp_path / "home" / "runs" / f"run_{other}" / "materials.json"))
                    own_run = read(tools, str(tmp_path / "home" / "runs" / f"run_{me}" / "materials.json"))
                    out.setdefault(me, []).append((mine.get("content"), theirs_repo["ok"], theirs_run["ok"], own_run["ok"]))
        except Exception as e:  # surfaced below; a thread's assert would vanish
            failed.append(e)

    threads = [threading.Thread(target=turn, args=p) for p in (("a", "b"), ("b", "a"))]
    for t in threads:
        t.start()
    for t in threads:
        t.join(30)
    assert not failed
    assert out["a"] == [("repo a", False, False, True)] * 20
    assert out["b"] == [("repo b", False, False, True)] * 20
