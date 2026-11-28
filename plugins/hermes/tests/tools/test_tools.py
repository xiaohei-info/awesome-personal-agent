"""Unit tests: the plugin against a stand-in `apa` executable (test boundary only)."""
import json
import stat

import pytest

from helpers import fake_session_context, load_tools


class Ctx:
    def __init__(self):
        self.tools = {}

    def register_tool(self, name, toolset, schema, handler, **kw):
        assert name not in self.tools and toolset == "apa" and schema["name"] == name
        self.tools[name] = (schema, handler)


# Records argv / APA_SESSION / stdin as one JSON line, then answers with the scripted stdout/stderr/exit code.
FAKE = """#!/bin/sh
python3 - "$@" <<'PY' >> "$APA_LOG"
import json, os, sys
print(json.dumps({"argv": sys.argv[1:], "session": os.environ.get("APA_SESSION")}))
open(os.environ["APA_LOG"] + ".harness", "a").write(os.environ.get("APA_HARNESS", "") + "\\n")
open(os.environ["APA_LOG"] + ".kind", "a").write(os.environ.get("APA_SESSION_KIND", "-") + "\\n")
PY
cat > "$APA_STDIN" 2>/dev/null || true
echo "$FAKE_OUT"
echo "$FAKE_ERR" >&2
exit ${FAKE_EXIT:-0}
"""


@pytest.fixture
def apa(tmp_path, monkeypatch):
    p = tmp_path / "apa"
    p.write_text(FAKE)
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    for k, v in {"APA_BIN": str(p), "APA_LOG": str(tmp_path / "log"), "APA_STDIN": str(tmp_path / "stdin")}.items():
        monkeypatch.setenv(k, v)
    return tmp_path


def calls(tmp):
    return [json.loads(l) for l in (tmp / "log").read_text().splitlines()]


@pytest.fixture
def tools():
    ctx = Ctx()
    load_tools().register(ctx)
    return {n: h for n, (_, h) in ctx.tools.items()}


def test_registers_the_apa_tools(tools):
    assert set(tools) == {"apa_read_file", "apa_list_dir", "apa_item_update", "apa_material_commit", "apa_propose", "apa_ask", "apa_decide", "apa_accept", "apa_dismiss", "apa_executors", "apa_delegate",
                          "apa_report", "apa_dispatch", "apa_item_history", "apa_attach", "apa_proposals_apply", "apa_item_comment",
                          "apa_item_list", "apa_item_show", "apa_item_thread", "apa_project_list", "apa_project_show", "apa_source_event", "apa_search", "apa_source_propose", "apa_source_ignore", "apa_source_attach", "apa_source_remind"}


def test_query_tools_map_to_read_commands(tools, apa):
    tools["apa_item_list"]({"q": "passport", "phase": "open", "planned_for": "2026-05-01"}, session_id="s")
    tools["apa_source_event"]({"id": "sev_1", "part": "body"}, session_id="s")
    tools["apa_search"]({"query": "北辰 加账号", "include_dropped": True}, session_id="s")
    argv = [c["argv"] for c in calls(apa)]
    assert argv[0][:2] == ["item", "list"] and "--planned-for" in argv[0]
    assert argv[1] == ["source", "event", "sev_1", "--part", "body"]
    assert argv[2] == ["search", "北辰 加账号", "--include-dropped"]


def test_cli_calls_carry_harness_hermes(tools, apa):
    tools["apa_decide"]({"id": "i1", "option": "B"}, session_id="s")
    assert (apa / "log.harness").read_text().split() == ["hermes"]


# 本人原话不再是依据（§14.3）：模型塞进来的 evidence、grant 不会出现在命令行上。
def test_decide_carries_hermes_session_and_never_evidence(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_OUT", '{"state":"done"}')
    r = json.loads(tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "就选方案 B", "grant": "g1"}, session_id="sess-9"))
    assert calls(apa) == [{"argv": ["decide", "i1", "--option", "B"], "session": "sess-9"}]
    assert r == {"exit_code": 0, "ok": True, "stdout": {"state": "done"}, "stderr": None}


def test_web_only_refusal_is_returned_untouched_and_not_retried(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "20")
    monkeypatch.setenv("FAKE_ERR", '{"code":"web_only","why":"web_only","next":"https://x/web/i1"}')
    r = json.loads(tools["apa_decide"]({"id": "i1", "option": "B"}, session_id="s"))
    assert r["exit_code"] == 20 and not r["ok"] and r["stderr"]["next"] == "https://x/web/i1"
    assert len(calls(apa)) == 1


