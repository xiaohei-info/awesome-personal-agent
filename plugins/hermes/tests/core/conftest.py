import importlib.util
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[4]
CORE = ROOT / "plugins" / "hermes" / "core"


def load_core():
    """Import plugins/hermes/core as package 'apa_core' (it is not on sys.path by design)."""
    spec = importlib.util.spec_from_file_location(
        "apa_core", CORE / "__init__.py", submodule_search_locations=[str(CORE)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_core"] = mod
    spec.loader.exec_module(mod)
    return mod
