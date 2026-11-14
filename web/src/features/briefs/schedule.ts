// 简报的定时：本人选「哪几天 + 几点」，这里生成标准 5 段 cron（§6.8）。
// cron 不带时区时内核按 APA 主机的时区解释，浏览器所在时区未必相同，所以生成时一律带 CRON_TZ=<浏览器时区>。
export const DAYS = ["*", "1-5", "1", "2", "3", "4", "5", "6", "0"] as const;
export type Days = (typeof DAYS)[number];

export const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || "";

/** "08:30" + 工作日 → "CRON_TZ=<zone> 30 8 * * 1-5"；zone 为空时不带前缀。 */
export function toCron(days: Days, time: string, zone: string): string {
  const [h, m] = time.split(":").map(Number);
  return `${zone ? `CRON_TZ=${zone} ` : ""}${m} ${h} * * ${days}`;
}

const SIMPLE = /^CRON_TZ=(\S+) (\d{1,2}) (\d{1,2}) \* \* (\*|1-5|[0-6])$/;

/** 读回本页生成的那种写法；带不上时区或写法更复杂时返回 null，由调用方原样显示。 */
export function parseCron(s: string): { days: Days; time: string; zone: string } | null {
  const m = SIMPLE.exec(s.trim());
  if (!m) return null;
  const [, zone, min, hour, days] = m;
  if (Number(min) > 59 || Number(hour) > 23) return null;
  return { days: days as Days, time: `${hour.padStart(2, "0")}:${min.padStart(2, "0")}`, zone };
}
