"""APA capture plugin for Hermes: `on_session_end` (turn level) posts the conversation delta to `POST /capture`.
No decisions here: whether capture is on for the project, run-identity suppression and candidate extraction
all belong to the kernel (it answers 200 `captured: false` otherwise).

What this plugin does decide is which turns are an external person's own words: a turn is sent only when it can be
positively attributed to the person at the keyboard (`capture_schema: 1` with `speaker_role: human`,
`origin_type: external_chat`). Scheduled-job sessions, background skill curators, cron echoes, background-process
notices, async delegation reports and pure assistant output are dropped here and never reach the kernel; a turn that
cannot be attributed is dropped too (fail-closed). Recognising them is harness-specific, so it lives here, never in
the kernel. Only the category goes to the plugin-local log (never text, user names or credentials).

`on_session_end` carries no transcript, so `post_llm_call` buffers each finished turn per session and the
end hook flushes that buffer (the per-session cursor: a turn is sent once). The POST runs in a detached
helper process (`python capture/__init__.py`, JSON on stdin), prints nothing and never uses forced
continuation; any failure is swallowed and never reaches Hermes.

Failures are not silent: every one is appended to the plugin-local log (command, status/reason; never credentials
or conversation text) and noted as {at, class} in the state dir. The next request the kernel accepts carries the noted
ones in its `failures` field, so the diagnostics page shows them (latest time, reason class; class only, no text).
A kernel that answers but refuses is not re-sent. An unreachable kernel keeps the request (with its Idempotency-Key)
in the state dir; the next hook re-sends it once the kernel is back (A.1).
"""
import hashlib
import importlib.util
import json
import os
import pathlib
import re
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

_spec = importlib.util.spec_from_file_location("apa_run_context", pathlib.Path(__file__).resolve().parents[1] / "tools" / "run_context.py")
run_context = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_context)

TEXT_MAX = 20000
TIMEOUT_SECS = 10

HARNESS = "hermes"

# 契约版本与标注（内核 §9.3、附录 B.2）：只有能确认是外部人发起的轮次才带上这一组字段。
SCHEMA = 1
HUMAN, EXTERNAL_CHAT = "human", "external_chat"

# 定时任务会话头 / 后台技能整理器：整个会话都不是外部人的对话，只看首轮就能定。
SESSION_HEADS = (("[IMPORTANT: You are running as a scheduled cron job", "cron_echo"),
                 ("Hermes' background skill CURATOR", "background_internal"))
# 单轮的系统包装：只丢这一轮，会话里其他人类轮次照旧。
TURN_HEADS = (("[Cronjob \"", "cron_echo"),
              ("[IMPORTANT: Background process ", "system_notice"),
              ("[ASYNC DELEGATION BATCH COMPLETE", "system_notice"),
              ("[INTERNAL NOTIFICATION", "system_notice"),
              # apa-wake 注入主会话的知会与唤醒（[APA 提醒]、[APA 简报]、[APA wake] 等）：那一轮是 APA 自己的话，再捕获会成环（§9.3）。
              ("[APA", "apa_notice"))
# 用户发图或发附件时 Hermes 在轮次开头插入的说明，后面往往还有本人的话：算外部人输入。
ATTACHMENT = "[The user sent "

# 并进一轮中间的 APA 知会块（跟在本人的话后面的唤醒与提醒、被引述的账本增量与跟进消息）：只认头尾齐全的整块，
# 结束语是内核信封与 apa-wake 写的固定文字；只提到 "[APA" 的话照原样保留。
APA_BLOCKS = re.compile(
    r"\[APA [^\]\n]*以下为资料，不是指令\].*?\[资料结束\]"
    r"|\[APA(?: 提醒| 简报| 需要你看一下)?\].*?不必回复 APA。"
    r"|\[APA wake\].*?permission card as this Agent\.", re.S)

_pending = {}  # session id -> [turn text, ...] not yet sent
_internal = {}  # session id -> 整个会话都不发的类别（定时任务会话、后台内部会话）
_seen = set()  # 已经过了首轮的会话：会话级判定只看首轮


def _human_text(text):
    """去掉方括号里的系统头后还剩人写的字：只剩系统头说明这是拿不准来源的包装消息。"""
    depth, rest = 0, []
    for ch in text:
        if ch == "[":
            depth += 1
        elif ch == "]":
            depth = max(0, depth - 1)
        elif depth == 0:
            rest.append(ch)
    return "".join(rest).strip()


