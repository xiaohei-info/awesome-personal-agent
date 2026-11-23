import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg } from "../../app/i18n";
import type { Agent, AgentView } from "./AgentPick";
import type { Options } from "./AgentEditor";

// 设置 › Agent 的数据（界面设计 §4）：声明（/settings/declarations 待确认、/settings/commands 已生效）、钥匙（/settings/tokens）、
// 与 Agent 卡（GET /agents，带档位）。判定都在服务端；这里只把它们按「一个 Agent」归拢。
// 一个 Agent 的 id：本人建的卡是 agent_…；旧设置迁移来的（以及还没被允许启动的程序）是 legacy:<程序标识>，与内核的兼容卡同一写法。

type Label = { zh: string; en: string };
export interface Declaration {
  instance_options?: Options;
  harness: string;
  name?: Label;
  headless?: { command?: string[]; access?: Record<string, unknown>; tools?: Record<string, Label[]>; tool_notes?: Record<string, Label> };
  main_session?: { wake?: boolean; launch?: string; wake_missing?: (Label & { how: Label })[] };
}
export interface Rec { harness: string; rev: number; state: string; declaration: Declaration }
export interface Credential { agent_id?: string; id: string; kind: string; label?: string; harness?: string; seen_harness?: string; revoked_at?: string; audience?: string; source_scopes?: Record<string, string> }
export interface Data { pending: Rec[]; live: Rec[]; tokens: Credential[]; cards: Agent[] }

export const AGENT_KINDS = ["delegate", "worker"];
export const LEGACY = "legacy:";

export function useData() {
  const { api } = useSession();
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      const [p, c, t, a] = await Promise.all([
        api.get<{ declarations: Rec[] }>("/settings/declarations"),
        api.get<{ declarations: Rec[] }>("/settings/commands"),
        api.get<{ credentials: Credential[] }>("/settings/tokens"),
        api.get<AgentView>("/agents").then((v) => v.agents, () => [] as Agent[]),
      ]);
      setData({ pending: p.declarations, live: c.declarations, tokens: t.credentials, cards: a });
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);
  return { data, error, setError, load };
}

/** 有哪些程序：有声明的、有归属钥匙的、钥匙最近自报过的。 */
export function agentsOf(d: Pick<Data, "pending" | "live" | "tokens">): string[] {
  const live = d.tokens.filter((c) => !c.revoked_at && AGENT_KINDS.includes(c.kind));
  return [...new Set([
    ...d.pending.map((r) => r.harness),
    ...d.live.map((r) => r.harness),
    ...live.flatMap((c) => (c.harness ? [c.harness] : c.seen_harness ? [c.seen_harness] : [])),
  ])].sort();
}

/** 一个 Agent 在界面上的全部事实。instance 为真是本人建的卡（设置按卡 id 存），否则按程序标识存（旧设置原样保留）。 */
export interface Entry {
  id: string;
  harness: string;
  instance: boolean;
  card?: Agent;
  /** 按卡 id 或程序标识存设置时用的键。 */
  key: string;
  pending?: Rec;
  live?: Rec;
  keys: Credential[];
  /** 归属不明、最近由这个程序自报使用的钥匙：由本人确认归给它。 */
  unclaimed: Credential[];
}

export function entries(d: Data): Entry[] {
  const of = (h: string, card?: Agent): Entry => {
    const instance = !!card && card.rev > 0;
    return {
      id: instance ? card!.id : LEGACY + h,
      harness: h,
      instance,
      card,
      key: instance ? card!.id : h,
      pending: d.pending.find((r) => r.harness === h),
      live: d.live.find((r) => r.harness === h),
      keys: d.tokens.filter((c) => c.harness === h && c.agent_id === (instance ? card!.id : undefined)),
      unclaimed: instance ? [] : d.tokens.filter((c) => !c.harness && c.seen_harness === h && !c.revoked_at && AGENT_KINDS.includes(c.kind)),
    };
  };
  return [
    ...agentsOf(d).map((h) => of(h, d.cards.find((c) => c.id === LEGACY + h))),
    ...d.cards.filter((c) => c.rev > 0).map((c) => of(c.harness, c)),
  ];
}

/** 接入清单的两步：有一把有效的钥匙；APA 被允许在后台启动它（程序的声明已确认）。 */
export const hasKey = (e: Entry) => e.keys.some((c) => !c.revoked_at);
export const ready = (e: Entry) => hasKey(e) && !!e.live;
