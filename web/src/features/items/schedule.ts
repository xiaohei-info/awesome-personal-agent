import { useEffect, useState } from "react";
import type { ApiClient } from "../../app/api";

// 排期的口径（主文档 §6.3、界面设计 §9.4）：时段 `HH:MM - HH:MM` 落在计划日期那天、不跨午夜；
// 日界与时刻一律按 APA 主机时区，不按浏览器。只到日的截止存成当天 23:59:59（主机时区），显示时只写日期。

/** 时段的起止分钟数；写法与内核 domain.ParseSlot 完全一致，不合法返回 null。 */
export function parseSlot(s: string | undefined): [number, number] | null {
  const m = /^(\d{2}):(\d{2}) - (\d{2}):(\d{2})$/.exec(s ?? "");
  if (!m) return null;
  const [h1, m1, h2, m2] = m.slice(1).map(Number);
  const start = h1 * 60 + m1;
  const end = h2 * 60 + m2;
  const ok = m1 < 60 && m2 < 60 && h1 < 24 && (h2 < 24 || (h2 === 24 && m2 === 0)) && end > start;
  return ok ? [start, end] : null;
}

const pad = (n: number) => String(n).padStart(2, "0");
/** 分钟数 → HH:MM（1440 → 24:00）。 */
export const hhmm = (min: number) => `${pad(Math.floor(min / 60))}:${pad(min % 60)}`;
export const slotOf = (start: number, end: number) => `${hhmm(start)} - ${hhmm(end)}`;

/** "+08:00" → 480 分钟。 */
export function offsetMinutes(offset: string): number {
  const m = /^([+-])(\d{2}):(\d{2})$/.exec(offset);
  return m ? (m[1] === "-" ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3])) : 0;
}

/** 一个时刻在主机时区的日期与时刻（YYYY-MM-DD、HH:MM:SS）。 */
export function hostParts(at: string | number, offset: string): { date: string; time: string } {
  const d = new Date((typeof at === "number" ? at : Date.parse(at)) + offsetMinutes(offset) * 60000);
  return {
    date: `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`,
    time: `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`,
  };
}

/** 只到日的截止：主机时区恰好 23:59:59。 */
export const dayOnly = (deadline: string, offset: string) => hostParts(deadline, offset).time === "23:59:59";

// shortcut: 用"此刻"的主机偏移换算截止当天，夏令时切换前后的截止会差一小时；主机在有夏令时的时区时改由服务端换算。
/** 截止的写入值：只给日期取当天 23:59:59，给了 HH:MM 取该时刻，都按主机时区。 */
export const deadlineValue = (date: string, time: string | null, offset: string) => `${date}T${time ? time + ":00" : "23:59:59"}${offset}`;

/** 截止的显示：只到日的写日期，否则写日期与 HH:MM（主机时区）。 */
export function deadlineText(deadline: string, offset: string): string {
  const p = hostParts(deadline, offset);
  return dayOnly(deadline, offset) ? p.date : `${p.date} ${p.time.slice(0, 5)}`;
}

export interface HostClock {
  today: string;
  utc_offset: string;
}

let cached: Promise<HostClock> | null = null;

/** 主机的今天与时区偏移，取自 GET /items 的范围读法（附录 B.2）；一页只取一次。取不到时退回浏览器时区。 */
export function hostClock(api: ApiClient): Promise<HostClock> {
  if (!cached) {
    const d = new Date();
    const day = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
    cached = api.get<HostClock>(`/items?from=${day}&to=${day}&kind=-`).then(
      (r) => (r.today && r.utc_offset ? { today: r.today, utc_offset: r.utc_offset } : browserClock()),
      () => {
        cached = null;
        return browserClock();
      },
    );
  }
  return cached;
}

export function browserClock(): HostClock {
  const d = new Date();
  const off = -d.getTimezoneOffset();
  const sign = off < 0 ? "-" : "+";
  return { today: `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`, utc_offset: `${sign}${pad(Math.floor(Math.abs(off) / 60))}:${pad(Math.abs(off) % 60)}` };
}

/** 页面用：主机时钟到手前先按浏览器时区显示。 */
export function useHostClock(api: ApiClient): HostClock {
  const [c, setC] = useState(browserClock);
  useEffect(() => {
    let live = true;
    hostClock(api).then((v) => live && setC(v));
    return () => {
      live = false;
    };
  }, [api]);
  return c;
}

/** 测试用：清掉缓存的主机时钟。 */
export const resetHostClock = () => {
  cached = null;
};