@pytest.mark.parametrize("tool,args,argv", [
    ("apa_propose", {"kind": "task", "title": "T", "tags": "a,b"}, ["propose", "--kind", "task", "--title", "T", "--tags", "a,b"]),
    ("apa_ask", {"id": "i", "question": "Q?", "option": ["A=yes", "B=no"], "next": ["A=open"], "free_text": True,
                 "default_on_timeout": "A"},
     ["ask", "i", "--question", "Q?", "--option", "A=yes", "--option", "B=no", "--next", "A=open", "--free-text"]),
    ("apa_accept", {"id": "i", "evidence": "ok"}, ["accept", "i"]),
    ("apa_dismiss", {"id": "i", "reason": "dup"}, ["dismiss", "i", "--reason", "dup"]),
    ("apa_executors", {"project": "p1"}, ["executors", "--project", "p1"]),
    ("apa_delegate", {"parent": "a", "assignee": "owner-friend", "idempotency_key": "k"},
     ["delegate", "--assignee", "owner-friend", "--parent", "a", "--idempotency-key", "k"]),
    ("apa_report", {"id": "c", "outcome": "done", "summary": "s", "pr": "u", "ack_cursor": "7"},
     ["report", "c", "--outcome", "done", "--summary", "s", "--pr", "u", "--ack-cursor", "7"]),
])
def test_cli_tools_map_args_and_session(tools, apa, tool, args, argv):
    tools[tool](args, session_id="s2")
    assert calls(apa) == [{"argv": argv, "session": "s2"}]


def test_delegate_not_allowed_exit_21_is_exposed(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "21")
    r = json.loads(tools["apa_delegate"]({"parent": "a", "assignee": "nope"}, session_id="s"))
    assert r["exit_code"] == 21 and r["ok"] is False


def test_missing_apa_is_a_real_error_not_fake_success(tools, monkeypatch):
    monkeypatch.setenv("APA_BIN", "/nonexistent/apa")
    r = json.loads(tools["apa_decide"]({"id": "i"}, session_id="s"))
    assert r["ok"] is False and "cannot run apa" in r["error"]


def test_item_update_writes_spec_json_with_session(tools, apa):
    spec = {"period": "2026-W45", "sections": [{"project": "p", "body": "ok"}]}
    tools["apa_item_update"]({"id": "i1", "spec": spec, "reason": "brief done"}, session_id="s3")
    [c] = calls(apa)
    assert c["session"] == "s3" and c["argv"][:2] == ["item", "update"] and c["argv"][2] == "i1"
    assert json.loads(c["argv"][c["argv"].index("--spec") + 1]) == spec
    assert c["argv"][-2:] == ["--reason", "brief done"]


def test_item_update_passes_project_and_schedule(tools, apa):
    tools["apa_item_update"]({"id": "i1", "project": "p1", "deadline": "2026-05-01", "planned_for": "2026-04-30",
                              "planned_slot": "09:00 - 10:00", "reason": "triage"}, session_id="s")
    [c] = calls(apa)
    for flag, value in (("--project", "p1"), ("--deadline", "2026-05-01"), ("--planned-for", "2026-04-30"), ("--planned-slot", "09:00 - 10:00")):
        assert c["argv"][c["argv"].index(flag) + 1] == value


def test_item_update_kernel_refusal_is_exposed(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "4")
    monkeypatch.setenv("FAKE_ERR", '{"code":"forbidden"}')
    r = json.loads(tools["apa_item_update"]({"id": "other", "spec": {}, "reason": "x"}, session_id="s"))
    assert r["exit_code"] == 4 and r["ok"] is False and r["stderr"] == {"code": "forbidden"}


def test_material_commit_passes_body_on_stdin_not_argv(tools, apa):
    body = "# 规范\n- 用 make test; $(rm -rf /) 只是文字\n"
    tools["apa_material_commit"]({"kind": "guideline", "title": "G", "project": "p1", "reason": "draft", "content": body},
                                 session_id="s4")
    assert calls(apa) == [{"argv": ["material", "commit", "--from", "-", "--reason", "draft", "--kind", "guideline",
                                    "--title", "G", "--project", "p1"], "session": "s4"}]
    assert (apa / "stdin").read_text() == body


