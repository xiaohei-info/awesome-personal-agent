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
                          "apa_report", "apa_dispatch", "apa_item_history", "apa_attach", "apa_proposals_apply", "apa_item_progress"}


def test_cli_calls_carry_harness_hermes(tools, apa):
    tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "x"}, session_id="s")
    assert (apa / "log.harness").read_text().split() == ["hermes"]


def test_decide_carries_hermes_session_and_evidence(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_OUT", '{"state":"done"}')
    r = json.loads(tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "就选方案 B"}, session_id="sess-9"))
    assert calls(apa) == [{"argv": ["decide", "i1", "--option", "B", "--evidence", "就选方案 B"], "session": "sess-9"}]
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
     ["ask", "i", "--question", "Q?", "--option", "A=yes", "--option", "B=no", "--next", "A=open", "--free-text",
      "--default-on-timeout", "A"]),
    ("apa_accept", {"id": "i", "evidence": "ok"}, ["accept", "i", "--evidence", "ok"]),
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


TIER2 = ["apa_decide", "apa_accept", "apa_dismiss", "apa_item_update", "apa_attach", "apa_proposals_apply", "apa_item_progress"]
TIER1 = ["apa_propose", "apa_ask", "apa_delegate", "apa_report", "apa_dispatch", "apa_item_history", "apa_executors", "apa_material_commit"]


@pytest.fixture
def schemas():
    ctx = Ctx()
    load_tools().register(ctx)
    return {n: s for n, (s, _) in ctx.tools.items()}


@pytest.mark.parametrize("tool", TIER2)
def test_second_tier_tools_offer_optional_evidence_and_grant(schemas, tool):
    sc = schemas[tool]
    assert {"evidence", "grant"} <= set(sc["parameters"]["properties"])
    assert not {"evidence", "grant"} & set(sc["parameters"]["required"])
    assert "this turn" in sc["parameters"]["properties"]["evidence"]["description"]
    assert "never" in sc["parameters"]["properties"]["evidence"]["description"].lower()


@pytest.mark.parametrize("tool", TIER1)
def test_first_tier_tools_have_no_evidence_or_grant(schemas, tool):
    assert not {"evidence", "grant"} & set(schemas[tool]["parameters"]["properties"])


@pytest.mark.parametrize("tool,args,argv", [
    ("apa_decide", {"id": "i", "option": "B", "grant": "g1"}, ["decide", "i", "--option", "B", "--grant", "g1"]),
    ("apa_accept", {"id": "i", "grant": "g1"}, ["accept", "i", "--grant", "g1"]),
    ("apa_dismiss", {"id": "i", "grant": "g1", "reason": "dup"}, ["dismiss", "i", "--reason", "dup", "--grant", "g1"]),
    ("apa_item_update", {"id": "i", "reason": "r", "grant": "g1"}, ["item", "update", "i", "--reason", "r", "--grant", "g1"]),
    ("apa_item_update", {"id": "i", "reason": "r", "evidence": "原话"}, ["item", "update", "i", "--reason", "r", "--evidence", "原话"]),
])
def test_second_tier_passes_grant_and_evidence_through_with_session(tools, apa, tool, args, argv):
    tools[tool](args, session_id="s7")
    assert calls(apa) == [{"argv": argv, "session": "s7"}]
    assert (apa / "log.harness").read_text().split() == ["hermes"]


def test_grant_refusal_exit_23_is_exposed(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT", "23")
    r = json.loads(tools["apa_decide"]({"id": "i", "option": "B", "grant": "g"}, session_id="s"))
    assert r["exit_code"] == 23 and r["ok"] is False


def kinds(tmp):
    return (tmp / "log.kind").read_text().split()


# ACT-HCTX-12：终端会话的 apa 调用带 APA_SESSION_KIND=direct（附录 A.5，SPIKE-SESSION-KIND）。
@pytest.mark.parametrize("bound", [{}, {"source": "cli"}, {"source": "tui"}, {"platform": "local", "source": "local"}])
def test_terminal_session_calls_carry_session_kind_direct(tools, apa, monkeypatch, bound):
    fake_session_context(monkeypatch, **bound)
    tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "就选方案 B"}, session_id="s1")
    assert kinds(apa) == ["direct"]
    assert calls(apa)[0]["session"] == "s1" and (apa / "log.harness").read_text().split() == ["hermes"]


