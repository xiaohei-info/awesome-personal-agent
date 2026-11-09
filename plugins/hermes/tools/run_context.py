"""Which APA run a tool or hook call belongs to.

On Hermes' one-gateway runtime (v0.23.0+) `hermes chat` is a thin client: tools and hooks run in the shared gateway
process, whose cwd and environment are not this run's (a gateway started by a run's client even keeps that run's APA_RUN
and APA_TOKEN for life). The only per-session value Hermes carries over is the client's allowlisted environment, frozen
with the session and bound to its turns in a ContextVar. The kernel puts one name there, APA_RUN_CONTEXT: the path of the
0600 file it wrote at launch with this run's id, credential and directories, removed when the run ends (design §7.7).
"""
import json
import os

VAR = "APA_RUN_CONTEXT"
SETUP = f"add {VAR} to terminal.client_env_passthrough in the Hermes profile's config.yaml"


def _client_env():
    try:
        import agent.session_client_env as m
    except ImportError:  # no gateway runtime: this process is the client, so its environment and cwd are the session's
        return None
    return m


def context_file():
    """Path of this session's run context file, or None when the session does not belong to an APA run."""
    m = _client_env()
    return (m.bound_client_env() or {}).get(VAR) if m else None


def load():
    """This run's context {run, url, token, project?, run_dir, work_dir}; ValueError says why there is none, nothing is guessed."""
    if _client_env() is None:
        raise ValueError("no run context: this Hermes has no per-session client environment (it needs v0.23.0 or later)")
    path = context_file()
    if not path:
        raise ValueError(f"no run context: this session was not started by an APA run, or {SETUP}")
    try:
        with open(path) as f:
            ctx = json.load(f)
    except FileNotFoundError:
        raise ValueError("no run context: the APA run has ended") from None
    except (OSError, ValueError) as e:
        raise ValueError(f"no run context: cannot read the run context file ({type(e).__name__})") from None
    if not (isinstance(ctx, dict) and ctx.get("work_dir") and ctx.get("run_dir")):
        raise ValueError("no run context: the run context file names no directories")
    return ctx


def apa_env(**extra):
    """Environment for an `apa` subprocess: this session's run identity, never the one the gateway inherited.
    The CLI reads the rest from the context file; a session with no run uses the gateway's own credentials."""
    env = dict(os.environ, APA_HARNESS="hermes", **extra)
    if _client_env() is None:
        return env
    env.pop(VAR, None)
    if env.get("APA_RUN"):  # left by the run whose client started this gateway, token and project included
        for k in ("APA_RUN", "APA_TOKEN", "APA_PROJECT", "APA_RUN_CREDENTIAL_FILE"):
            env.pop(k, None)
    path = context_file()
    if path:
        env[VAR] = path
    return env


def session_cwd():
    """The session's frozen working directory (a run's is its work_dir), or None when it has none; never the gateway's."""
    if _client_env() is None:
        return os.getcwd()
    from agent.runtime_cwd import scoped_session_cwd
    return scoped_session_cwd() or None
