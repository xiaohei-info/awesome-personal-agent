import importlib.util
import pathlib
import sys

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
