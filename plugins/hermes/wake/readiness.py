"""Whether apa-wake can wake the main session here, and what is missing if not.

The only place that decides it: apa-wake's register() starts its loop on it, and apa-headless declares
main_session.wake / wake_missing from it (appendix A.2). Read-only: it reads Hermes' plugin list, config and the
environment, and changes nothing. Texts are what the owner sees in Web; config keys appear only in `how`.
"""
import os

NAME = "apa-wake"

# Order is the order the owner fixes them in.
GAPS = {
    "plugin_off": {
        "zh": "Hermes 里还没启用唤醒插件", "en": "The wake-up plugin is not enabled in Hermes",
        "how": {"zh": "运行 hermes plugins enable apa-wake，再重启 Hermes 的 gateway",
                "en": "Run hermes plugins enable apa-wake, then restart the Hermes gateway"}},
    "no_session": {
        "zh": "还没指定要唤醒哪个对话", "en": "No chat is chosen to wake",
        "how": {"zh": "在 Hermes 的 .env 里设 APA_WAKE_SESSION_KEY=<要唤醒的会话>，再重启 gateway",
                "en": "Set APA_WAKE_SESSION_KEY=<session to wake> in Hermes' .env, then restart the gateway"}},
    "injection_denied": {
        "zh": "Hermes 还没允许插件在对话里开始一轮", "en": "Hermes does not let the plugin start a turn in the chat yet",
        "how": {"zh": "在 Hermes 的 config.yaml 里加 plugins.entries.apa-wake.allow_gateway_injection: true，再重启 gateway",
                "en": "Add plugins.entries.apa-wake.allow_gateway_injection: true to Hermes' config.yaml, then restart the gateway"}},
    "no_key": {
        "zh": "唤醒插件还没有连 APA 的钥匙", "en": "The wake-up plugin has no key for APA",
        "how": {"zh": "在 Hermes 的 .env 里设 APA_URL 与 APA_TOKEN（APA 网页：设置 › Agent › 高级 › 钥匙），再重启 gateway",
                "en": "Set APA_URL and APA_TOKEN in Hermes' .env (APA Web: Settings › Agents › Advanced › Keys), then restart the gateway"}},
}


def gaps(enabled, session_key, injection_allowed, has_key):
    """Missing items, in GAPS order; empty means waking works."""
    have = {"plugin_off": enabled, "no_session": bool(session_key), "injection_denied": injection_allowed, "no_key": has_key}
    return [code for code in GAPS if not have[code]]


def _plugin():
    """(enabled, registry key) of apa-wake as Hermes' plugin manager sees it."""
    try:
        from hermes_cli.plugins import get_plugin_manager
        for p in get_plugin_manager().list_plugins():
            if p.get("name") == NAME or p.get("key") == NAME:
                return bool(p.get("enabled")) and not p.get("error"), p.get("key") or NAME
    except Exception:  # noqa: BLE001 - no plugin runtime, or it changed shape: not enabled is the safe answer
        pass
    return False, NAME


def _injection_allowed(key):
    """Hermes' own condition (plugins.entries.<key>.allow_gateway_injection is True); unreadable config means no."""
    try:
        from hermes_cli.config import load_config_readonly
        entries = ((load_config_readonly() or {}).get("plugins") or {}).get("entries") or {}
        entry = entries.get(key) or entries.get(NAME) or {}
        return isinstance(entry, dict) and entry.get("allow_gateway_injection") is True
    except Exception:  # noqa: BLE001 - Hermes fails closed here too
        return False


def current(enabled=None, key=None):
    """Missing items for this process. apa-wake passes enabled=True and its own key from register()."""
    if enabled is None:
        enabled, key = _plugin()
    key = key or NAME
    env = os.environ
    return gaps(enabled, env.get("APA_WAKE_SESSION_KEY", ""), _injection_allowed(key),
                bool(env.get("APA_URL") and env.get("APA_TOKEN")))


def missing(codes):
    """The declaration's main_session.wake_missing for these codes."""
    return [GAPS[c] for c in codes]
