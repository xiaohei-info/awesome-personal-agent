import { useEffect, useId, useRef, useState } from "react";
import type { ApiClient } from "./api";
import { useSession } from "./session";
import { useLang, useT } from "./i18n";
import { common } from "./strings";
import { vocab } from "./vocab";

// id → 显示名（设计 §4.2）：项目、来源、凭据、执行者按 id 显示名字，不让本人看 id、手打 id。
// 每个 API 客户端各缓存一份，一个页面会话里只取一次；改名后调 forgetNames 让下次重取。
// credential 显示的是它所属的 Agent 的名字（同一 Agent 的多把钥匙合并成同一个名字）；key 是钥匙自己的标签，只用在悬停与详情。
export type NameKind = "project" | "source" | "credential" | "key" | "executor";
export interface Named {
  id: string;
  name: string;
}

type Row = { id: string; name?: string; label?: string; agent_id?: string; harness?: string };
const SOURCES: Record<NameKind, { path: string; key: string; name: (r: Row) => string }> = {
  project: { path: "/projects", key: "projects", name: (r) => r.name || r.id },
  source: { path: "/sources", key: "sources", name: (r) => r.name || r.id }, // 内核保证有显示名（§9.1）；id 只在旧服务端时兜底
  credential: { path: "/settings/tokens", key: "credentials", name: (r) => r.label || r.id },
  key: { path: "/settings/tokens", key: "credentials", name: (r) => r.label || r.id },
  executor: { path: "/executors", key: "executors", name: (r) => r.label || r.id },
};

/** 钥匙 → Agent 名：归属 Agent 的按卡名，旧设置迁移来的（只有程序标识）按 legacy:<程序> 的卡名；对不上返回 undefined，退回钥匙标签。 */
async function agentNames(api: ApiClient): Promise<(k: Row) => string | undefined> {
  const cards = await api.get<{ agents: { id: string; name: string }[] }>("/agents").then((r) => new Map(r.agents.map((a) => [a.id, a.name])), () => new Map<string, string>());
  return (k) => cards.get(k.agent_id || "legacy:" + k.harness);
}

const cache = new WeakMap<ApiClient, Map<NameKind, Promise<Named[]>>>();
// 已取到的名单：后挂载的组件首帧就用上名字，不先闪一下 id。
const resolved = new WeakMap<ApiClient, Map<NameKind, Named[]>>();

export function loadNames(api: ApiClient, kind: NameKind): Promise<Named[]> {
  let m = cache.get(api);
  if (!m) cache.set(api, (m = new Map()));
  let p = m.get(kind);
  if (!p) {
    const s = SOURCES[kind];
    const agents = kind === "credential" ? agentNames(api) : Promise.resolve(() => undefined);
    p = Promise.all([api.get<Record<string, Row[]>>(s.path), agents]).then(([r, agent]) => {
      const list = (r[s.key] ?? []).map((x) => ({ id: x.id, name: agent(x) ?? s.name(x) }));
      if (!resolved.has(api)) resolved.set(api, new Map());
      resolved.get(api)!.set(kind, list);
      return list;
    });
    // 名字只是显示上的改进：取不到时退回显示 id，并允许下次重试。
    p.catch(() => m!.delete(kind));
    m.set(kind, p);
  }
  return p;
}

export function forgetNames(api: ApiClient, kind: NameKind) {
  cache.get(api)?.delete(kind);
  resolved.get(api)?.delete(kind);
}

/** 某类对象的列表（取到之前为空）。 */
export function useNamed(kind: NameKind): Named[] {
  const { api } = useSession();
  const [list, setList] = useState<Named[]>(() => resolved.get(api)?.get(kind) ?? []);
  useEffect(() => {
    let live = true;
    loadNames(api, kind).then((l) => live && setList(l), () => undefined);
    return () => {
      live = false;
    };
  }, [api, kind]);
  return list;
}

