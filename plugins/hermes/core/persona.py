"""Persona: `apa persona` read when a new session renders its prompt, then frozen by Hermes."""
from . import apa

SECTION_ID = "apa.persona"
MAX_CHARS = 4000  # Hermes ceiling (A.5); a kernel-valid persona (~400 tokens + declaration) can exceed 2000 chars and Hermes drops over-budget sections


def render(session_info):
    return apa.run(["persona"], session_info.get("session_id"))


def register(ctx):
    ctx.register_system_prompt_section(SECTION_ID, render, max_chars=MAX_CHARS)
