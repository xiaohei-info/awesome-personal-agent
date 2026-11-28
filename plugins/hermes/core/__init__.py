"""APA core plugin for Hermes: one submodule per capability, each exposing register(ctx)."""
from . import instructions, session_env

_SUBMODULES = (instructions, session_env)


def register(ctx):
    for mod in _SUBMODULES:
        mod.register(ctx)
