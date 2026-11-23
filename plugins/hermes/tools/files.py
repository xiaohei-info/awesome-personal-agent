"""Read-only file access for read-tier runs: only this run's working and run directories, never writes.

Reads never truncate silently. A plain read (path only) is unchanged: up to MAX_BYTES, flagged. Every read also says how
big the file is, which bytes came back and where to go on (total_bytes, returned_bytes, offset, has_more, next_offset).
offset/limit page through any file; for the kernel's materials file, catalog lists each material (entries, time range,
bytes, fields, a sample) and material + since/until or from/to returns just that part. Pages stay under Hermes'
per-result threshold (100K chars), so nothing is spilled to a file the run cannot read."""
import datetime
import io
import json
import os
import re

from . import run_context

MAX_BYTES = 200_000  # plain read without paging arguments, as before
PAGE = 30_000  # default page when offset/limit/material is given
PAGE_MAX = 60_000


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


def _int(v, name, lo=0):
    if v is None or v == "":
        return None
    try:
        n = int(v)
    except (TypeError, ValueError):
        raise ValueError(f"{name} must be an integer")
    if n < lo:
        raise ValueError(f"{name} must be >= {lo}")
    return n


def _page_limit(limit):
    """(limit, note): limit defaults to PAGE and is clamped to PAGE_MAX, saying so."""
    n = _int(limit, "limit", 1)
    if n is None:
        return PAGE, None
    if n > PAGE_MAX:
        return PAGE_MAX, f"limit {n} 超过上限，按 {PAGE_MAX} 返回"
    return n, None


def _bytes_page(f, total, offset, limit, legacy=False):
    """One page of an open binary file (or BytesIO); the end never splits a UTF-8 character."""
    if offset > total:
        raise ValueError(f"offset {offset} is past the end of the file ({total} bytes)")
    f.seek(offset)
    chunk = f.read(limit + 1)  # one byte past the page tells whether the cut lands inside a character
    end = offset + min(limit, len(chunk))
    while offset < end < total and (chunk[end - offset] & 0xC0) == 0x80:
        end -= 1
    more = end < total
    out = {"ok": True, "total_bytes": total, "offset": offset, "returned_bytes": end - offset, "has_more": more,
           "next_offset": end if more else None}
    if legacy:
        out["truncated"] = more
    if more:
        out["notice"] = (f"未读完：本次返回第 {offset}–{end} 字节，共 {total} 字节，还剩 {total - end} 字节；"
                         f"用 offset={end} 接着读（每次 limit 不超过 {PAGE_MAX}），资料文件也可用 catalog / material 定向取")
    out["content"] = chunk[:end - offset].decode("utf-8", "replace")
    return out


def read_file(path, offset=None, limit=None, catalog=False, material=None, since=None, until=None, from_=None, to=None):
    try:
        real = _resolve(path)
        with open(real, "rb") as f:
            total = os.fstat(f.fileno()).st_size
            if catalog or material:
                return _materials(f.read(), catalog, material, since, until, _int(from_, "from"), _int(to, "to"), limit,
                                  _int(offset, "offset") or 0)
            if offset is None and limit is None:
                return _bytes_page(f, total, 0, MAX_BYTES, legacy=True)
            n, note = _page_limit(limit)
            out = _bytes_page(f, total, _int(offset, "offset") or 0, n)
    except (OSError, ValueError) as e:
        return {"ok": False, "error": str(e)}
    if note:
        out["limit_note"] = note
    return out


# ---- materials file ----

def _load_materials(data):
    try:
        doc = json.loads(data)
    except ValueError:
        doc = None
    if not isinstance(doc, dict) or not isinstance(doc.get("materials"), list):
        raise ValueError("not a materials file (no 'materials' list); read it with offset/limit")
    return doc


def _catalog_of(doc):
    """The kernel writes the catalog; files staged before it existed get names and sizes only."""
    if isinstance(doc.get("catalog"), list):
        return doc["catalog"]
    return [{"name": m.get("Name"), "bytes": len(str(m.get("Body", "")).encode())} for m in doc["materials"]]


