"""Read-only file access for read-tier runs: only the run directory and the working directory, never writes."""
import os

MAX_BYTES = 200_000


def _roots():
    roots = [os.getcwd()]
    home, run = os.environ.get("APA_HOME"), os.environ.get("APA_RUN")
    if home and run:
        roots.append(os.path.join(home, "runs", run))
    return [os.path.realpath(r) for r in roots]


def _resolve(path):
    """Real path (symlinks followed) inside a root, else ValueError."""
    if not path:
        raise ValueError("path is required")
    real = os.path.realpath(os.path.expanduser(path))
    if not any(real == r or real.startswith(r + os.sep) for r in _roots()):
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
