"""Persona: `apa persona` read when a new session renders its prompt, then frozen by Hermes."""
from . import apa

SECTION_ID = "apa.persona"
MAX_CHARS = 2000  # persona is capped ~400 tokens by the kernel; headroom for the fixed preamble


def render(session_info):
    return apa.run(["persona"], session_info.get("session_id"))


def register(ctx):
    ctx.register_system_prompt_section(SECTION_ID, render, max_chars=MAX_CHARS)
