"""Multi-turn driver for ONE real Hermes session (live e2e only; no product logic).

`hermes chat -q` is one process per turn, which makes the plugins see a new/resumed session every time.
Per-turn increments (ACT-HCTX-04) need several turns in one process, so this builds the AIAgent exactly the
way `hermes_cli/oneshot.py:_run_agent` does (real config, real model endpoint, real plugin loader and hooks)
and keeps it alive across turns. Protocol: JSON lines on stdin, one `APA-TURN <json>` line per command on stdout.

  {"op":"say","text":"..."}   run one turn      -> {"session_id","final","tools":[{"name","args"}],"secs",
                                                    "hook_secs","injected"}  (observed from Hermes' own pre_llm_call collection)
  {"op":"compact"}            fire Hermes' own post_compaction event for this session
  {"op":"exit"}
Env: DRIVER_RESUME=<session id> resumes a stored session (same loader as `hermes chat --resume`).
"""
import json
import logging
import os
import sys
import time

REAL_STDOUT = sys.stdout
sys.stdout = sys.stderr  # anything Hermes prints must not corrupt the protocol
logging.disable(logging.CRITICAL)


def emit(obj):
    REAL_STDOUT.write("APA-TURN " + json.dumps(obj, ensure_ascii=False) + "\n")
    REAL_STDOUT.flush()


def build_agent():
    from hermes_cli.config import load_config
    from hermes_cli.oneshot import (_create_session_db_for_oneshot, _load_resume_target, _resolve_model_and_provider,
                                    _oneshot_clarify_callback)
    from hermes_cli.runtime_provider import resolve_runtime_with_fallback
    from hermes_cli.tools_config import _get_platform_tools
    from hermes_constants import resolve_reasoning_config
    from run_agent import AIAgent

    cfg = load_config()
    choice = _resolve_model_and_provider(cfg, None, None)
    db = _create_session_db_for_oneshot()
    sid, history, _ = _load_resume_target(db, os.environ.get("DRIVER_RESUME") or None)
    runtime, _fb = resolve_runtime_with_fallback(cfg, requested=choice.provider, target_model=choice.model or None,
                                                 explicit_base_url=choice.base_url, explicit_api_key=choice.api_key)
    agent = AIAgent(
        api_key=runtime.get("api_key"), base_url=runtime.get("base_url"), provider=runtime.get("provider"),
        requested_provider=runtime.get("requested_provider"), api_mode=runtime.get("api_mode"), model=choice.model,
        enabled_toolsets=sorted(_get_platform_tools(cfg, "cli")), quiet_mode=True, platform="cli", session_db=db,
        session_id=sid, credential_pool=runtime.get("credential_pool"), request_overrides=runtime.get("request_overrides"),
        reasoning_config=resolve_reasoning_config(cfg, choice.model), clarify_callback=_oneshot_clarify_callback)
    agent.suppress_status_output = True
    agent.stream_delta_callback = None
    agent.tool_gen_callback = None
    return agent, history, db


def tools_of(messages, start):
    out = []
    for m in messages[start:]:
        for tc in (m.get("tool_calls") or []) if m.get("role") == "assistant" else []:
            fn = tc.get("function") or {}
            out.append({"name": fn.get("name"), "args": fn.get("arguments")})
    return out


HOOK = {"secs": 0.0, "context": ""}


def observe_pre_llm_call():
    """Wrap (not replace) Hermes' pre_llm_call collection to record what the plugins injected and how long it took."""
    from agent import turn_context
    real = turn_context._collect_pre_llm_call_context

    def wrapped(*a, **kw):
        t = time.monotonic()
        try:
            out = real(*a, **kw)
        finally:
            HOOK["secs"] = time.monotonic() - t
        HOOK["context"] = out if isinstance(out, str) else json.dumps(out, ensure_ascii=False)
        return out

    turn_context._collect_pre_llm_call_context = wrapped


def main():
    agent, history, db = build_agent()
    observe_pre_llm_call()
    for line in sys.stdin:
        cmd = json.loads(line)
        if cmd["op"] == "exit":
            break
        if cmd["op"] == "compact":
            from agent.compaction_events import fire_post_compaction
            fire_post_compaction(session_id=agent.session_id, kind="auto", reason="live-e2e", before_tokens=9,
                                 after_tokens=1, messages_removed=3)
            emit({"session_id": agent.session_id, "compacted": True})
            continue
        if cmd["op"] == "compress":  # Hermes' own manual-compression path; it fires post_compaction itself
            before_n = len(history)
            history, _ = agent._compress_context(history, None, force=True)
            emit({"session_id": agent.session_id, "compacted": True, "before": before_n, "after": len(history)})
            continue
        t = time.monotonic()
        before = len(history)
        result = agent.run_conversation(cmd["text"], conversation_history=history or None)
        history = result.get("messages") or history
        emit({"session_id": agent.session_id, "final": result.get("final_response") or "",
              "tools": tools_of(history, before), "secs": time.monotonic() - t,
              "hook_secs": HOOK["secs"], "injected": HOOK["context"]})
        HOOK["context"] = ""
    try:
        agent.close()
        db.close()
    except Exception:
        pass
    os._exit(0)


if __name__ == "__main__":
    main()
