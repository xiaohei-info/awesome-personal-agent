import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg } from "../../app/i18n";

// 设置 › Agent 的数据（界面设计 §4）：声明（/settings/declarations 待确认、/settings/commands 已生效）与钥匙（/settings/tokens）。
// 一个 Agent 就是一份声明，id 是声明的标识；它只管接入与权限卡，谁来干活在执行者页选。判定都在服务端，这里只把它们按 Agent 归拢。
// 隔离另写的声明（base 记所属程序，如由旧的日常卡迁来）：同一程序、单独一张权限卡，钥匙用 agent_id 绑到它。

type Label = { zh: string; en: string };
export interface Declaration {
  harness: string;
  /** 非空：同一程序隔离另写的声明，值是所属程序的标识。 */
  base?: string;
  name?: Label;
  headless?: { command?: string[]; access?: Record<string, unknown>; tools?: Record<string, Label[]>; tool_notes?: Record<string, Label> };
  main_session?: { wake?: boolean; launch?: string; wake_missing?: (Label & { how: Label })[] };
}
export interface Rec { harness: string; rev: number; state: string; declaration: Declaration }
export interface Credential { agent_id?: string; id: string; kind: string; label?: string; harness?: string; seen_harness?: string; revoked_at?: string; audience?: string; source_scopes?: Record<string, string> }
export interface Data { pending: Rec[]; live: Rec[]; tokens: Credential[] }

export const AGENT_KINDS = ["delegate", "worker"];

export function useData() {
  const { api } = useSession();
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      const [p, c, t] = await Promise.all([
        api.get<{ declarations: Rec[] }>("/settings/declarations"),
        api.get<{ declarations: Rec[] }>("/settings/commands"),
        api.get<{ credentials: Credential[] }>("/settings/tokens"),
      ]);
      setData({ pending: p.declarations, live: c.declarations, tokens: t.credentials });
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);
  return { data, error, setError, load };
}

/** 有哪些 Agent：有声明的、有归属钥匙的、钥匙最近自报过的。 */
export function agentsOf(d: Pick<Data, "pending" | "live" | "tokens">): string[] {
  const live = d.tokens.filter((c) => !c.revoked_at && AGENT_KINDS.includes(c.kind));
  return [...new Set([
    ...d.pending.map((r) => r.harness),
    ...d.live.map((r) => r.harness),
    ...live.flatMap((c) => (c.agent_id ? [c.agent_id] : c.harness ? [c.harness] : c.seen_harness ? [c.seen_harness] : [])),
  ])].sort();
}

/** 一个 Agent 在界面上的全部事实；id 是声明的标识，权限卡、在哪里做都按它存。 */
export interface Entry {
  id: string;
  /** 钥匙登记的程序：普通声明就是 id，隔离另写的是所属程序。 */
  harness: string;
  base?: string;
  pending?: Rec;
  live?: Rec;
  keys: Credential[];
  /** 归属不明、最近由这个程序自报使用的钥匙：由本人确认归给它。 */
  unclaimed: Credential[];
}

export function entries(d: Data): Entry[] {
  return agentsOf(d).map((id) => {
    const pending = d.pending.find((r) => r.harness === id);
    const live = d.live.find((r) => r.harness === id);
    const base = (live ?? pending)?.declaration.base || undefined;
    return {
      id,
      harness: base ?? id,
      base,
      pending,
      live,
      keys: d.tokens.filter((c) => (base ? c.agent_id === id : c.harness === id && !c.agent_id)),
      unclaimed: base ? [] : d.tokens.filter((c) => !c.harness && c.seen_harness === id && !c.revoked_at && AGENT_KINDS.includes(c.kind)),
    };
  });
}

/** 发钥匙的请求体里归属的部分：隔离另写的声明用 agent_id 绑到它，harness 写所属程序。 */
export const keyOwner = (e: Entry): Record<string, string> => (e.base ? { harness: e.base, agent_id: e.id } : { harness: e.id });

/** 接入清单的两步：有一把有效的钥匙；APA 被允许在后台启动它（声明已确认）。 */
export const hasKey = (e: Entry) => e.keys.some((c) => !c.revoked_at);
export const ready = (e: Entry) => hasKey(e) && !!e.live;
