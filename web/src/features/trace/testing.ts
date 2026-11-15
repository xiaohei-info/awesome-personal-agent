import type { TraceData } from "./Trace";

// 测试替身用：把替身记下的 Event 按内核读模型的规则写成处理经过（GET /items/{id}/trace 的形状）。
export interface DoubleEvent { seq: number; type: string; object: string; actor: { type: string; principal: string; via: string }; at: string; reason?: string; to?: string; fields?: string[] }
const KIND: Record<string, string> = { "item.created": "created", "item.updated": "edited", "item.phase_changed": "phase", "item.related": "related" };

export function traceOf(events: DoubleEvent[]): TraceData {
  return {
    entries: events.map((e) => {
      const kind = KIND[e.type] ?? "event";
      const who = e.actor.type === "human" ? { kind: "owner" } : e.actor.type === "agent" ? { kind: "agent", name: e.actor.principal } : { kind: "system" };
      const args = kind === "phase" ? { to: e.to } : kind === "edited" ? { fields: e.fields ?? [] } : { type: e.type };
      return { at: e.at, who, kind, args, steps: [{ ...e, actor: e.actor }] };
    }),
  };
}

/** 替身路由：/v1/items/{id}/trace → 该事项的经过；不是这个路径时为 null。 */
export function traceRoute(url: string, events: DoubleEvent[]): Response | null {
  const m = url.match(/^\/v1\/items\/([^/]+)\/trace$/);
  if (!m) return null;
  return new Response(JSON.stringify(traceOf(events.filter((e) => e.object === m[1]))), { status: 200, headers: { "Content-Type": "application/json" } });
}
