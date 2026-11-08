"""APA capture plugin for Hermes: `on_session_end` (turn level) posts the conversation delta to `POST /capture`.
No decisions here: whether capture is on for the project, run-identity suppression and candidate extraction
all belong to the kernel (it answers 200 `captured: false` otherwise).

`on_session_end` carries no transcript, so `post_llm_call` buffers each finished turn per session and the
end hook flushes that buffer (the per-session cursor: a turn is sent once). The POST runs in a detached
helper process (`python capture/__init__.py`, JSON on stdin), prints nothing and never uses forced
continuation; any failure is swallowed (written to the plugin-local log `apa-capture.log` under HERMES_HOME) and never
reaches Hermes. The owner's own words of the turn ride along as `X-APA-Evidence` (needed under a delegate
credential; candidates still land only as proposed).
"""
import json
import os
import pathlib
import socket
import subprocess
import sys
import urllib.request

TEXT_MAX = 20000
TIMEOUT_SECS = 10

_pending = {}  # session id -> [turn text, ...] not yet sent
_last_user = {}  # session id -> the owner's latest message (this turn's own words)
EVIDENCE_MAX = 2000


def _log(msg):
    try:
        home = pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes"))
        with open(home / "apa-capture.log", "a") as f:
            f.write(msg.rstrip() + "\n")
    except Exception:
        pass


def _turn_text(user_message, assistant_response):
    parts = []
    if isinstance(user_message, str) and user_message.strip():
        parts.append("user: " + user_message.strip())
    if isinstance(assistant_response, str) and assistant_response.strip():
        parts.append("assistant: " + assistant_response.strip())
    return "\n".join(parts)


def post_llm_call(session_id="", user_message=None, assistant_response=None, **_):
    try:
        text = _turn_text(user_message, assistant_response)
        if session_id and text:
            _pending.setdefault(str(session_id), []).append(text)
            if isinstance(user_message, str) and user_message.strip():
                _last_user[str(session_id)] = user_message
    except Exception as e:
        _log(f"post_llm_call: {e!r}")


def on_session_end(session_id="", **_):
    try:
        turns = _pending.pop(str(session_id), None)
        evidence = _last_user.pop(str(session_id), "")
        if not turns:
            return
        text = "\n\n".join(turns)[-TEXT_MAX:]
        payload = {"text": text, "session": str(session_id), "path": os.getcwd(), "host": socket.gethostname(),
                   "evidence": evidence}
        home = pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes"))
        with open(home / "apa-capture.log", "a") as log:
            p = subprocess.Popen([sys.executable, "-I", str(pathlib.Path(__file__).resolve())], stdin=subprocess.PIPE,
                                 stdout=subprocess.DEVNULL, stderr=log, start_new_session=True)
        p.stdin.write(json.dumps(payload).encode())
        p.stdin.close()
    except Exception as e:
        _log(f"on_session_end: {e!r}")  # fail open: capture problems must never reach the conversation


def _credentials(env):
    """Same rule as the apa CLI: a run (APA_RUN) uses only its run credential and never the saved file."""
    url, token = env.get("APA_URL", ""), env.get("APA_TOKEN", "")
    if env.get("APA_RUN"):
        f = env.get("APA_RUN_CREDENTIAL_FILE")
        if not token and f:
            token = pathlib.Path(f).read_text().strip()
        return url, token
    if not (url and token):
        home = env.get("APA_HOME") or str(pathlib.Path.home() / ".apa")
        saved = json.loads((pathlib.Path(home) / "credentials.json").read_text())
        url, token = url or saved.get("url", ""), token or saved.get("token", "")
    return url, token


def _send(payload, env):
    url, token = _credentials(env)
    if not (url and token):
        return
    session = payload.pop("session")
    evidence = " ".join(str(payload.pop("evidence", "") or "").split())[:EVIDENCE_MAX]
    headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json", "X-APA-Session": session}
    if evidence:  # http.client sends headers as latin-1; this passes the UTF-8 bytes through untouched
        headers["X-APA-Evidence"] = evidence.encode("utf-8").decode("latin-1")
    req = urllib.request.Request(url.rstrip("/") + "/v1/capture", data=json.dumps(payload).encode(), method="POST",
                                 headers=headers)
    urllib.request.urlopen(req, timeout=TIMEOUT_SECS).read()


def register(ctx):
    ctx.register_hook("post_llm_call", post_llm_call)
    ctx.register_hook("on_session_end", on_session_end)


if __name__ == "__main__":
    try:
        _send(json.load(sys.stdin), dict(os.environ))
    except Exception as e:
        print(f"capture send failed: {e!r}", file=sys.stderr)  # stderr is the plugin-local log