# ACT-HCTX-13：网关会话按平台给出的会话来源报：私聊 direct、群/频道/话题 group，取不到 unknown。
@pytest.mark.parametrize("bound,want", [
    ({"platform": "telegram", "chat_type": "dm", "user_id": "42"}, "direct"),
    ({"platform": "telegram", "chat_type": "group", "user_id": "42"}, "group"),
    ({"platform": "slack", "chat_type": "channel", "user_id": "U1"}, "group"),
    ({"platform": "discord", "chat_type": "thread", "user_id": "U1"}, "group"),
    ({"platform": "telegram", "chat_type": "dm"}, "unknown"),          # 私聊却没有发送者：看不出是谁
    ({"platform": "telegram", "chat_type": "weird", "user_id": "42"}, "unknown"),
    ({"platform": "telegram", "user_id": "42"}, "unknown"),            # 没有会话类型
    ({"platform": "api_server", "chat_type": "dm", "user_id": "42"}, "unknown"),  # 非聊天入口：不替它说话
    ({"platform": "webhook", "chat_type": "group", "user_id": "42"}, "group"),  # rule 1 first: a multi-person type is never unknown
    ({"source": "gateway"}, "unknown"),
])
def test_gateway_session_kind_follows_the_platform(tools, apa, monkeypatch, bound, want):
    fake_session_context(monkeypatch, **bound)
    tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "x"}, session_id="s1")
    assert kinds(apa) == [want]


def test_unreadable_gateway_binding_is_unknown(tools, apa, monkeypatch):
    import sys
    import types
    mod = types.ModuleType("gateway.session_context")
    mod.get_session_env = lambda *a, **k: 1 / 0
    monkeypatch.setitem(sys.modules, "gateway", types.ModuleType("gateway"))
    monkeypatch.setitem(sys.modules, "gateway.session_context", mod)
    tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "x"}, session_id="s1")
    assert kinds(apa) == ["unknown"]


def test_no_hermes_session_module_is_unknown(tools, apa, monkeypatch):
    import sys
    monkeypatch.setitem(sys.modules, "gateway", None)  # import raises ImportError
    tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "x"}, session_id="s1")
    assert kinds(apa) == ["unknown"]


def test_session_kind_is_not_inherited_from_the_gateway_environment(tools, apa, monkeypatch):
    monkeypatch.setenv("APA_SESSION_KIND", "direct")  # what a gateway started from a terminal would carry
    fake_session_context(monkeypatch, platform="telegram", chat_type="group", user_id="42")
    tools["apa_decide"]({"id": "i1", "option": "B", "evidence": "x"}, session_id="s1")
    assert kinds(apa) == ["group"]


def test_every_tool_schema_has_no_session_kind_parameter():
    ctx = Ctx()
    load_tools().register(ctx)
    for name, (schema, _) in ctx.tools.items():
        assert not any("kind" in p and "session" in p for p in schema["parameters"]["properties"]), name
    assert "session_kind" not in json.dumps([s for s, _ in ctx.tools.values()])


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
    from apa_tools import dispatch
    assert "itm_c0ffee" in dispatch.delegated("s1")


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
    from apa_tools import dispatch
    assert dispatch.last_dispatch("itm_1", "agt_1") == "dsp_1"


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
    from apa_tools import dispatch
    assert "itm_b" in dispatch.delegated("s9") and dispatch.last_dispatch("itm_b", "agt_1") == "dsp_2"


# ---- 聊天里处置（附录 A.5）：三个代答工具 ----

def stdin_text(tmp):
    return (tmp / "stdin").read_text()


