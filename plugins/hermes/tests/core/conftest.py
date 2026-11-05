import importlib.util
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[4]
CORE = ROOT / "plugins" / "hermes" / "core"


def load_core():
    """Import plugins/hermes/core as package 'apa_core' (it is not on sys.path by design)."""
    for k in [k for k in sys.modules if k == "apa_core" or k.startswith("apa_core.")]:
        del sys.modules[k]  # fresh import each time so submodule attributes are set
    spec = importlib.util.spec_from_file_location(
        "apa_core", CORE / "__init__.py", submodule_search_locations=[str(CORE)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_core"] = mod
    spec.loader.exec_module(mod)
    return mod
