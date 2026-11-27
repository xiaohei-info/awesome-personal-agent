// GET /usage 的形状与图表用的整形。数字只做合计与分组；没回报用量的只计次数、不估算。
export type Part = "judge" | "model" | "agent";
export const PARTS: Part[] = ["judge", "model", "agent"];
export interface Counts { requests: number; reported: number; input_tokens: number; output_tokens: number; cache_read_tokens: number; cache_write_tokens: number }
export interface Row extends Counts { part: Part; purpose: string; agent?: string; model?: string }
export interface Point extends Counts { part: Part; slot: string }
export interface Summary { since?: string; rows: Row[]; slots: string[]; series: Point[] }

const KEYS = ["requests", "reported", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"] as const;

export function sum(list: Counts[]): Counts {
  const out: Counts = { requests: 0, reported: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0 };
  for (const c of list) for (const k of KEYS) out[k] += c[k];
  return out;
}

export const total = (c: Counts) => c.input_tokens + c.output_tokens;

/** 输入含缓存读写（内核口径），堆叠前拆成互不重叠的四段。 */
export function split(c: Counts) {
  return {
    read: c.cache_read_tokens,
    fresh: Math.max(0, c.input_tokens - c.cache_read_tokens - c.cache_write_tokens),
    write: c.cache_write_tokens,
    output: c.output_tokens,
  };
}
export const SEGMENTS = ["read", "fresh", "write", "output"] as const; // 自下而上

/** 缓存读取占比：缓存读 /（缓存读 + 非缓存输入），不含缓存写；没有任何缓存读写时没有这个数。 */
export function cacheRate(c: Counts): number | undefined {
  const s = split(c);
  return (s.read || s.write) && s.read + s.fresh ? s.read / (s.read + s.fresh) : undefined;
}

export interface Group<T> extends Counts { key: string; items: T[] }

/** 按 key 分组合计，量大的在前；byCount 时按次数排，否则按 token，一次都没回报的沉底。 */
export function rank<T extends Counts>(list: T[], key: (r: T) => string, byCount: boolean): Group<T>[] {
  const groups = new Map<string, T[]>();
  for (const r of list) groups.set(key(r), [...(groups.get(key(r)) ?? []), r]);
  const size = (c: Counts) => (byCount ? c.requests : c.reported ? total(c) : -1);
  return [...groups].map(([k, items]) => ({ key: k, items, ...sum(items) })).sort((a, b) => size(b) - size(a) || b.requests - a.requests);
}
