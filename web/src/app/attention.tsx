import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import type { ApiClient } from "./api";
import { COUNTS_EVENT } from "./page";
import { loadStatus, type Status } from "./status";

// 注意力汇总（界面设计 §5）：一处算出「等你决定、建议、系统要你处理、提醒」，导航角标、状态灯、今天页顶部都取它，数字永远一致。
// 每一项都是服务端的现状：本页不缓存事项状态做判断依据，换页面或页面里数据变了（COUNTS_EVENT）就重取。

/** 等你决定的问题：needs_input 且还没回答（与「等你决定」页同一个接口、同一个过滤）。 */
export interface Question {
  id: string;
  title: string;
  ask?: { resolution?: unknown };
}
/** 待确认的一行：Agent 的启动请求、文档修改。确认动作仍在权威位置完成，这里只是摘要加链接。 */
export interface Confirm {
  kind: "launch" | "revision";
  id: string;
  name: string;
  to: string;
}
/** 提醒：退回网页、还没确认的投递，扣除决策类、系统类与简报类（它们各在问题卡、状态页、简报页出现，不在这里再算）。 */
export interface Reminder {
  id: string;
  item: string;
  class: string;
  content: { title: string; summary?: string; web_url?: string };
  injections?: { injected_to_session: string; at: string }[];
  suppressed?: string;
}

export interface Attention {
  questions: number;
  confirms: Confirm[];
  /** 等你决定的红色数字：问题数加待确认的条数。 */
  decide: number;
  proposals: number;
  reminders: Reminder[];
  status: Status | null;
}

// 决策类随问题卡、系统类随状态页、简报类就是简报本身（在简报页），都不在「提醒」里再列一遍。
const OWN_CLASSES = new Set(["decision", "health", "brief"]);
const unanswered = (i: Question) => !!i.ask && !i.ask.resolution;

type Label = { zh: string; en: string };
interface Decl { harness: string; rev: number; declaration: { name?: Label } }
interface PendingRevision { revision: { id: string }; material: { id: string; title: string } }

/** 待确认的行：每一项取不到就当作没有，不影响其余几项。 */
export async function loadConfirms(api: ApiClient, lang: "zh" | "en"): Promise<Confirm[]> {
  const [decls, revs] = await Promise.all([
    api.get<{ declarations: Decl[] }>("/settings/declarations").then((r) => r.declarations ?? []).catch(() => [] as Decl[]),
    api.get<{ pending: PendingRevision[] }>("/revisions/pending").then((r) => r.pending ?? []).catch(() => [] as PendingRevision[]),
  ]);
  return [
    ...decls.map((d): Confirm => ({ kind: "launch", id: d.harness, name: d.declaration.name?.[lang] || d.harness, to: "/settings/agents/" + encodeURIComponent("legacy:" + d.harness) + "#connect" })),
    ...revs.map((p): Confirm => ({ kind: "revision", id: p.revision.id, name: p.material.title, to: "/materials?id=" + encodeURIComponent(p.material.id) })),
  ];
}

export async function loadAttention(api: ApiClient, lang: "zh" | "en"): Promise<Attention> {
  const [qs, confirms, proposals, reminders, status] = await Promise.all([
    // 每一项取不到（或形状不对）就当作没有，不拖累其余几项。
    api.get<{ items: Question[] }>("/items?phase=needs_input").then((r) => r.items.filter(unanswered).length).catch(() => 0),
    loadConfirms(api, lang),
    api.get<{ items: unknown[] }>("/items?phase=proposed").then((r) => r.items.length).catch(() => 0),
    api.get<{ deliveries: Reminder[] }>("/deliveries/unread").then((r) => [...r.deliveries].reverse().filter((d) => !OWN_CLASSES.has(d.class))).catch(() => [] as Reminder[]),
    loadStatus(api),
  ]);
  return { questions: qs, confirms, decide: qs + confirms.length, proposals, reminders, status };
}

const Ctx = createContext<Attention | null>(null);

/** 外壳在最外层取一次，下面的导航、状态灯、今天页都读同一份。 */
export function AttentionProvider({ api, lang, pathname, children }: { api: ApiClient; lang: "zh" | "en"; pathname: string; children: ReactNode }) {
  const [a, setA] = useState<Attention | null>(null);
  useEffect(() => {
    let live = true;
    const load = () => loadAttention(api, lang).then((x) => live && setA(x));
    load();
    window.addEventListener(COUNTS_EVENT, load);
    return () => {
      live = false;
      window.removeEventListener(COUNTS_EVENT, load);
    };
  }, [api, lang, pathname]);
  return <Ctx.Provider value={a}>{children}</Ctx.Provider>;
}

/** 当前的注意力汇总；还没取到时为 null（不显示数字）。 */
export const useAttention = () => useContext(Ctx);

/** 页面里的数据变了（答完、处理完）：让汇总重取。 */
export const refreshCounts = () => window.dispatchEvent(new Event(COUNTS_EVENT));
