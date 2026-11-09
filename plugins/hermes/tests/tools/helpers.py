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
    turn, as gateway/session_policy.py:policy_scope does. Returns session(context_file, cwd), a context manager for one turn."""
    bound, cwd = contextvars.ContextVar("client_env", default=None), contextvars.ContextVar("cwd", default="")
    client_env = types.ModuleType("agent.session_client_env")
    client_env.bound_client_env = bound.get
    runtime_cwd = types.ModuleType("agent.runtime_cwd")
    runtime_cwd.scoped_session_cwd = cwd.get
    agent = types.ModuleType("agent")
    agent.session_client_env, agent.runtime_cwd = client_env, runtime_cwd
    for name, mod in (("agent", agent), ("agent.session_client_env", client_env), ("agent.runtime_cwd", runtime_cwd)):
        monkeypatch.setitem(sys.modules, name, mod)

    @contextlib.contextmanager
    def session(context_file=None, session_cwd=""):
        tokens = bound.set({"APA_RUN_CONTEXT": str(context_file)} if context_file else {}), cwd.set(str(session_cwd))
        try:
            yield
        finally:
            bound.reset(tokens[0])
            cwd.reset(tokens[1])

    return session
