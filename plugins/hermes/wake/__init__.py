"""APA wake plugin for Hermes (gateway only): claims `class: wake` deliveries and opens a turn in the real main session.

No judgment here (design §13.6, appendix A.1): the kernel decides which run wakes whom and what the run may do.
The plugin only
  1. claims a wake delivery (GET /deliveries/next) with its own worker/delegate token (APA_TOKEN),
  2. swaps the run id for a short-lived run credential (POST /runs/{id}/credential),
  3. injects a facts-only message with ctx.inject_message, then acks (or fails) the delivery,
  4. keeps the credential in memory and hands it only to `apa_wake_cli`, which runs `apa` with APA_RUN/APA_TOKEN.
The credential is never put in the message, the environment of the gateway, or other tools.

Explicit opt-in: nothing starts unless APA_WAKE_SESSION_KEY (the main session to wake) is set, and Hermes must allow it
with plugins.entries.apa-wake.allow_gateway_injection: true. Without that the harness stays headless-only.
Any failure leaves Hermes untouched: the loop logs, backs off, and the kernel retries a failed delivery.
"""
import importlib.util
import json
import logging
import os
import pathlib
import subprocess
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

_spec = importlib.util.spec_from_file_location("apa_run_context", pathlib.Path(__file__).resolve().parents[1] / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)

log = logging.getLogger("apa.wake")

HTTP_TIMEOUT_SECS = 10
LONG_POLL_SECS = 30  # kernel caps wait at 60
CLI_TIMEOUT_SECS = 30
BACKOFF_SECS = 5
REFRESH_MARGIN_SECS = 30

_runs = {}  # run id -> {"token", "expires": epoch secs}; in memory only
_lock = threading.Lock()
stop = threading.Event()  # set by tests; Hermes ends the daemon thread with the process


class WakeError(Exception):
    pass


def _http(method, path, body=None, timeout=HTTP_TIMEOUT_SECS):
    """JSON call to the kernel with the plugin's own token. Returns (status, parsed body or None)."""
    url = os.environ.get("APA_URL", "").rstrip("/")
    token = os.environ.get("APA_TOKEN", "")
    if not url or not token:
        raise WakeError("APA_URL and APA_TOKEN are required")
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{url}/v1{path}", data=data, method=method,
                                 headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")[:300]
        raise WakeError(f"{method} {path} -> {e.code}: {detail}") from e
    except (OSError, ValueError) as e:
        raise WakeError(f"{method} {path}: {type(e).__name__}: {e}") from e


def _epoch(ts):
    return datetime.fromisoformat(ts.replace("Z", "+00:00")).astimezone(timezone.utc).timestamp()


def exchange(run_id):
    """Swap the run id for a run credential (replaces any earlier one for that run: the kernel voids the old one)."""
    _, out = _http("POST", f"/runs/{run_id}/credential", {})
    if not out or not out.get("token"):
        raise WakeError(f"credential exchange for {run_id} returned no token")
    with _lock:
        _runs[run_id] = {"token": out["token"], "expires": _epoch(out["expires_at"])}


def message(d):
    """Facts only; no credential, no instructions beyond how to reach the ledger as this run."""
    run, item = d["run"], d["item"]
    title = (d.get("content") or {}).get("title", "")
    return (f"[APA wake] Background run {run} woke this session for item {item}. {title}\n"
            f"Handle it as in chat: read `apa context`, then report. Run every apa command through the "
            f"`apa_wake_cli` tool with run_id={run}; it carries the run identity. This run cannot decide, "
            f"accept or dismiss for the owner: if the owner must decide, ask and end the run as needs_input.")


