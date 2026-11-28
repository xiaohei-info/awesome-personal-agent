"""Session identity on every `apa` call of a session: the session's terminal children get APA_SESSION and APA_HARNESS
through Hermes' ctx.register_session_env, so what the model runs in a terminal carries the same identity as the plugins'
own calls. Identity only: the kernel uses it to tell which session wrote what ("this session is tracking"), never to
decide what the session may do."""

HARNESS = "hermes"
NAMES = ("APA_SESSION", "APA_HARNESS")


def resolve(session_id):
    return {"APA_SESSION": session_id, "APA_HARNESS": HARNESS} if session_id else None


def register(ctx):
    ctx.register_session_env(list(NAMES), resolve)