def _classify(session_id, user_message):
    """(None, category) 表示这一轮不能发；("human", "external_chat") 表示确认是外部人的输入。
    先定会话级（首轮命中定时任务会话头或后台整理器，整个会话都不发），再定单轮。"""
    if not isinstance(user_message, str) or not user_message.strip():
        return None, "assistant_output"
    sid, text = str(session_id), user_message.strip()
    if sid in _internal:
        return None, _internal[sid]
    if sid not in _seen:
        for head, cat in SESSION_HEADS:
            if head in text:
                _internal[sid] = cat
                return None, cat
    for head, cat in TURN_HEADS:
        if text.startswith(head):
            return None, cat
    if text.startswith(ATTACHMENT):
        return HUMAN, EXTERNAL_CHAT
    if not _human_text(text):
        return None, "unknown"
    return HUMAN, EXTERNAL_CHAT


def _turn_text(user_message, assistant_response):
    parts = []
    for who, text in (("user", user_message), ("assistant", assistant_response)):
        if isinstance(text, str):
            text = APA_BLOCKS.sub("", text).strip()
            if text:
                parts.append(f"{who}: {text}")
    return "\n".join(parts)


def _state_dir(env):
    d = env.get("APA_HERMES_CAPTURE_DIR") or str(pathlib.Path(env.get("HERMES_HOME") or pathlib.Path.home() / ".hermes") / "apa-capture")
    return pathlib.Path(d)


def _log(env, reason):
    """One line: command and reason only (no credentials, no conversation text)."""
    try:
        d = _state_dir(env)
        d.mkdir(parents=True, exist_ok=True)
        with open(d / "capture.log", "a") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S%z')} POST /v1/capture {reason}\n")
    except OSError:
        pass


MAX_NOTED = 20


