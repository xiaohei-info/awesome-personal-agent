import type { Item } from "../items/ItemsPage";
import { hostParts, parseSlot } from "../items/schedule";

// 日历的取材与排布（界面设计 §9.12）：纯计算，不读不写服务端。日期一律是主机时区的 YYYY-MM-DD。

export type View = "day" | "week" | "month";
export const VIEWS: View[] = ["day", "week", "month"];

const DAY = 86400000;
const toMs = (d: string) => Date.parse(d + "T00:00:00Z");
const fromMs = (ms: number) => new Date(ms).toISOString().slice(0, 10);
export const addDays = (d: string, n: number) => fromMs(toMs(d) + n * DAY);
/** 周一为 0。 */
const weekday = (d: string) => (new Date(toMs(d)).getUTCDay() + 6) % 7;
export const monday = (d: string) => addDays(d, -weekday(d));

/** 视图要读的日期范围（含首尾）：日一天，周从周一起七天，月是从 1 号所在周的周一起的 6×7 格。 */
export function range(view: View, date: string): { from: string; to: string; days: string[] } {
  const from = view === "day" ? date : view === "week" ? monday(date) : monday(date.slice(0, 8) + "01");
  const n = view === "day" ? 1 : view === "week" ? 7 : 42;
  const days = Array.from({ length: n }, (_, i) => addDays(from, i));
  return { from, to: days[n - 1], days };
}

/** 上一段、下一段：日 ±1 天，周 ±7 天，月 ±1 个月（落到该月 1 号）。 */
export function step(view: View, date: string, dir: 1 | -1): string {
  if (view === "day") return addDays(date, dir);
  if (view === "week") return addDays(date, 7 * dir);
  const [y, m] = date.split("-").map(Number);
  const t = new Date(Date.UTC(y, m - 1 + dir, 1));
  return fromMs(t.getTime());
}

export type Status = "todo" | "waiting" | "needs_input" | "done";
const SHOWN: Record<string, Status> = { open: "todo", waiting: "waiting", needs_input: "needs_input", done: "done" };

/** 日历上的一条：计划那天的一条（plan），或截止那天的一个截止标记（due）。 */
export interface Entry {
  item: Item;
  type: "plan" | "due";
  day: string;
  status: Status;
  /** 时段的起止分钟；没有时段（未定时）或截止标记为 null。 */
  slot: [number, number] | null;
  /** 截止在主机时区的日期与 HH:MM（只到日时为 null）。 */
  due: { date: string; time: string | null } | null;
  /** 这一条带"截止"标记：截止标记本身，或计划与截止在同一天。 */
  dueHere: boolean;
  overdue: boolean;
  pastDue: boolean;
  lateVsDue: boolean;
}

const ACTIVE = new Set(["open", "waiting", "needs_input"]);

/**
 * 把事项排成每天的条目。只收进行中、在等、等你决定、已完成四类，不收简报；没有日期也没有截止的不上日历。
 * 计划与截止同一天只出一条（带截止标记）；不在同一天时，计划那天一条，截止那天另一个截止标记。
 */
export function entries(items: Item[], today: string, offset: string, now: number): Map<string, Entry[]> {
  const out = new Map<string, Entry[]>();
  const put = (e: Entry) => out.set(e.day, [...(out.get(e.day) ?? []), e]);
  for (const it of items) {
    const status = SHOWN[it.phase];
    if (!status || it.kind === "brief") continue;
    const dp = it.deadline ? hostParts(it.deadline, offset) : null;
    const due = dp ? { date: dp.date, time: dp.time === "23:59:59" ? null : dp.time.slice(0, 5) } : null;
    const pastDue = !!it.deadline && Date.parse(it.deadline) < now && it.phase !== "done";
    const base = { item: it, status, due, pastDue };
    const plan = it.planned_for;
    if (plan) {
      put({ ...base, type: "plan", day: plan, slot: parseSlot(it.planned_slot), dueHere: due?.date === plan,
        overdue: plan < today && ACTIVE.has(it.phase), lateVsDue: !!due && plan > due.date });
    }
    if (due && due.date !== plan) put({ ...base, type: "due", day: due.date, slot: null, dueHere: true, overdue: false, lateVsDue: false });
  }
  return out;
}

