import { useEffect, useState } from "react";
import type { ApiClient } from "./api";
import { useSession } from "./session";

// id → 显示名（设计 §4.2）：项目、来源、凭据、执行者按 id 显示名字，不让本人看 id、手打 id。
// 每个 API 客户端各缓存一份，一个页面会话里只取一次；改名后调 forgetNames 让下次重取。
export type NameKind = "project" | "source" | "credential" | "executor";
export interface Named {
  id: string;
  name: string;
}

type Row = { id: string; name?: string; label?: string };
const SOURCES: Record<NameKind, { path: string; key: string; name: (r: Row) => string }> = {
  project: { path: "/projects", key: "projects", name: (r) => r.name || r.id },
  source: { path: "/sources", key: "sources", name: (r) => r.id },
  credential: { path: "/settings/tokens", key: "credentials", name: (r) => r.label || r.id },
  executor: { path: "/settings/executors", key: "executors", name: (r) => r.id },
};

const cache = new WeakMap<ApiClient, Map<NameKind, Promise<Named[]>>>();

export function loadNames(api: ApiClient, kind: NameKind): Promise<Named[]> {
  let m = cache.get(api);
  if (!m) cache.set(api, (m = new Map()));
  let p = m.get(kind);
  if (!p) {
    const s = SOURCES[kind];
    p = api.get<Record<string, Row[]>>(s.path).then((r) => (r[s.key] ?? []).map((x) => ({ id: x.id, name: s.name(x) })));
    // 名字只是显示上的改进：取不到时退回显示 id，并允许下次重试。
    p.catch(() => m!.delete(kind));
    m.set(kind, p);
  }
  return p;
}

export function forgetNames(api: ApiClient, kind: NameKind) {
  cache.get(api)?.delete(kind);
}

/** 某类对象的列表（取到之前为空）。 */
export function useNamed(kind: NameKind): Named[] {
  const { api } = useSession();
  const [list, setList] = useState<Named[]>([]);
  useEffect(() => {
    let live = true;
    loadNames(api, kind).then((l) => live && setList(l), () => undefined);
    return () => {
      live = false;
    };
  }, [api, kind]);
  return list;
}

/** 按 id 取显示名；名单里没有（或还没取到）时显示 id。 */
export function useNames(kind: NameKind): (id?: string | null) => string {
  const list = useNamed(kind);
  const by = new Map(list.map((x) => [x.id, x.name]));
  return (id) => (id ? by.get(id) ?? id : "");
}

/** 选一个对象：显示名字，提交 id。当前值不在名单里时仍保留为一个选项，不丢。 */
export function EntityPick({ kind, value, onChange, label, empty }: {
  kind: NameKind;
  value: string;
  onChange: (id: string) => void;
  label?: string;
  /** 空选项的文字；省略则没有空选项。 */
  empty?: string;
}) {
  const list = useNamed(kind);
  const opts = value && !list.some((x) => x.id === value) ? [...list, { id: value, name: value }] : list;
  return (
    <select aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
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
      p = api.get<{ title?: string }>("/items/" + encodeURIComponent(id)).then((it) => it.title || id);
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
