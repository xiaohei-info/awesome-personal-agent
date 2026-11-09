"""APA headless plugin for Hermes: declares how the kernel runs `hermes chat -q --oneshot` and submits that declaration.

The declaration is data (declaration.json); the kernel executes it, this plugin only hands it over via `apa declare`.
"""
import importlib.util
import json
import os
import pathlib
import subprocess

HERE = pathlib.Path(__file__).resolve().parent
TIMEOUT_SECS = 30

_spec = importlib.util.spec_from_file_location("apa_run_context", HERE.parent / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)


def declaration():
    """declaration.json plus the executors the tools plugin has configured.

    One harness holds one declaration (a new one replaces the old), so the executors must travel with it or
    activating this declaration would drop them from the list.
    """
    run_context.require_passthrough()
    body = json.loads((HERE / "declaration.json").read_text())
    spec = importlib.util.spec_from_file_location("apa_headless_executors", HERE.parent / "tools" / "executors.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    if mod.load():  # a malformed table raises ValueError to the caller; only a genuinely empty one is skipped
        body["executors"] = mod.declaration()["executors"]
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


def register(ctx):
    desc = "Submit this host's headless-run declaration (and configured executors) to the ledger (apa declare); it takes effect after the owner confirms in Web."
    ctx.register_tool(name="apa_declare_headless", toolset="apa", description=desc, handler=_declare,
                      schema={"name": "apa_declare_headless", "description": desc,
                              "parameters": {"type": "object", "properties": {}, "required": []}})