@pytest.mark.parametrize("tool,args,argv", [
    ("apa_attach", {"id": "p1", "to": "i9", "reason": "same topic", "evidence": "补到 i9"},
     ["attach", "p1", "--to", "i9", "--reason", "same topic", "--evidence", "补到 i9"]),
    ("apa_item_progress", {"id": "i1", "text": "done A", "on": "2026-11-01", "grant": "g1"},
     ["item", "progress", "i1", "--text", "done A", "--on", "2026-11-01", "--grant", "g1"]),
    ("apa_proposals_apply", {"recommended": True, "source": "s1", "project": "p1", "evidence": "都按建议"},
     ["proposals", "apply", "--recommended", "--source", "s1", "--project", "p1", "--evidence", "都按建议"]),
])
def test_chat_dispose_tools_map_to_cli_with_session(tools, apa, tool, args, argv):
    tools[tool](args, session_id="s3")
    assert calls(apa) == [{"argv": argv, "session": "s3"}]
    assert kinds(apa) == ["direct"] or kinds(apa) == ["unknown"]


def test_proposals_apply_list_goes_through_stdin_not_a_file(tools, apa, monkeypatch):
    monkeypatch.setenv("FAKE_OUT", '{"results":[{"id":"a","do":"dismiss","ok":true}]}')
    acts = [{"id": "a", "do": "dismiss", "reason": "no"}, {"id": "b", "do": "attach", "to": "i2"}, {"id": "c", "do": "accept"}]
    r = json.loads(tools["apa_proposals_apply"]({"actions": acts, "evidence": "第 1 条忽略，其余按建议"}, session_id="s"))
    assert calls(apa)[0]["argv"] == ["proposals", "apply", "--file", "-", "--evidence", "第 1 条忽略，其余按建议"]
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
    r = json.loads(tools["apa_attach"]({"id": "p", "to": "i", "evidence": "x"}, session_id="s"))
    assert r["exit_code"] == 23 and len(calls(apa)) == 1


# 附录 A.5 五条顺序；APA_DIRECT_PLATFORMS 只在本人声明时生效。
@pytest.mark.parametrize("env,bound,want", [
    ("telegram", {"platform": "telegram", "chat_type": "group", "user_id": "1"}, "group"),   # 名单里的平台加多人类型仍是 group
    ("local,cli", {"platform": "local", "source": "local", "chat_type": "thread"}, "group"),  # 终端加多人类型也是 group
    ("Bot-Chat", {"platform": "bot-chat"}, "direct"),                                          # 名单（不分大小写）
    (" a , bot ", {"source": "bot", "platform": "x"}, "direct"),                              # 来源在名单里
    ("api_server", {"platform": "api_server"}, "direct"),                                      # 本人声明的入口
    ("other", {"platform": "api_server"}, "unknown"),
    ("", {"platform": "bot-chat"}, "unknown"),                                                  # 不设：与此前相同
    ("other", {"platform": "telegram", "chat_type": "dm"}, "unknown"),                         # 第 4 条要发送者
    ("other", {"platform": "telegram", "chat_type": "dm", "user_id": "7"}, "direct"),
])
def test_session_kind_order_with_direct_platforms(tools, apa, monkeypatch, env, bound, want):
    monkeypatch.setenv("APA_DIRECT_PLATFORMS", env)
    fake_session_context(monkeypatch, **bound)
    tools["apa_accept"]({"id": "i", "evidence": "x"}, session_id="s")
    assert kinds(apa) == [want]


# ACT-CHT-05：名单里的平台加多人会话，三个代答工具都报 group，内核的 23 原样交回、不重试。
@pytest.mark.parametrize("tool,args", [
    ("apa_attach", {"id": "p", "to": "i", "evidence": "补到 i"}),
    ("apa_item_progress", {"id": "i", "text": "t", "evidence": "记一条"}),
    ("apa_proposals_apply", {"recommended": True, "evidence": "按建议处理"}),
])
def test_chat_dispose_tools_in_a_group_session_report_group_and_expose_exit_23(tools, apa, monkeypatch, tool, args):
    monkeypatch.setenv("APA_DIRECT_PLATFORMS", "telegram")
    monkeypatch.setenv("FAKE_EXIT", "23")
    monkeypatch.setenv("FAKE_ERR", '{"code":"session_not_direct"}')
    fake_session_context(monkeypatch, platform="telegram", chat_type="group", user_id="42")
    r = json.loads(tools[tool](args, session_id="s"))
    assert kinds(apa) == ["group"] and r["exit_code"] == 23 and r["stderr"]["code"] == "session_not_direct" and len(calls(apa)) == 1