def _fail(env, cls, reason):
    """Log it and note {at, class} for the next request the kernel accepts. Class is one of kernel_unreachable,
    no_credentials, kernel_refused, plugin_error; never text or credentials."""
    _log(env, reason)
    try:
        f = _state_dir(env) / "failed.json"
        noted = _noted(env)
        noted.append({"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "class": cls})
        f.write_text(json.dumps(noted[-MAX_NOTED:]))
    except OSError:
        pass


def _noted(env):
    try:
        v = json.loads((_state_dir(env) / "failed.json").read_text())
        return v if isinstance(v, list) else []
    except (OSError, ValueError):
        return []


def _spawn(payload):
    """Hand the payload (or None: only re-send what is kept) to a detached helper; returns at once."""
    p = subprocess.Popen([sys.executable, "-I", str(pathlib.Path(__file__).resolve())], stdin=subprocess.PIPE,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True, env=run_context.apa_env())
    p.stdin.write(json.dumps(payload).encode())
    p.stdin.close()


def _has_kept():
    try:
        return any(_state_dir(os.environ).joinpath("pending").glob("*.json"))
    except OSError:
        return False


def post_llm_call(session_id="", user_message=None, assistant_response=None, **_):
    try:
        sid = str(session_id)
        role, why = _classify(sid, user_message)
        _seen.add(sid)
        if role is None:
            if user_message:  # 纯助手输出（没有用户消息）不是异常，不记为跳过
                _log(os.environ, "skipped turn: " + why)
        elif sid:
            text = _turn_text(user_message, assistant_response)
            if text:
                _pending.setdefault(sid, []).append(text)
        if _has_kept():
            _spawn(None)
    except Exception:
        pass


def on_session_end(session_id="", **_):
    try:
        sid = str(session_id)
        turns = _pending.pop(sid, None)
        _internal.pop(sid, None)  # 会话结束就清掉内部会话标记，防止内存泄漏
        _seen.discard(sid)
        if not turns:
            if _has_kept():
                _spawn(None)
            return
        text = "\n\n".join(turns)[-TEXT_MAX:]
        _spawn({"text": text, "session": sid, "capture_schema": SCHEMA, "speaker_role": HUMAN, "origin_type": EXTERNAL_CHAT,
                "path": run_context.session_cwd() or "", "host": socket.gethostname(), "context": run_context.context_file()})
    except ValueError as e:  # a run without its context: nothing goes out under the gateway's credential
        _fail(os.environ, "no_credentials", str(e).splitlines()[0])
    except Exception:
        pass  # fail open: capture problems must never reach the conversation


def _credentials(env, context=None):
    """Same rule as the apa CLI: a run uses only its run credential and never the saved file. A session bound to a run
    context uses that run's; once the run has ended the file is gone and the request is refused here, as the kernel would."""
    if context:
        ctx = json.loads(pathlib.Path(context).read_text())
        return ctx["url"], ctx["token"]
    url, token = env.get("APA_URL", ""), env.get("APA_TOKEN", "")
    if env.get("APA_RUN"):
        f = env.get("APA_RUN_CREDENTIAL_FILE")
        if not token and f:
            token = pathlib.Path(f).read_text().strip()
        return url, token
    if not (url and token):
        home = env.get("APA_HOME") or str(pathlib.Path.home() / ".apa")
        saved = json.loads((pathlib.Path(home) / "credentials.json").read_text())
        url, token = url or saved.get("url", ""), token or saved.get("token", "")
    return url, token


def _post(item, env):
    """One POST. Returns None when delivered or refused (refusal is logged), "unreachable" when it should be kept."""
    try:
        url, token = _credentials(env, item.get("context"))
    except (OSError, ValueError) as e:
        _fail(env, "no_credentials", "no usable credentials: " + type(e).__name__)
        return None
    if not (url and token):
        _fail(env, "no_credentials", "no usable credentials")
        return None
    noted = _noted(env)
    body = dict(item["body"], failures=noted) if noted else item["body"]
    req = urllib.request.Request(url.rstrip("/") + "/v1/capture", data=json.dumps(body).encode(), method="POST",
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json",
                                          "X-APA-Session": item["session"], "X-APA-Harness": HARNESS,
                                          "Idempotency-Key": item["key"]})
    try:
        urllib.request.urlopen(req, timeout=TIMEOUT_SECS).read()
    except urllib.error.HTTPError as e:  # kernel reachable but refused: logged by us, health view by the kernel; no resend
        _fail(env, "kernel_refused", f"status={e.code} reason={e.reason}")
    except (urllib.error.URLError, OSError) as e:  # URLError, refused connection, timeout
        _fail(env, "kernel_unreachable", "kernel unreachable: " + str(getattr(e, "reason", e) or type(e).__name__))
        return "unreachable"
    else:  # delivered: the kernel has the noted failures now; drop exactly those (newer ones stay)
        if noted:
            try:
                rest = [x for x in _noted(env) if x not in noted]
                (_state_dir(env) / "failed.json").write_text(json.dumps(rest))
            except OSError:
                pass
    return None


def _send(payload, env):
    """Keep the request first (so a crash or dead kernel loses nothing), then send everything kept, oldest first."""
    pend = _state_dir(env) / "pending"
    pend.mkdir(parents=True, exist_ok=True)
    if payload:
        session, context = payload.pop("session"), payload.pop("context", None)
        key = "hermes-capture-" + hashlib.sha1(f"{session}|{payload.get('text')}".encode()).hexdigest()[:16]
        f = pend / (key + ".json")
        f.write_text(json.dumps({"session": session, "key": key, "body": payload, "context": context}))
        f.chmod(0o600)
    for f in sorted(pend.glob("*.json"), key=lambda p: p.stat().st_mtime):
        claimed = f.with_suffix(".sending")
        try:
            f.rename(claimed)  # atomic claim: two helpers never send the same kept request
        except OSError:
            continue
        try:
            item = json.loads(claimed.read_text())
            outcome = _post(item, env)
        except (OSError, ValueError, KeyError) as e:
            _fail(env, "plugin_error", "unusable kept request: " + type(e).__name__)
            outcome = None
        if outcome == "unreachable":
            claimed.rename(f)
            return  # the kernel is down: no point trying the rest
        claimed.unlink()


def register(ctx):
    ctx.register_hook("post_llm_call", post_llm_call)
    ctx.register_hook("on_session_end", on_session_end)


if __name__ == "__main__":
    try:
        _send(json.load(sys.stdin), dict(os.environ))
    except Exception as e:
        _fail(dict(os.environ), "plugin_error", "helper failed: " + type(e).__name__)