def test_material_commit_revision_of_existing_id(tools, apa):
    tools["apa_material_commit"]({"id": "m1", "reason": "r", "content": "x"}, session_id="s")
    assert calls(apa)[0]["argv"] == ["material", "commit", "m1", "--from", "-", "--reason", "r"]


@pytest.mark.parametrize("args,why", [({"reason": "r"}, "content"), ({"content": "x"}, "reason"),
                                      ({"reason": "r", "content": "x"}, "kind and title")])
def test_material_commit_rejects_incomplete_calls_without_running_apa(tools, apa, args, why):
    r = json.loads(tools["apa_material_commit"](args, session_id="s"))
    assert r["ok"] is False and why in r["error"] and not (apa / "log").exists()


DECIDE = ["apa_decide", "apa_accept", "apa_dismiss", "apa_attach", "apa_proposals_apply"]


@pytest.fixture
def schemas():
    ctx = Ctx()
    load_tools().register(ctx)
    return {n: s for n, (s, _) in ctx.tools.items()}


# 原话、授权单、会话类型、超时默认都不再是参数：没有任何工具让模型填它们。
def test_no_tool_offers_evidence_grant_session_kind_or_timeout_default(schemas):
    for name, sc in schemas.items():
        assert not {"evidence", "grant", "default_on_timeout"} & set(sc["parameters"]["properties"]), name
    assert "session_kind" not in json.dumps(list(schemas.values()))


# 二档工具的说明告诉模型：只有本人在 Web 放开了拍板才生效，回答本人可见可撤回；23 不重试，去问或给链接。
@pytest.mark.parametrize("tool", DECIDE)
def test_decide_tier_descriptions_explain_exit_23(schemas, tool):
    d = schemas[tool]["description"]
    assert "permission card" in d and "decide for me" in d and "exit 23" in d and "do not retry" in d and "web_url" in d


def test_tier_closed_exit_23_is_exposed_not_retried(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "23")
    monkeypatch.setenv("FAKE_ERR", '{"code":"tier_closed","reason":"tier_closed"}')
    r = json.loads(tools["apa_decide"]({"id": "i", "option": "B"}, session_id="s"))
    assert r["exit_code"] == 23 and r["ok"] is False and r["stderr"]["code"] == "tier_closed" and len(calls(apa)) == 1


def kinds(tmp):
    return (tmp / "log.kind").read_text().split()


# 会话类型不参与判定（§14.3）：插件不再从 Hermes 的会话绑定推算它，也不设 APA_SESSION_KIND。
@pytest.mark.parametrize("bound", [{}, {"platform": "telegram", "chat_type": "dm", "user_id": "42"}, {"platform": "telegram", "chat_type": "group"}])
def test_plugin_does_not_report_a_session_kind(tools, apa, monkeypatch, bound):
    monkeypatch.delenv("APA_SESSION_KIND", raising=False)
    fake_session_context(monkeypatch, **bound)
    tools["apa_accept"]({"id": "i1"}, session_id="s1")
    assert kinds(apa) == ["-"]
    assert calls(apa)[0]["session"] == "s1" and (apa / "log.harness").read_text().split() == ["hermes"]


# ---- 执行者与委派记录（§6.17）：执行者 id 由插件按派法生成；句柄只认 Hermes 自己的形状 ----

def stdin_json(tmp):
    return json.loads((tmp / "stdin").read_text())


