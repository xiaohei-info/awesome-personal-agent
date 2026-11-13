import contextlib
import contextvars
import importlib.util
import pathlib
import sys
import types

ROOT = pathlib.Path(__file__).resolve().parents[4]
TOOLS = ROOT / "plugins" / "hermes" / "tools"


def load_tools():
    """Import plugins/hermes/tools as package 'apa_tools' (it is not on sys.path by design)."""
    for k in [k for k in sys.modules if k == "apa_tools" or k.startswith("apa_tools.")]:
        del sys.modules[k]  # fresh import each time so submodule attributes are set
    spec = importlib.util.spec_from_file_location(
        "apa_tools", TOOLS / "__init__.py", submodule_search_locations=[str(TOOLS)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_tools"] = mod
    spec.loader.exec_module(mod)
    return mod


def fake_gateway(monkeypatch):
    """Stand-in for Hermes' per-session scope (agent.session_client_env, agent.runtime_cwd, v0.23.0): ContextVars set for a
    turn, as gateway/session_policy.py:policy_scope does. Returns session(context_file, cwd, client_path), a context manager
    for one turn. session.config is the profile's config (an exception raises when it is read)."""
    bound, cwd = contextvars.ContextVar("client_env", default=None), contextvars.ContextVar("cwd", default="")
    client_env = types.ModuleType("agent.session_client_env")
    client_env.bound_client_env = bound.get

    def configured_names(config):  # same reading as Hermes': terminal.client_env_passthrough, a list of strings
        terminal = config.get("terminal") if isinstance(config, dict) else None
        raw = terminal.get("client_env_passthrough") if isinstance(terminal, dict) else None
        return frozenset(n.strip() for n in raw if isinstance(n, str) and n.strip()) if isinstance(raw, list) else frozenset()

    client_env.configured_names = configured_names
    runtime_cwd = types.ModuleType("agent.runtime_cwd")
    runtime_cwd.scoped_session_cwd = cwd.get
    agent = types.ModuleType("agent")
    agent.session_client_env, agent.runtime_cwd = client_env, runtime_cwd

    @contextlib.contextmanager
    def session(context_file=None, session_cwd="", client_path=None):
        env = {"APA_RUN_CONTEXT": str(context_file)} if context_file else {}
        if client_path is not None:
            env["PATH"] = client_path
        tokens = bound.set(env), cwd.set(str(session_cwd))
        try:
            yield
        finally:
            bound.reset(tokens[0])
            cwd.reset(tokens[1])

    session.config = {"terminal": {"client_env_passthrough": ["APA_RUN_CONTEXT"]}}

    def load_config(path=None, *, fail_closed=False):
        assert fail_closed  # a torn config must not pass as "last known good"
        if isinstance(session.config, Exception):
            raise session.config
        return session.config

    hermes_config = types.ModuleType("hermes_cli.config")
    hermes_config.get_config_path = lambda: "config.yaml"
    config_effective = types.ModuleType("hermes_cli.config_effective")
    config_effective.load_user_config_effective = load_config
    hermes_cli = types.ModuleType("hermes_cli")
    hermes_cli.config, hermes_cli.config_effective = hermes_config, config_effective
    for name, mod in (("agent", agent), ("agent.session_client_env", client_env), ("agent.runtime_cwd", runtime_cwd),
                      ("hermes_cli", hermes_cli), ("hermes_cli.config", hermes_config), ("hermes_cli.config_effective", config_effective)):
        monkeypatch.setitem(sys.modules, name, mod)
    return session


def fake_session_context(monkeypatch, **bound):
    """Stand-in for Hermes' gateway.session_context.get_session_env: the HERMES_SESSION_* values bound for one turn
    (keys without the prefix, e.g. platform='telegram', chat_type='dm'). No keys = a terminal with nothing bound."""
    mod = types.ModuleType("gateway.session_context")
    mod.get_session_env = lambda name, default="": bound.get(name.removeprefix("HERMES_SESSION_").lower(), default)
    gateway = types.ModuleType("gateway")
    gateway.session_context = mod
    monkeypatch.setitem(sys.modules, "gateway", gateway)
    monkeypatch.setitem(sys.modules, "gateway.session_context", mod)