/** 对话捕获的内部来源 capture:<项目>（§9.3）：不在来源登记里，按项目名称呼。 */
export const CAPTURE_PREFIX = "capture:";

/** 按 id 取显示名；名单里没有（或还没取到）时显示 id。来源里的对话捕获显示「对话捕获 · 项目名」。 */
export function useNames(kind: NameKind): (id?: string | null) => string {
  const list = useNamed(kind);
  const projects = useNamed("project");
  const lang = useLang();
  const by = new Map(list.map((x) => [x.id, x.name]));
  const project = new Map(projects.map((x) => [x.id, x.name]));
  return (id) => {
    if (!id) return "";
    if (kind === "source" && id.startsWith(CAPTURE_PREFIX)) {
      const p = id.slice(CAPTURE_PREFIX.length);
      return `${vocab[lang].captureSource} · ${project.get(p) ?? p}`;
    }
    return by.get(id) ?? id;
  };
}

/** 选一个对象：显示名字，提交 id。当前值不在名单里时仍保留为一个选项，不丢。 */
export function EntityPick({ kind, value, onChange, label, empty, disabled, autoFocus }: {
  kind: NameKind;
  value: string;
  onChange: (id: string) => void;
  label?: string;
  /** 空选项的文字；省略则没有空选项。 */
  empty?: string;
  disabled?: boolean;
  autoFocus?: boolean;
}) {
  const list = useNamed(kind);
  const opts = value && !list.some((x) => x.id === value) ? [...list, { id: value, name: value }] : list;
  return (
    <select aria-label={label} value={value} disabled={disabled} autoFocus={autoFocus} onChange={(e) => onChange(e.target.value)}>
      {empty !== undefined && <option value="">{empty}</option>}
      {opts.map((x) => (
        <option key={x.id} value={x.id}>{x.name}</option>
      ))}
    </select>
  );
}

const titles = new WeakMap<ApiClient, Map<string, Promise<string>>>();

/** 事项标题（按需取，按 id 缓存）；取到之前和取不到时显示 id。 */
export function useItemTitle(id?: string | null): string {
  const { api } = useSession();
  const [title, setTitle] = useState("");
  useEffect(() => {
    if (!id) return;
    let m = titles.get(api);
    if (!m) titles.set(api, (m = new Map()));
    let p = m.get(id);
    if (!p) {
      // GET /items/{id} 返回 {item, relations, …}，标题在 item 里。
      p = api.get<{ item?: { title?: string } }>("/items/" + encodeURIComponent(id)).then((r) => r.item?.title || id);
      p.catch(() => m!.delete(id));
      m.set(id, p);
    }
    let live = true;
    p.then((t) => live && setTitle(t), () => undefined);
    return () => {
      live = false;
    };
  }, [api, id]);
  return id ? title || id : "";
}

export interface ItemOption {
  id: string;
  title: string;
  project?: string;
}

