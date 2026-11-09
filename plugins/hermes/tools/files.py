"""Read-only file access for read-tier runs: only this run's working and run directories, never writes."""
import os

from . import run_context

MAX_BYTES = 200_000


def _resolve(path):
    """Real path (symlinks followed) inside this run's directories, else ValueError. Relative paths start at its working directory."""
    if not path:
        raise ValueError("path is required")
    ctx = run_context.load()
    work, run = os.path.realpath(ctx["work_dir"]), os.path.realpath(ctx["run_dir"])
    real = os.path.realpath(os.path.join(work, os.path.expanduser(path)))
    if not any(real == r or real.startswith(r + os.sep) for r in (work, run)):
        raise ValueError(f"{path} is outside the run directory and the working directory")
    return real


def read_file(path):
    try:
        real = _resolve(path)
        with open(real, "rb") as f:
            data = f.read(MAX_BYTES + 1)
    except (OSError, ValueError) as e:
        return {"ok": False, "error": str(e)}
    return {"ok": True, "content": data[:MAX_BYTES].decode("utf-8", "replace"), "truncated": len(data) > MAX_BYTES}


def list_dir(path):
    try:
        real = _resolve(path or ".")
        entries = sorted(e + "/" if os.path.isdir(os.path.join(real, e)) else e for e in os.listdir(real))
    except (OSError, ValueError) as e:
        return {"ok": False, "error": str(e)}
    return {"ok": True, "entries": entries}
