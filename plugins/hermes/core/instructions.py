"""Standing instructions: `apa guide --template`, rendered once per new session."""
from . import apa

SECTION_ID = "apa.instructions"


def render(session_info):
    return apa.run(["guide", "--template"], session_info.get("session_id"))


def register(ctx):
    ctx.register_system_prompt_section(SECTION_ID, render)
