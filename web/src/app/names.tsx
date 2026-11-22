import { useEffect, useState } from "react";
import type { ApiClient } from "./api";
import { useSession } from "./session";
import { useLang } from "./i18n";
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

/** 按标题找一个事项：输入几个字列出匹配的事项（GET /items?q=），选中后提交 id；不让人手打 id。 */
export function ItemPick({ value, onChange, label, find, empty }: {
  value: string;
  onChange: (id: string) => void;
  /** 下拉的无障碍名称。 */
  label: string;
  /** 搜索框的无障碍名称与提示。 */
  find: string;
  /** 「不限」选项的文字。 */
  empty: string;
}) {
  const { api } = useSession();
  const [q, setQ] = useState("");
  const [hits, setHits] = useState<{ id: string; title: string }[]>([]);
  const current = useItemTitle(value);
  useEffect(() => {
    if (!q.trim()) return setHits([]);
    let live = true;
    // shortcut: 每次输入都查一次、只取第一页，事项很多时再加防抖与分页。
    api.get<{ items: { id: string; title: string }[] }>("/items?q=" + encodeURIComponent(q.trim())).then(
      (r) => live && setHits(r.items),
      () => live && setHits([]),
    );
    return () => {
      live = false;
    };
  }, [api, q]);
  const opts = value && !hits.some((x) => x.id === value) ? [{ id: value, title: current || value }, ...hits] : hits;
  return (
    <span>
      <input aria-label={find} placeholder={find} value={q} onChange={(e) => setQ(e.target.value)} />
      <select aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{empty}</option>
        {opts.map((x) => <option key={x.id} value={x.id}>{x.title}</option>)}
      </select>
    </span>
  );
}