const PRIO: Record<string, number> = { urgent: 0, high: 1, normal: 2, low: 3 };
const doneLast = (a: Entry, b: Entry) => Number(a.status === "done") - Number(b.status === "done");

/** 一天里的三栏：截止标记、有时段的（按开始时间）、未定时的（按优先级，同级按标题）；已完成的在各组末尾。 */
export function split(list: Entry[] = []): { due: Entry[]; timed: Entry[]; untimed: Entry[] } {
  const due = list.filter((e) => e.type === "due").sort((a, b) => doneLast(a, b) || (a.due?.time ?? "99").localeCompare(b.due?.time ?? "99") || a.item.title.localeCompare(b.item.title));
  const timed = list.filter((e) => e.type === "plan" && e.slot).sort((a, b) => doneLast(a, b) || a.slot![0] - b.slot![0] || a.slot![1] - b.slot![1]);
  const untimed = list.filter((e) => e.type === "plan" && !e.slot).sort((a, b) =>
    doneLast(a, b) || (PRIO[a.item.priority ?? "normal"] ?? 2) - (PRIO[b.item.priority ?? "normal"] ?? 2) || a.item.title.localeCompare(b.item.title));
  return { due, timed, untimed };
}

/** 月格与窄屏周列表里的顺序：截止 → 有时段 → 未定时。 */
export const ordered = (list?: Entry[]) => {
  const s = split(list);
  return [...s.due, ...s.timed, ...s.untimed];
};

/** 时间格里的一块：所在栏与栏数；overflow 是第三栏里收起的"+n"。 */
export interface Block {
  entry: Entry;
  col: number;
  cols: number;
  overlap: boolean;
}
export interface Overflow {
  entries: Entry[];
  start: number;
  end: number;
}

/**
 * 重叠的时段并排分栏，最多三栏；一组重叠里需要超过三栏时，第三栏起的都收进第三栏的"+n"。
 * 和其他任何一块时间相交的块标"时间重叠"。
 */
export function lanes(timed: Entry[]): { blocks: Block[]; overflow: Overflow[] } {
  const sorted = [...timed].sort((a, b) => a.slot![0] - b.slot![0] || b.slot![1] - a.slot![1]);
  const hits = (a: Entry, b: Entry) => a.slot![0] < b.slot![1] && b.slot![0] < a.slot![1];
  const blocks: Block[] = [];
  const overflow: Overflow[] = [];
  let group: Entry[] = [];
  let groupEnd = -1;
  const flush = () => {
    if (!group.length) return;
    const ends: number[] = [];
    const col = group.map((e) => {
      let c = ends.findIndex((end) => end <= e.slot![0]);
      if (c < 0) c = ends.length;
      ends[c] = e.slot![1];
      return c;
    });
    const cols = Math.min(ends.length, 3);
    const extra = ends.length > 3 ? group.filter((_, i) => col[i] >= 2) : [];
    group.forEach((e, i) => {
      if (ends.length > 3 && col[i] >= 2) return;
      blocks.push({ entry: e, col: col[i], cols, overlap: group.some((o) => o !== e && hits(o, e)) });
    });
    if (extra.length) overflow.push({ entries: extra, start: Math.min(...extra.map((e) => e.slot![0])), end: Math.max(...extra.map((e) => e.slot![1])) });
    group = [];
    groupEnd = -1;
  };
  for (const e of sorted) {
    if (group.length && e.slot![0] >= groupEnd) flush();
    group.push(e);
    groupEnd = Math.max(groupEnd, e.slot![1]);
  }
  flush();
  return { blocks, overflow };
}

/** 日视图打开时滚到的分钟：07:00 与最早一条时段前一小时取更早的。 */
export const scrollStart = (timed: Entry[]) => Math.max(0, Math.min(7 * 60, ...timed.map((e) => e.slot![0] - 60)));
