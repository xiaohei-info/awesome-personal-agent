"""Run the apa CLI for a hook: stdout on success, '' on any failure or >2 s (fail open)."""
import importlib.util
import os
import pathlib
import subprocess

_spec = importlib.util.spec_from_file_location("apa_run_context", pathlib.Path(__file__).resolve().parents[1] / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)

TIMEOUT_SECS = 2


def run(args, session_id=None):
    env = run_context.apa_env()
    if session_id:
        env["APA_SESSION"] = str(session_id)
    try:
        p = subprocess.run([os.environ.get("APA_BIN", "apa"), *args], capture_output=True,
                           text=True, timeout=TIMEOUT_SECS, env=env, cwd=run_context.session_cwd())
    except (OSError, subprocess.SubprocessError):
        return ""
    return p.stdout.strip() if p.returncode == 0 else ""