def test_delegate_with_mech_names_the_executor_from_the_way_of_delegating(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_OUT", '{"id":"itm_c0ffee","parent":"a"}')
    tools["apa_delegate"]({"parent": "a", "mech": "paseo", "host": "mbp", "profile": "team-quick", "assignee": "ignored"}, session_id="s1")
    [c] = calls(apa)
    assert c["argv"][:5] == ["delegate", "--assignee", "hermes.paseo.mbp.team-quick", "--executor", "-"] and c["argv"][5:] == ["--parent", "a"]
    assert stdin_json(apa) == {"kind": "remote-agent", "host": "mbp", "label": "Paseo · team-quick @ mbp",
                               "handle": {"mech": "paseo", "host": "mbp", "target": "mbp", "profile": "team-quick"}}
    assert (apa / "log.harness").read_text().split() == ["hermes"]


def test_delegate_start_lets_apa_run_a_registered_executor(tools, apa):
    tools["apa_delegate"]({"parent": "a", "assignee": "team-quick", "start": True, "start_model": "m1", "start_thinking": "high"}, session_id="s")
    [c] = calls(apa)
    assert c["argv"][:3] == ["delegate", "--assignee", "team-quick"] and c["argv"][3:8] == ["--start", "--model", "m1", "--thinking", "high"]


def test_dispatch_attach_and_untrack(tools, apa):
    tools["apa_dispatch"]({"action": "attach", "dispatch": "dsp_1", "parent": "itm_p"}, session_id="s")
    tools["apa_dispatch"]({"action": "untrack", "dispatch": "dsp_2"}, session_id="s")
    assert [c["argv"] for c in calls(apa)] == [["dispatch", "attach", "dsp_1", "--parent", "itm_p"], ["dispatch", "untrack", "dsp_2"]]
    assert json.loads(tools["apa_dispatch"]({"action": "attach", "dispatch": "dsp_1"}, session_id="s"))["ok"] is False


def test_same_way_of_delegating_gets_the_same_executor_id(tools, apa):
    for args in ({"mech": "paseo", "host": "user@MBP.local:22", "provider": "codex", "model": "gpt-5.4"},
                 {"mech": "paseo", "host": "user@mbp.local:22", "provider": "codex", "model": "gpt-5.4"}):
        tools["apa_delegate"]({"parent": "a", **args}, session_id="s")
    tools["apa_delegate"]({"parent": "a", "mech": "paseo", "host": "mbp", "provider": "codex", "model": "o9"}, session_id="s")
    ids = [c["argv"][2] for c in calls(apa)]
    assert ids[0] == ids[1] == "hermes.paseo.mbp.codex.gpt-5-4" and ids[2] != ids[0]


def test_delegate_task_executor_lives_on_this_machine(tools, apa, monkeypatch):
    monkeypatch.setattr("socket.gethostname", lambda: "APA-Host.example")
    tools["apa_delegate"]({"parent": "a", "mech": "delegate_task"}, session_id="s")
    assert calls(apa)[0]["argv"][2] == "hermes.delegate_task.apa-host"
    assert stdin_json(apa)["kind"] == "native-subagent" and stdin_json(apa)["host"] == "apa-host"


@pytest.mark.parametrize("args,why", [
    ({"parent": "a", "mech": "codex_shell"}, "unknown mech"),
    ({"parent": "a", "mech": "paseo", "host": "mbp"}, "profile, or provider"),
    ({"parent": "a", "mech": "paseo", "host": "mbp; rm -rf /", "profile": "x"}, "host"),
    ({"parent": "a"}, "mech"),
])
def test_delegate_rejects_unknown_ways_without_running_apa(tools, apa, args, why):
    r = json.loads(tools["apa_delegate"](args, session_id="s"))
    assert r["ok"] is False and why in r["error"] and not (apa / "log").exists()


def test_dispatch_add_checks_hermes_handle_shapes_before_writing(tools, apa, monkeypatch):
    for bad in ({"mech": "paseo", "command": "rm -rf /"}, {"mech": "shell", "host": "x"}, {"mech": "paseo", "agent_id": "a b"}):
        r = json.loads(tools["apa_dispatch"]({"item": "itm_1", "handle": bad}, session_id="s"))
        assert r["ok"] is False
    assert not (apa / "log").exists()
    monkeypatch.setenv("FAKE_OUT", '{"id":"dsp_1","state":"running"}')
    tools["apa_dispatch"]({"item": "itm_1", "session": "agt_1", "resumable": True, "handle": {"mech": "paseo", "host": "mbp", "agent_id": "agt_1"}}, session_id="s")
    [c] = calls(apa)
    assert c["argv"] == ["dispatch", "add", "itm_1", "--session", "agt_1", "--resumable", "--handle", "-"]
    assert stdin_json(apa) == {"mech": "paseo", "host": "mbp", "agent_id": "agt_1"}


def test_dispatch_update_marks_lost(tools, apa):
    tools["apa_dispatch"]({"action": "update", "dispatch": "dsp_1", "state": "lost"}, session_id="s")
    assert calls(apa)[0]["argv"] == ["dispatch", "update", "dsp_1", "--state", "lost"]


def test_item_history_gives_resume_hints_only_for_hermes_own_valid_handles(tools, apa, monkeypatch):
    ex = {"id": "hermes.paseo.mbp.q", "harness": "hermes", "handle": {"mech": "paseo", "host": "mbp", "target": "mbp", "profile": "q"}}
    hist = {"history": [{"item": "itm_b", "dispatches": [
        {"id": "dsp_2", "item": "itm_b", "harness": "hermes", "session": "agt_1", "resumable": True, "state": "ended", "started_at": "2",
         "handle": {"mech": "paseo", "host": "mbp", "target": "mbp", "agent_id": "agt_1"}, "executor": ex},
        {"id": "dsp_1", "item": "itm_b", "harness": "claude-code", "session": "x", "resumable": True, "state": "ended", "started_at": "1",
         "handle": {"mech": "agent", "id": "x"}, "executor": {"id": "cc.sub"}},
        {"id": "dsp_0", "item": "itm_b", "harness": "hermes", "session": "agt_0", "resumable": True, "state": "ended", "started_at": "0",
         "handle": {"mech": "paseo", "host": "mbp", "agent_id": "agt_0; curl evil"}, "executor": ex},
    ]}]}
    monkeypatch.setenv("FAKE_OUT", json.dumps(hist))
    r = json.loads(tools["apa_item_history"]({"item": "itm_b"}, session_id="s9"))
    assert calls(apa)[0]["argv"] == ["context", "--scope", "item", "--item", "itm_b"]
    d2, d1, d0 = r["stdout"]["history"][0]["dispatches"]
    assert d2["hermes"]["use"] == "terminal" and d2["hermes"]["resume"] == 'paseo send --host mbp agt_1 "<message; name itm_b>"'
    assert d2["hermes"]["or_new"] == 'paseo-profile-run --host mbp q "<task; name itm_b>"'
    assert d1["hermes"]["use"] == "reference only" and "claude-code" in d1["hermes"]["why"]
    assert d0["hermes"]["use"] == "reference only"


# ---- 聊天里处置（附录 A.5）：三个代答工具 ----

def stdin_text(tmp):
    return (tmp / "stdin").read_text()


@pytest.mark.parametrize("tool,args,argv", [
    ("apa_attach", {"id": "p1", "to": "i9", "reason": "same topic", "evidence": "补到 i9"},
     ["attach", "p1", "--to", "i9", "--reason", "same topic"]),
    ("apa_item_comment", {"id": "i1", "text": "done A", "reply_to": "i1.p000001", "on": "2026-11-01", "grant": "g1"},
     ["item", "comment", "i1", "--text", "done A", "--reply-to", "i1.p000001", "--on", "2026-11-01"]),
    ("apa_proposals_apply", {"recommended": True, "source": "s1", "project": "p1", "evidence": "都按建议"},
     ["proposals", "apply", "--recommended", "--source", "s1", "--project", "p1"]),
])
def test_chat_dispose_tools_map_to_cli_with_session(tools, apa, tool, args, argv):
    tools[tool](args, session_id="s3")
    assert calls(apa) == [{"argv": argv, "session": "s3"}]


def test_proposals_apply_list_goes_through_stdin_not_a_file(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_OUT", '{"results":[{"id":"a","do":"dismiss","ok":true}]}')
    acts = [{"id": "a", "do": "dismiss", "reason": "no"}, {"id": "b", "do": "attach", "to": "i2"}, {"id": "c", "do": "accept"}]
    r = json.loads(tools["apa_proposals_apply"]({"actions": acts}, session_id="s"))
    assert calls(apa)[0]["argv"] == ["proposals", "apply", "--file", "-"]
    assert json.loads(stdin_text(apa)) == {"actions": acts}
    assert r["stdout"]["results"][0]["ok"] is True


@pytest.mark.parametrize("args", [
    {}, {"recommended": True, "actions": [{"id": "a", "do": "accept"}]}, {"actions": [{"id": "a", "do": "delete"}]},
    {"actions": [{"id": "a", "do": "attach"}]}, {"actions": [{"id": "a", "do": "accept", "to": "x"}]},
    {"actions": [{"do": "accept"}]}, {"actions": [{"id": "a", "do": "accept"}], "source": "s"},
    {"actions": [{"id": str(i), "do": "accept"} for i in range(201)]},
])
def test_proposals_apply_rejects_bad_calls_without_running_apa(tools, apa, args):
    r = json.loads(tools["apa_proposals_apply"](args, session_id="s"))
    assert r["ok"] is False and r["error"] and not (apa / "log").exists()


def test_chat_dispose_refusal_exit_23_is_exposed_not_retried(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "23")
    r = json.loads(tools["apa_attach"]({"id": "p", "to": "i"}, session_id="s"))
    assert r["exit_code"] == 23 and len(calls(apa)) == 1
