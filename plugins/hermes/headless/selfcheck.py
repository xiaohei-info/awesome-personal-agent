#!/usr/bin/env python3
"""Self-check for `apa doctor` (appendix A.1): read-only, prints one JSON object, touches nothing in Hermes' config.

reports_session_kind is true because every `apa` call this plugin set makes goes through tools/run_context.apa_env,
which sets APA_SESSION_KIND from the session (direct / group / unknown). `apa doctor` runs this on the APA host with the
declaration's `selfcheck.command`; the declaration field itself is accepted by the kernel in the doctor node."""
import json
import pathlib

PLUGINS = pathlib.Path(__file__).resolve().parents[1]
VERSION = "0.2.0"


def report():
    hooks = sorted(p.parent.name for p in PLUGINS.glob("*/plugin.yaml"))
    return {"plugin_version": VERSION, "hooks": [{"name": n, "installed": True} for n in hooks],
            "reports_session_kind": True, "overlaps": []}


if __name__ == "__main__":
    print(json.dumps(report()))
