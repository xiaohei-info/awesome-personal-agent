"""Run the apa CLI for a tool call. Unlike the hooks, a tool reports the real exit code and error body."""
import json
import os
import subprocess

from . import run_context

TIMEOUT_SECS = 30


def _json_or_text(s):
    s = s.strip()
    try:
        return json.loads(s) if s else None
    except ValueError:
        return s


def call(args, session_id=None, stdin=None):
    """Result for the model: {exit_code, ok, stdout, stderr}. The kernel's verdict (exit 4/20/21/22 ...) passes through untouched."""
    env = run_context.apa_env()
    if session_id:
        env["APA_SESSION"] = str(session_id)
    try:
        p = subprocess.run([os.environ.get("APA_BIN", "apa"), *args], input=stdin, capture_output=True,
                           text=True, timeout=TIMEOUT_SECS, env=env, cwd=run_context.session_cwd())
    except (OSError, subprocess.SubprocessError) as e:
        return {"ok": False, "error": f"cannot run apa: {type(e).__name__}: {e}"}
    return {"exit_code": p.returncode, "ok": p.returncode == 0,
            "stdout": _json_or_text(p.stdout), "stderr": _json_or_text(p.stderr)}
