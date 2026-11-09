"""APA inject plugin for Hermes: `pre_llm_call` fetches context through the apa CLI and returns it for the
user message. No decisions here: the kernel owns increments, cursors and the envelope.

Fails open: any apa failure, missing binary or >2 s yields no injection and never blocks the message.
"""
import importlib.util
import os
import pathlib
import subprocess
import threading
import time

_spec = importlib.util.spec_from_file_location("apa_run_context", pathlib.Path(__file__).resolve().parents[1] / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)

TIMEOUT_SECS = 2
INSTRUCTION_VERSION = 1  # version of the standing instructions this plugin ships (kernel compares)

_keys = {}          # Hermes session id -> session key whose kernel cursor this process continues
_reinject = set()   # session ids whose next turn must inject the full summary again (after compaction)


def _run(args, session_key, out, slot, env, cwd):
    try:
        p = subprocess.run([os.environ.get("APA_BIN", "apa"), *args], capture_output=True, text=True,
                           timeout=TIMEOUT_SECS, env=env, cwd=cwd)
        out[slot] = p.stdout.strip() if p.returncode == 0 else ""
    except (OSError, subprocess.SubprocessError):
        out[slot] = ""


def _instruction_version():
    try:
        return int(os.environ.get("APA_INSTRUCTION_VERSION", INSTRUCTION_VERSION))
    except ValueError:
        return INSTRUCTION_VERSION


def pre_llm_call(session_id="", is_first_turn=False, **_):
    if not session_id:  # without a session the kernel keeps no cursor and would repeat everything
        return None
    fresh = is_first_turn or session_id not in _keys or session_id in _reinject
    if fresh:
        # A new kernel cursor makes `apa context` return the full projection again (resume, compaction);
        # the plain session id is kept for a genuinely new session.
        new = is_first_turn and session_id not in _keys
        _keys[session_id] = session_id if new else f"{session_id}~{time.time_ns()}"
        _reinject.discard(session_id)
    key = _keys[session_id]

    ctx_args = ["context", "--session", key, "--scope", "cwd", "--format", "inject"]
    jobs = [ctx_args]
    if fresh:  # the stale-instructions notice is asked for once per injection start
        ctx_args += ["--instruction", str(_instruction_version())]
        jobs.append(["agents", "check"])
    out = [""] * len(jobs)
    # bare threads start with an empty context: read the session's scope here, on the hook's thread
    try:
        env, cwd = run_context.apa_env(APA_SESSION=key), run_context.session_cwd()
    except ValueError:  # a run without its context: no injection rather than one under the gateway's credential
        return None
    threads = [threading.Thread(target=_run, args=(a, key, out, i, env, cwd), daemon=True) for i, a in enumerate(jobs)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(TIMEOUT_SECS + 1)
    return {"context": out[0]} if out[0] else None


def post_compaction(session_id="", **_):
    _reinject.add(session_id)  # compaction may keep the id or move to a new one; the new id is unseen anyway


def register(ctx):
    ctx.register_hook("pre_llm_call", pre_llm_call)
    ctx.register_hook("post_compaction", post_compaction)