def _materials(data, catalog, name, since, until, frm, to, limit, offset):
    doc = _load_materials(data)
    cat = _catalog_of(doc)
    if catalog and not name:
        return {"ok": True, "total_bytes": len(data), "materials_count": len(cat), "messages": len(doc.get("messages") or []),
                "usage": "material=<name> 取一段；可加 since/until（按该段 time_fields，since 含、until 不含）或 from/to（条号，to 不含）；"
                         "matched_from 是从 from 起符合条件的条数；返回 has_more 时用 next_from 接着取。整份文件也可 offset/limit 分页",
                "catalog": cat}
    mat = next((m for m in doc["materials"] if m.get("Name") == name), None)
    if mat is None:
        raise ValueError(f"no material named {name!r}; have: {', '.join(str(c.get('name')) for c in cat)}")
    n, note = _page_limit(limit)
    entry = next((c for c in cat if c.get("name") == name), {})
    raw = str(mat.get("Body", ""))
    try:
        body = json.loads(raw)
    except ValueError:
        body = None
    lst = _walk(body, entry.get("list")) if entry.get("list") and isinstance(body, dict) else None
    if lst is None:  # not a list of entries: the whole body, paged by bytes
        if since or until or frm is not None or to is not None:
            raise ValueError(f"material {name!r} has no entries to select; read it without since/until/from/to")
        b = raw.encode()
        out = _bytes_page(io.BytesIO(b), len(b), offset, n)
        if out["has_more"]:
            out["notice"] = f"这一段未读完：共 {len(b)} 字节；用 material={name} offset={out['next_offset']} 接着读"
        return {"ok": True, "material": name, **{k: v for k, v in out.items() if k != "ok"}}
    fields = entry.get("time_fields") or []
    if (since or until) and not fields:
        raise ValueError(f"material {name!r} has no time field; select with from/to")
    lo, hi = frm or 0, len(lst) if to is None else min(to, len(lst))
    idx = [i for i in range(lo, hi) if _in_window(_time_of(lst[i], fields), since, until)]
    picked, size, nxt = [], 0, None
    for i in idx:
        b = len(json.dumps(lst[i], ensure_ascii=False).encode())
        if picked and size + b > n:
            nxt = i
            break
        picked.append(i)
        size += b
    out = {"ok": True, "material": name, "list": entry.get("list"), "time_fields": fields, "entries_total": len(lst),
           "matched_from": len(idx), "returned": len(picked), "returned_bytes": size, "has_more": nxt is not None, "next_from": nxt}
    if note:
        out["limit_note"] = note
    if nxt is not None:
        rest = len(idx) - len(picked)
        out["notice"] = f"未取完：还有 {rest} 条符合条件；用 from={nxt}（since/until 不变）接着取"
    out["totals"] = entry.get("totals") or {}
    out["entries"] = [dict(lst[i], _n=i) for i in picked]
    others = {k: v for k, v in body.items() if k != name}
    if others and not frm:  # only with the first page, so paging does not repeat them
        out["other_fields"] = others
    elif others:
        out["other_fields_note"] = f"与条目并列的字段（{', '.join(others)}）只随 from=0 的第一页给出"
    return out


def _walk(body, path):
    """Entries at a catalog path such as 'activity[]', 'activity[].entries[]' or 'open_items.items[]'."""
    cur = [body]
    for seg in path.split("."):
        key, many = (seg[:-2], True) if seg.endswith("[]") else (seg, False)
        nxt = []
        for c in cur:
            v = c.get(key) if isinstance(c, dict) else None
            if many:
                nxt.extend(v if isinstance(v, list) else [])
            elif v is not None:
                nxt.append(v)
        cur = nxt
    return cur


def _time_of(e, fields):
    for f in fields:
        v = e.get(f) if isinstance(e, dict) else None
        if isinstance(v, str) and v:
            return v
    return ""


_DATE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def _parse(s):
    s = re.sub(r"(\.\d{6})\d+", r"\1", s.replace("Z", "+00:00"))  # Go writes up to nanoseconds
    return datetime.datetime.fromisoformat(s)


def _before(a, b):
    """a < b for a date or an RFC3339 time on either side; a date compares by day."""
    if _DATE.match(a) or _DATE.match(b):
        return a[:10] < b[:10]
    try:
        return _parse(a) < _parse(b)
    except ValueError:
        return a < b


def _in_window(t, since, until):
    if not since and not until:
        return True
    if not t:
        return False
    return (not since or not _before(t, since)) and (not until or _before(t, until))


def list_dir(path):
    try:
        real = _resolve(path or ".")
        entries = sorted(e + "/" if os.path.isdir(os.path.join(real, e)) else e for e in os.listdir(real))
    except (OSError, ValueError) as e:
        return {"ok": False, "error": str(e)}
    return {"ok": True, "entries": entries}
