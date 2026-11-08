"""Read-only file access for read-tier runs: only the run directory and the working directory, never writes."""
import os

MAX_BYTES = 200_000


def _roots():
    return [os.path.realpath(os.getcwd())]


def _in_run_dir(real):
    """The kernel runs a task in <APA_HOME>/runs/<APA_RUN> but passes the child neither APA_HOME nor the dir,
    so match the layout: a path under .../runs/<this run's id>/. Sibling runs have another id and stay refused."""
    run = os.environ.get("APA_RUN")
    parts = real.split(os.sep)
    return bool(run) and any(parts[i] == "runs" and parts[i + 1] == run for i in range(len(parts) - 2))


def _resolve(path):
    """Real path (symlinks followed) inside the working directory or this run's directory, else ValueError."""
    if not path:
        raise ValueError("path is required")
    real = os.path.realpath(os.path.expanduser(path))
    if not (any(real == r or real.startswith(r + os.sep) for r in _roots()) or _in_run_dir(real)):
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
