"""apa_read_file / apa_list_dir: read-only, confined to the run directory and the working directory."""
import json
import os

import pytest

from helpers import load_tools
from test_tools import Ctx


@pytest.fixture
def env(tmp_path, monkeypatch):
    home, cwd = tmp_path / "home", tmp_path / "checkout"
    run = home / "runs" / "run1"
    run.mkdir(parents=True)
    cwd.mkdir()
    (run / "materials.json").write_text('{"target": {"id": "i1"}}')
    (cwd / "README.md").write_text("hello")
    (cwd / "src").mkdir()
    (tmp_path / "secret.txt").write_text("nope")
    (cwd / "link").symlink_to(tmp_path / "secret.txt")
    monkeypatch.chdir(cwd)
    monkeypatch.setenv("APA_HOME", str(home))
    monkeypatch.setenv("APA_RUN", "run1")
    return tmp_path


@pytest.fixture
def tools():
    ctx = Ctx()
    load_tools().register(ctx)
    return {n: h for n, (_, h) in ctx.tools.items()}


def read(tools, path, **kw):
    return json.loads(tools["apa_read_file"]({"path": path, **kw}))


def test_reads_the_materials_file_in_the_run_directory(tools, env):
    r = read(tools, str(env / "home" / "runs" / "run1" / "materials.json"))
    assert r["ok"] and json.loads(r["content"]) == {"target": {"id": "i1"}}


def test_reads_repository_files_relative_to_the_working_directory(tools, env):
    assert read(tools, "README.md")["content"] == "hello"


def test_lists_directories_marking_subdirectories(tools, env):
    r = json.loads(tools["apa_list_dir"]({"path": "."}))
    assert r["ok"] and "README.md" in r["entries"] and "src/" in r["entries"]


@pytest.mark.parametrize("path", ["../secret.txt", "link", "/etc/passwd"])
def test_paths_outside_the_two_roots_are_refused(tools, env, path):
    r = read(tools, path)
    assert r["ok"] is False and "outside" in r["error"] and "content" not in r


def test_listing_outside_the_roots_is_refused(tools, env):
    assert json.loads(tools["apa_list_dir"]({"path": ".."}))["ok"] is False


def test_sibling_run_directory_is_not_readable(tools, env):
    other = env / "home" / "runs" / "run2"
    other.mkdir()
    (other / "materials.json").write_text("x")
    assert read(tools, str(other / "materials.json"))["ok"] is False


def test_large_files_are_truncated_and_flagged(tools, env):
    (env / "checkout" / "big.txt").write_text("a" * 300_000)
    r = read(tools, "big.txt")
    assert r["ok"] and r["truncated"] is True and len(r["content"]) == 200_000


def test_missing_file_reports_the_real_error(tools, env):
    r = read(tools, "nope.md")
    assert r["ok"] is False and "nope.md" in r["error"]


def test_no_write_tool_exists(tools):
    assert not [n for n in tools if "write" in n]
