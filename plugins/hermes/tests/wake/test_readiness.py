"""Wake-up readiness: one judgment for apa-wake's loop and apa-headless' declaration. Hermes' plugin manager and
config are replaced by stand-in modules; nothing of Hermes is imported."""
import importlib.util
import pathlib
import sys
import types

import pytest

READINESS = pathlib.Path(__file__).resolve().parents[2] / "wake" / "readiness.py"


@pytest.fixture
def r():
    spec = importlib.util.spec_from_file_location("apa_wake_readiness_test", READINESS)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture
def hermes(monkeypatch):
    """Stand-in hermes_cli.plugins / hermes_cli.config; returns the state the test edits."""
    state = {"plugins": [], "config": {}}
    pkg = types.ModuleType("hermes_cli")
    plugins = types.ModuleType("hermes_cli.plugins")
    config = types.ModuleType("hermes_cli.config")
    plugins.get_plugin_manager = lambda: types.SimpleNamespace(list_plugins=lambda: state["plugins"])
    config.load_config_readonly = lambda: state["config"]
    for name, mod in {"hermes_cli": pkg, "hermes_cli.plugins": plugins, "hermes_cli.config": config}.items():
        monkeypatch.setitem(sys.modules, name, mod)
    for k in ("APA_WAKE_SESSION_KEY", "APA_URL", "APA_TOKEN"):
        monkeypatch.delenv(k, raising=False)
    return state


def ready(hermes, monkeypatch):
    hermes["plugins"] = [{"name": "apa-wake", "key": "apa-wake", "enabled": True, "error": None}]
    hermes["config"] = {"plugins": {"entries": {"apa-wake": {"allow_gateway_injection": True}}}}
    monkeypatch.setenv("APA_WAKE_SESSION_KEY", "agent:main")
    monkeypatch.setenv("APA_URL", "http://127.0.0.1:1")
    monkeypatch.setenv("APA_TOKEN", "t")


def test_everything_in_place_means_nothing_missing(r, hermes, monkeypatch):
    ready(hermes, monkeypatch)
    assert r.current() == []


@pytest.mark.parametrize("break_it, gap", [
    (lambda h, m: h.update(plugins=[]), "plugin_off"),
    (lambda h, m: h.update(plugins=[{"name": "apa-wake", "key": "apa-wake", "enabled": True, "error": "boom"}]), "plugin_off"),
    (lambda h, m: m.delenv("APA_WAKE_SESSION_KEY"), "no_session"),
    (lambda h, m: h.update(config={"plugins": {"entries": {"apa-wake": {"allow_gateway_injection": "yes"}}}}), "injection_denied"),
    (lambda h, m: m.delenv("APA_TOKEN"), "no_key"),
])
def test_each_missing_item_is_named(r, hermes, monkeypatch, break_it, gap):
    ready(hermes, monkeypatch)
    break_it(hermes, monkeypatch)
    assert r.current() == [gap]


def test_without_hermes_everything_is_missing_in_order(r, monkeypatch):
    for name in ("hermes_cli", "hermes_cli.plugins", "hermes_cli.config"):
        monkeypatch.setitem(sys.modules, name, None)  # import fails
    for k in ("APA_WAKE_SESSION_KEY", "APA_URL", "APA_TOKEN"):
        monkeypatch.delenv(k, raising=False)
    assert r.current() == ["plugin_off", "no_session", "injection_denied", "no_key"]


def test_apa_wake_itself_passes_enabled_and_its_key(r, hermes, monkeypatch):
    ready(hermes, monkeypatch)
    hermes["plugins"] = []
    hermes["config"] = {"plugins": {"entries": {"user/apa-wake": {"allow_gateway_injection": True}}}}
    assert r.current(enabled=True, key="user/apa-wake") == []


def test_texts_are_product_language_and_config_keys_only_in_how(r):
    for g in r.missing(list(r.GAPS)):
        assert g["zh"] and g["en"] and g["how"]["zh"] and g["how"]["en"]
        assert "APA_" not in g["zh"] + g["en"] and "allow_gateway_injection" not in g["zh"] + g["en"]


def test_in_gateway_is_hermes_live_gateway_injector(r, hermes, monkeypatch):
    """The process kind comes from Hermes itself (GatewayRunner publishes the injector), not from env or process names."""
    ready(hermes, monkeypatch)
    plugins = sys.modules["hermes_cli.plugins"]
    for live in (False, True):
        monkeypatch.setattr(plugins, "get_plugin_manager", lambda live=live: types.SimpleNamespace(
            has_gateway_message_injector=live, list_plugins=lambda: hermes["plugins"]))
        assert r.in_gateway() is live
        assert r.current() == []  # readiness (and so the declaration) does not depend on the process kind
    monkeypatch.setitem(sys.modules, "hermes_cli.plugins", None)
    assert r.in_gateway() is False
