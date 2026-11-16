import importlib.util
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[4]
HEADLESS = ROOT / "plugins" / "hermes" / "headless"


def load_headless():
    """Import plugins/hermes/headless as package 'apa_headless' (not on sys.path by design)."""
    for k in [k for k in sys.modules if k == "apa_headless" or k.startswith("apa_headless.")]:
        del sys.modules[k]
    spec = importlib.util.spec_from_file_location("apa_headless", HEADLESS / "__init__.py",
                                                  submodule_search_locations=[str(HEADLESS)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["apa_headless"] = mod
    spec.loader.exec_module(mod)
    return mod


def kernel_argv(decl, prompt, tier, session_id=None):
    """The argv the kernel builds (background.launch / resume.launch): command, output, access, then resume."""
    h = decl["headless"]
    argv = [a.replace("{prompt}", prompt).replace("{run_dir}", "/run") for a in h["command"]]
    argv += h["output"] + h["access"][tier]
    if session_id:
        argv += [a.replace("{session_id}", session_id) for a in h["resume"]]
    return argv
