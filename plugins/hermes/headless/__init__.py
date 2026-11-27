"""APA headless plugin for Hermes: declares how the kernel runs `hermes chat -q --oneshot` and submits that declaration.

The declaration is data (declaration.json) plus whether apa-wake can wake the main session here, as apa-wake's own
readiness.py judges it. The kernel executes it; this plugin only hands it over via `apa declare`: when the model calls
`apa_declare_headless`, and once each time the gateway starts. The kernel drops a submission equal to the latest one,
so an unchanged declaration never asks the owner again. It also writes each APA run's token usage into the run
directory (_record_usage).
"""
import importlib.util
import json
import logging
import os
import pathlib
import subprocess
import threading
import time

HERE = pathlib.Path(__file__).resolve().parent
TIMEOUT_SECS = 30
GATEWAY_WAIT_SECS = 120  # how long the start-up sync waits for the gateway to publish its injector
GATEWAY_POLL_SECS = 2
RETRY_SECS = (10, 60, 300)  # start-up sync retries while the kernel is unreachable

log = logging.getLogger("apa.headless")


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


run_context = _load("apa_run_context", HERE.parent / "tools" / "run_context.py")
readiness = _load("apa_wake_readiness", HERE.parent / "wake" / "readiness.py")


def declaration():
    """declaration.json plus main_session from apa-wake's readiness. Executors are not declared: Hermes registers them
    when it first delegates (design §6.17). It only says what this Agent can do; which one to use is the owner's
    choice in Web. With anything missing, wake is false and wake_missing says what (shown on the Agent card)."""
    run_context.require_passthrough()
    body = json.loads((HERE / "declaration.json").read_text())
    gaps = readiness.current()
    body["main_session"] = {"wake": True} if not gaps else {"wake_missing": readiness.missing(gaps)}
    return body


def _declare(args, session_id=None, **_):
    try:
        body = declaration()
        env = run_context.apa_env()
    except (OSError, ValueError) as e:
        return json.dumps({"ok": False, "error": str(e)})
    if session_id:
        env["APA_SESSION"] = str(session_id)
    try:
        p = subprocess.run([os.environ.get("APA_BIN", "apa"), "declare", "-"], input=json.dumps(body),
                           capture_output=True, text=True, timeout=TIMEOUT_SECS, env=env)
    except (OSError, subprocess.SubprocessError) as e:
        return json.dumps({"ok": False, "error": f"cannot run apa: {type(e).__name__}: {e}"})
    return json.dumps({"exit_code": p.returncode, "ok": p.returncode == 0, "stdout": p.stdout.strip(), "stderr": p.stderr.strip()})


_in_gateway = readiness.in_gateway  # once true every plugin is loaded and readiness is what the gateway really has


UNREACHABLE = 7  # apa exit code: kernel unreachable (kernel/domain ExitUnreachable)


def _sync(sleep=time.sleep):
    """Submit the declaration once the gateway is up. shortcut: only at gateway start-up; a later change of
    allow_gateway_injection waits for the next restart or an apa_declare_headless call. Upgrade: sync again
    from apa-wake's failure path when waking must follow live config changes."""
    for _ in range(GATEWAY_WAIT_SECS // GATEWAY_POLL_SECS):
        if _in_gateway():
            break
        sleep(GATEWAY_POLL_SECS)
    else:
        return None
    out = {}
    for wait in (0, *RETRY_SECS):
        sleep(wait)
        out = json.loads(_declare({}))
        if out.get("ok") or out.get("exit_code") != UNREACHABLE:
            break
    (log.info if out.get("ok") else log.warning)("apa-headless: start-up declaration sync: %s", out)
    return out


USAGE_FILE = "usage.json"  # declaration.json headless.usage.file
_usage_lock = threading.Lock()  # a run's subagents report from their own threads


def _record_usage(usage=None, model=None, **_):
    """post_api_request: add this model call to the run's own total in <run_dir>/usage.json, which the kernel reads as
    declared in headless.usage (appendix A.2). Summed per call because Hermes' session counters, and so the tokens on
    its result line, carry earlier runs over on a resume. A session that is not an APA run records nothing; a failure
    never reaches the conversation."""
    if not usage:
        return
    try:
        if not run_context.context_file():
            return
        path = pathlib.Path(run_context.load()["run_dir"]) / USAGE_FILE
        with _usage_lock:
            try:
                total = json.loads(path.read_text())
            except (OSError, ValueError):
                total = {}
            # prompt_tokens includes cache reads and writes
            for key, source in (("input_tokens", "prompt_tokens"), ("output_tokens", "output_tokens"),
                                ("cache_read_tokens", "cache_read_tokens"), ("cache_write_tokens", "cache_write_tokens")):
                total[key] = total.get(key, 0) + int(usage.get(source) or 0)
            if model:
                total["model"] = str(model)
            tmp = path.with_name(USAGE_FILE + ".tmp")
            tmp.write_text(json.dumps(total))
            os.replace(tmp, path)
    except Exception as e:
        log.warning("apa-headless: cannot record run usage: %s", type(e).__name__)


def register(ctx):
    ctx.register_hook("post_api_request", _record_usage)
    desc = "Submit this host's headless-run declaration to the ledger (apa declare); it takes effect after the owner confirms in Web."
    ctx.register_tool(name="apa_declare_headless", toolset="apa", description=desc, handler=_declare,
                      schema={"name": "apa_declare_headless", "description": desc,
                              "parameters": {"type": "object", "properties": {}, "required": []}})
    threading.Thread(target=_sync, daemon=True, name="apa-headless-sync").start()