/** 可搜索事项选择器：按项目名与事项标题实时过滤，支持键盘导航与清空。未传 items 时自动取 GET /items。 */
export function ItemPick({ value, onChange, label, items, placeholder, disabled, loading }: {
  value: string;
  onChange: (id: string) => void;
  /** 输入框与列表的无障碍名称。 */
  label: string;
  /** 外部传入候选项；省略时自动从 GET /items 拉取。 */
  items?: ItemOption[];
  placeholder?: string;
  disabled?: boolean;
  loading?: boolean;
}) {
  const { api } = useSession();
  const c = useT(common);
  const projectName = useNames("project");
  const [loaded, setLoaded] = useState<ItemOption[] | null>(null);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const listId = useId();

  useEffect(() => {
    if (items !== undefined) return;
    let live = true;
    api.get<{ items: ItemOption[] }>("/items").then(
      (r) => live && setLoaded(r.items),
      () => live && setLoaded([]),
    );
    return () => {
      live = false;
    };
  }, [api, items]);

  const list = items ?? loaded ?? [];
  const isLoading = loading ?? (items === undefined && loaded === null);
  const selected = list.find((x) => x.id === value);
  const fallbackTitle = useItemTitle(value && !selected ? value : null);
  const selectedLabel = selected ? selected.title || selected.id : value ? fallbackTitle : "";

  const terms = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = list.filter((it) => {
    if (terms.length === 0) return true;
    const title = (it.title || "").toLowerCase();
    const proj = (it.project ? projectName(it.project) : "").toLowerCase();
    return terms.every((term) => title.includes(term) || proj.includes(term));
  });
  const at = Math.min(active, Math.max(filtered.length - 1, 0));

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) {
        setOpen(false);
        setQuery("");
      }
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  useEffect(() => {
    if (!open || !listRef.current) return;
    const el = listRef.current.children[at] as HTMLElement | undefined;
    el?.scrollIntoView?.({ block: "nearest" });
  }, [open, at, filtered.length]);

  const choose = (id: string) => {
    onChange(id);
    setOpen(false);
    setQuery("");
    setActive(0);
  };

  const clear = () => {
    onChange("");
    setQuery("");
    setActive(0);
    setOpen(false);
  };

  return (
    <div className="item-pick" ref={rootRef}>
      <div className="item-pick-input">
        <input
          type="text"
          role="combobox"
          aria-label={label}
          aria-expanded={open}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={open && filtered[at] ? `${listId}-${at}` : undefined}
          disabled={disabled}
          placeholder={isLoading ? c.loading : open && selectedLabel ? selectedLabel : placeholder || c.searchItemPlaceholder}
          value={open ? query : selectedLabel}
          onFocus={() => {
            if (disabled) return;
            setOpen(true);
            const idx = value ? filtered.findIndex((x) => x.id === value) : 0;
            setActive(idx >= 0 ? idx : 0);
          }}
          onClick={() => !disabled && setOpen(true)}
          onChange={(e) => {
            setQuery(e.target.value);
            setActive(0);
            if (!open) setOpen(true);
          }}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              if (!open) setOpen(true);
              else if (filtered.length) setActive((at + 1) % filtered.length);
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              if (!open) setOpen(true);
              else if (filtered.length) setActive((at - 1 + filtered.length) % filtered.length);
            } else if (e.key === "Enter" && open) {
              e.preventDefault();
              if (filtered[at]) choose(filtered[at].id);
            } else if (e.key === "Escape" && open) {
              e.preventDefault();
              setOpen(false);
              setQuery("");
            }
          }}
          onBlur={(e) => {
            if (e.relatedTarget && !rootRef.current?.contains(e.relatedTarget as Node)) {
              setOpen(false);
              setQuery("");
            }
          }}
        />
        {!disabled && (value || query) && (
          <button
            type="button"
            className="quiet item-pick-clear"
            aria-label={c.clearChoice}
            onMouseDown={(e) => e.preventDefault()}
            onClick={clear}
          >
            ×
          </button>
        )}
      </div>
      {open && !disabled && (
        <div className="item-pick-menu">
          {isLoading ? (
            <p className="item-pick-empty" role="status">{c.loading}</p>
          ) : filtered.length === 0 ? (
            <p className="item-pick-empty">{c.itemSearchEmpty}</p>
          ) : (
            <ul id={listId} ref={listRef} role="listbox" aria-label={label}>
              {filtered.map((it, idx) => {
                const pName = it.project ? projectName(it.project) : "";
                return (
                  <li
                    key={it.id}
                    id={`${listId}-${idx}`}
                    role="option"
                    aria-selected={idx === at}
                    data-picked={it.id === value ? "true" : undefined}
                    onMouseEnter={() => setActive(idx)}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => choose(it.id)}
                  >
                    <span>{it.title || it.id}</span>
                    {pName && <small>{pName}</small>}
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
