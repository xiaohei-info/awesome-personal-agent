"""APA capture plugin for Hermes: `on_session_end` (turn level) posts the conversation delta to `POST /capture`.
No decisions here: whether capture is on for the project, run-identity suppression and candidate extraction
all belong to the kernel (it answers 200 `captured: false` otherwise).

`on_session_end` carries no transcript, so `post_llm_call` buffers each finished turn per session and the
end hook flushes that buffer (the per-session cursor: a turn is sent once). The POST runs in a detached
helper process (`python capture/__init__.py`, JSON on stdin), prints nothing and never uses forced
continuation; any failure is swallowed and never reaches Hermes.

Failures are not silent: every one is appended to the plugin-local log (command, status/reason; never credentials
or conversation text). A kernel that answers but refuses is only logged. An unreachable kernel keeps the request
(with its Idempotency-Key) in the state dir; the next hook re-sends it once the kernel is back (A.1).
"""
import hashlib
import importlib.util
import json
import os
import pathlib
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

_spec = importlib.util.spec_from_file_location("apa_run_context", pathlib.Path(__file__).resolve().parents[1] / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)

TEXT_MAX = 20000
TIMEOUT_SECS = 10

HARNESS = "hermes"

_pending = {}  # session id -> [turn text, ...] not yet sent


def _turn_text(user_message, assistant_response):
    parts = []
    if isinstance(user_message, str) and user_message.strip():
        parts.append("user: " + user_message.strip())
    if isinstance(assistant_response, str) and assistant_response.strip():
        parts.append("assistant: " + assistant_response.strip())
    return "\n".join(parts)


def _state_dir(env):
    d = env.get("APA_HERMES_CAPTURE_DIR") or str(pathlib.Path(env.get("HERMES_HOME") or pathlib.Path.home() / ".hermes") / "apa-capture")
    return pathlib.Path(d)


def _log(env, reason):
    """One line: command and reason only (no credentials, no conversation text)."""
    try:
        d = _state_dir(env)
        d.mkdir(parents=True, exist_ok=True)
        with open(d / "capture.log", "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S%z')} POST /v1/capture {reason}\n")
    except OSError:
        pass


def _spawn(payload):
    """Hand the payload (or None: only re-send what is kept) to a detached helper; returns at once."""
    p = subprocess.Popen([sys.executable, "-I", str(pathlib.Path(__file__).resolve())], stdin=subprocess.PIPE,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True, env=run_context.apa_env())
    p.stdin.write(json.dumps(payload).encode())
    p.stdin.close()


def _has_kept():
    try:
        return any(_state_dir(os.environ).joinpath("pending").glob("*.json"))
    except OSError:
        return False


def post_llm_call(session_id="", user_message=None, assistant_response=None, **_):
    try:
        text = _turn_text(user_message, assistant_response)
        if session_id and text:
            _pending.setdefault(str(session_id), []).append(text)
        if _has_kept():
            _spawn(None)
    except Exception:
        pass


def on_session_end(session_id="", **_):
    try:
        turns = _pending.pop(str(session_id), None)
        if not turns:
            if _has_kept():
                _spawn(None)
            return
        text = "\n\n".join(turns)[-TEXT_MAX:]
        _spawn({"text": text, "session": str(session_id), "path": run_context.session_cwd() or "", "host": socket.gethostname(),
                "context": run_context.context_file()})
    except Exception:
        pass  # fail open: capture problems must never reach the conversation


def _credentials(env, context=None):
    """Same rule as the apa CLI: a run uses only its run credential and never the saved file. A session bound to a run
    context uses that run's; once the run has ended the file is gone and the request is refused here, as the kernel would."""
    if context:
        ctx = json.loads(pathlib.Path(context).read_text())
        return ctx["url"], ctx["token"]
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


def _post(item, env):
    """One POST. Returns None when delivered or refused (refusal is logged), "unreachable" when it should be kept."""
    try:
        url, token = _credentials(env, item.get("context"))
    except (OSError, ValueError) as e:
        _log(env, "no usable credentials: " + type(e).__name__)
        return None
    if not (url and token):
        _log(env, "no usable credentials")
        return None
    req = urllib.request.Request(url.rstrip("/") + "/v1/capture", data=json.dumps(item["body"]).encode(), method="POST",
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json",
                                          "X-APA-Session": item["session"], "X-APA-Harness": HARNESS,
                                          "Idempotency-Key": item["key"]})
    try:
        urllib.request.urlopen(req, timeout=TIMEOUT_SECS).read()
    except urllib.error.HTTPError as e:  # kernel reachable but refused: logged by us, health view by the kernel; no resend
        _log(env, f"status={e.code} reason={e.reason}")
    except (urllib.error.URLError, OSError) as e:  # URLError, refused connection, timeout
        _log(env, "kernel unreachable: " + str(getattr(e, "reason", e) or type(e).__name__))
        return "unreachable"
    return None


def _send(payload, env):
    """Keep the request first (so a crash or dead kernel loses nothing), then send everything kept, oldest first."""
    pend = _state_dir(env) / "pending"
    pend.mkdir(parents=True, exist_ok=True)
    if payload:
        session, context = payload.pop("session"), payload.pop("context", None)
        key = "hermes-capture-" + hashlib.sha1(f"{session}|{payload.get('text')}".encode()).hexdigest()[:16]
        f = pend / (key + ".json")
        f.write_text(json.dumps({"session": session, "key": key, "body": payload, "context": context}))
        f.chmod(0o600)
    for f in sorted(pend.glob("*.json"), key=lambda p: p.stat().st_mtime):
        claimed = f.with_suffix(".sending")
        try:
            f.rename(claimed)  # atomic claim: two helpers never send the same kept request
        except OSError:
            continue
        try:
            item = json.loads(claimed.read_text())
            outcome = _post(item, env)
        except (OSError, ValueError, KeyError) as e:
            _log(env, "unusable kept request: " + type(e).__name__)
            outcome = None
        if outcome == "unreachable":
            claimed.rename(f)
            return  # the kernel is down: no point trying the rest
        claimed.unlink()


def register(ctx):
    ctx.register_hook("post_llm_call", post_llm_call)
    ctx.register_hook("on_session_end", on_session_end)


if __name__ == "__main__":
    try:
        _send(json.load(sys.stdin), dict(os.environ))
    except Exception as e:
        _log(dict(os.environ), "helper failed: " + type(e).__name__)
