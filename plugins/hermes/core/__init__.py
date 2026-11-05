"""APA core plugin for Hermes: one submodule per capability, each exposing register(ctx)."""
from . import instructions, persona

_SUBMODULES = (instructions, persona)


def register(ctx):
    for mod in _SUBMODULES:
        mod.register(ctx)
