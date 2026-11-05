import importlib.util
import pathlib
import sys

import pytest

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
WAKE = HERE.parents[1] / "wake"

from kernel_stub import PLUGIN_TOKEN, Kernel  # noqa: E402


def load_wake():
    """Import plugins/hermes/wake as package 'apa_wake' (fresh state each time)."""
    for k in [k for k in sys.modules if k == "apa_wake" or k.startswith("apa_wake.")]:
        del sys.modules[k]
    spec = importlib.util.spec_from_file_location("apa_wake", WAKE / "__init__.py", submodule_search_locations=[str(WAKE)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_wake"] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture
def kernel():
    k = Kernel()
    yield k
    k.stop()


@pytest.fixture
def wake(kernel, monkeypatch):
    monkeypatch.setenv("APA_URL", kernel.url)
    monkeypatch.setenv("APA_TOKEN", PLUGIN_TOKEN)
    monkeypatch.delenv("APA_WAKE_SESSION_KEY", raising=False)
    return load_wake()