def handle(ctx, d, session_key):
    """One claimed delivery: exchange, inject, ack. Failures are reported back so the kernel can retry."""
    ver = d["version"]
    try:
        if not d.get("run"):
            raise WakeError("wake delivery carries no run id")
        exchange(d["run"])
        if not ctx.inject_message(message(d), session_key=session_key):
            raise WakeError("Hermes refused the injection (allow_gateway_injection, live gateway, session key)")
    except Exception as e:  # noqa: BLE001 - any failure goes back to the kernel; the credential must not linger
        with _lock:
            _runs.pop(d.get("run"), None)
        try:
            _http("POST", f"/deliveries/{d['id']}/fail", {"version": ver, "reason": str(e)[:200]})
        except WakeError as e2:
            log.warning("apa-wake: fail report for %s lost: %s", d["id"], e2)
        raise
    _http("POST", f"/deliveries/{d['id']}/ack", {"version": ver, "external_ref": d["run"]})


def poll_once(ctx, session_key, wait=LONG_POLL_SECS):
    """Claim at most one wake delivery and handle it. Returns the delivery or None when the queue is empty."""
    status, d = _http("GET", f"/deliveries/next?wait={wait}", timeout=wait + HTTP_TIMEOUT_SECS)
    if status == 204 or not d:
        return None
    handle(ctx, d, session_key)
    return d


def _loop(ctx, session_key, stop):
    while not stop.is_set():
        try:
            poll_once(ctx, session_key)
        except Exception as e:  # noqa: BLE001 - fail open: never take the gateway down
            log.warning("apa-wake: %s", e)
            stop.wait(BACKOFF_SECS)


def _credential(run_id):
    """The run's token, re-exchanged when it is about to expire (the old one is voided by the kernel)."""
    with _lock:
        cur = _runs.get(run_id)
    if cur is None:
        raise WakeError(f"no wake credential for run {run_id}")
    if cur["expires"] - time.time() < REFRESH_MARGIN_SECS:
        exchange(run_id)
        with _lock:
            cur = _runs[run_id]
    return cur["token"]


def _cli(params, **_):
    """Run `apa` as the wake run. The kernel's verdict (exit 4 for decide/accept/dismiss, ...) passes through untouched."""
    args, run_id = params.get("args"), params.get("run_id")
    try:
        if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
            raise WakeError("args must be a list of strings")
        if not run_id:
            with _lock:
                ids = list(_runs)
            if len(ids) != 1:
                raise WakeError("pass run_id: " + (f"{len(ids)} wake runs are active" if ids else "no wake run is active"))
            run_id = ids[0]
        env = run_context.gateway_env(APA_RUN=run_id, APA_TOKEN=_credential(run_id))
        env.pop(run_context.VAR, None)  # the identity is explicit; the CLI would prefer an inherited context
        p = subprocess.run([os.environ.get("APA_BIN", "apa"), *args], capture_output=True, text=True,
                           timeout=CLI_TIMEOUT_SECS, env=env)
    except (WakeError, OSError, subprocess.SubprocessError) as e:
        return json.dumps({"ok": False, "error": f"{type(e).__name__}: {e}"})
    return json.dumps({"exit_code": p.returncode, "ok": p.returncode == 0,
                       "stdout": p.stdout.strip(), "stderr": p.stderr.strip()})


SCHEMA = {"name": "apa_wake_cli",
          "description": "Run an apa CLI command under the identity of the APA wake run that woke this session.",
          "parameters": {"type": "object", "properties": {
              "args": {"type": "array", "items": {"type": "string"}, "description": "apa arguments, e.g. [\"context\"]"},
              "run_id": {"type": "string", "description": "the wake run id from the wake message"}},
              "required": ["args"]}}


def register(ctx):
    ctx.register_tool(name="apa_wake_cli", toolset="apa", description=SCHEMA["description"], handler=_cli, schema=SCHEMA)
    key = os.environ.get("APA_WAKE_SESSION_KEY", "")
    if not key:  # waking is declared explicitly; default stays headless
        return
    threading.Thread(target=_loop, args=(ctx, key, stop), daemon=True, name="apa-wake").start()
