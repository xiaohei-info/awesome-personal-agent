"""Which APA run a tool or hook call belongs to.

On Hermes' one-gateway runtime (v0.23.0+) `hermes chat` is a thin client: tools and hooks run in the shared gateway
process, whose cwd and environment are not this run's (a gateway started by a run's client even keeps that run's APA_RUN
and APA_TOKEN for life). The only per-session value Hermes carries over is the client's allowlisted environment, frozen
with the session and bound to its turns in a ContextVar. The kernel puts one name there, APA_RUN_CONTEXT: the path of the
0600 file it wrote at launch with this run's id, credential and directories, removed when the run ends (design §7.7).

That name only travels if the profile allows it. PATH always travels, so the kernel also appends a marker entry to the
run's PATH: a session whose client PATH has the marker but no context is a run with the line missing. It gets an error,
never the gateway's own credential. A session without the marker cannot be told from an ordinary one and stays as it is.
"""
import json
import os

VAR = "APA_RUN_CONTEXT"
RUN_MARK = ".apa-run"  # kernel/outcall/proc.RunMark
LINE = f"terminal:\n  client_env_passthrough: [{VAR}]"
SETUP = f"add this to the Hermes profile's config.yaml:\n{LINE}"


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


def _marked(path):
    return any(os.path.basename(p) == RUN_MARK for p in (path or "").split(os.pathsep))


def _missing_in_run():
    """True when the kernel launched this session but its run context did not arrive. Reads the session's PATH,
    not the gateway's: a gateway started by a run's client keeps that run's marker for life."""
    m = _client_env()
    return bool(m) and not context_file() and _marked((m.bound_client_env() or {}).get("PATH"))


def load():
    """This run's context {run, url, token, project?, run_dir, work_dir}; ValueError says why there is none, nothing is guessed."""
    if _client_env() is None:
        raise ValueError("no run context: this Hermes has no per-session client environment (it needs v0.23.0 or later)")
    path = context_file()
    if _missing_in_run():
        raise ValueError(f"no run context: this APA run's context did not reach Hermes; {SETUP}")
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


# Session surfaces that are the user's own terminal (gateway/session_context.py binds HERMES_SESSION_PLATFORM for
# gateway sessions and HERMES_SESSION_SOURCE for CLI/TUI; both empty is an in-process `hermes chat`).
_TERMINAL = frozenset({"", "cli", "tui", "local"})
# Hermes surfaces that are neither the user's terminal nor a chat with a stated type: who speaks there is not ours to say.
_NOT_A_CHAT = frozenset({"api_server", "codex", "desktop", "gateway", "kanban", "msgraph_webhook", "tool", "webhook"})
_GROUP_CHAT = frozenset({"group", "channel", "thread"})


def session_kind():
    """direct | group | unknown for the session this call belongs to (appendix A.5, SPIKE-SESSION-KIND). Read from
    Hermes' session binding, never from the model. A terminal session is one person at their own keyboard: direct.
    A gateway session is what the platform says: a DM is direct, a group/channel/thread is group, and anything the
    binding does not state (other surfaces such as api_server/webhook, a DM without a sender, an unreadable
    binding) is unknown, so words in it are not accepted as the owner's."""
    try:
        from gateway.session_context import get_session_env
    except ImportError:
        return "unknown"
    try:
        platform = (get_session_env("HERMES_SESSION_PLATFORM", "") or "").strip().lower()
        source = (get_session_env("HERMES_SESSION_SOURCE", "") or "").strip().lower()
        if platform in _TERMINAL and source in _TERMINAL:
            return "direct"
        if platform in _NOT_A_CHAT or source in _NOT_A_CHAT:
            return "unknown"
        chat_type = (get_session_env("HERMES_SESSION_CHAT_TYPE", "") or "").strip().lower()
        if chat_type in _GROUP_CHAT:
            return "group"
        if chat_type == "dm" and (get_session_env("HERMES_SESSION_USER_ID", "") or "").strip():
            return "direct"
    except Exception:  # noqa: BLE001 - Hermes' binding layer is not ours; not knowing is the safe answer
        pass
    return "unknown"


def gateway_env(**extra):
    """The gateway's environment without what it inherited from the run whose client started it."""
    env = dict(os.environ)
    env.pop("APA_SESSION_KIND", None)  # a session's kind is read from that session, never inherited from the gateway
    if _client_env() is not None:
        env.pop(VAR, None)
        if env.get("APA_RUN"):  # token and project came with it
            for k in ("APA_RUN", "APA_TOKEN", "APA_PROJECT", "APA_RUN_CREDENTIAL_FILE"):
                env.pop(k, None)
        if "PATH" in env:
            env["PATH"] = os.pathsep.join(p for p in env["PATH"].split(os.pathsep) if os.path.basename(p) != RUN_MARK)
    env.update(extra)
    return env


def apa_env(**extra):
    """Environment for an `apa` subprocess: this session's run identity, never the one the gateway inherited.
    The CLI reads the rest from the context file; a session with no run uses the gateway's own credentials.
    ValueError for a run whose context did not arrive: the caller must not run `apa` at all."""
    env = gateway_env(APA_HARNESS="hermes", APA_SESSION_KIND=session_kind(), **extra)
    path = context_file()
    if path:
        env[VAR] = path
    elif _missing_in_run():
        raise ValueError(f"this APA run's context did not reach Hermes, so `apa` was not run (it would have used the gateway's own credential); {SETUP}")
    return env


def require_passthrough():
    """ValueError unless this profile lets the run context reach the gateway. Declaring without it would let runs start
    that can only ever be refused. Builds without the one-gateway runtime need no such line."""
    m = _client_env()
    if m is None:
        return
    try:
        from hermes_cli.config import get_config_path
        from hermes_cli.config_effective import load_user_config_effective
        names = m.configured_names(load_user_config_effective(get_config_path(), fail_closed=True))
    except Exception as e:  # Hermes' config layer raises parser-specific errors: anything here means "not confirmed"
        raise ValueError(f"cannot confirm the Hermes profile allows {VAR} ({type(e).__name__}); it must contain:\n{LINE}") from None
    if VAR not in names:
        raise ValueError(f"the Hermes profile does not pass {VAR} to the gateway, so runs would not get their identity; {SETUP}")


def session_cwd():
    """The session's frozen working directory (a run's is its work_dir), or None when it has none; never the gateway's."""
    if _client_env() is None:
        return os.getcwd()
    from agent.runtime_cwd import scoped_session_cwd
    return scoped_session_cwd() or None
