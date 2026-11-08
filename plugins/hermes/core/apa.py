"""Run the apa CLI for a hook: stdout on success, '' on any failure or >2 s (fail open)."""
import os
import subprocess

TIMEOUT_SECS = 2


def run(args, session_id=None):
    env = dict(os.environ, APA_HARNESS="hermes")
    if session_id:
        env["APA_SESSION"] = str(session_id)
    try:
        p = subprocess.run([os.environ.get("APA_BIN", "apa"), *args], capture_output=True,
                           text=True, timeout=TIMEOUT_SECS, env=env)
    except (OSError, subprocess.SubprocessError):
        return ""
    return p.stdout.strip() if p.returncode == 0 else ""
