// 预填新建页（界面设计 §9.11）：/new 与 /items/new 的查询参数只作表单初值，来自外部，按不可信文本处理。
export const MAX_TITLE = 200;
export const MAX_BODY = 4000;

export interface Prefill {
  title: string;
  body: string;
  /** 已去掉换行与首尾空白；空串即没有。 */
  link: string;
  /** 项目 id 或名字，认不认得由页面对着项目列表决定。 */
  project: string;
  plannedFor: string;
  /** datetime-local 的值（本地时间）。 */
  deadline: string;
  cutTitle: boolean;
  cutBody: boolean;
  /** 有任何一项预填内容。 */
  any: boolean;
}

const cut = (s: string, n: number): [string, boolean] => {
  const cs = Array.from(s);
  return cs.length > n ? [cs.slice(0, n).join(""), true] : [s, false];
};
const pad = (n: number) => String(n).padStart(2, "0");
const local = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;

/** 只给日期时取当天 23:59（本地）；带时区的时刻转成本地；认不出就留空。 */
export function parseDeadline(v: string): string {
  v = v.trim();
  if (/^\d{4}-\d{2}-\d{2}$/.test(v)) return Number.isNaN(new Date(v + "T23:59").getTime()) ? "" : v + "T23:59";
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(v)) return Number.isNaN(new Date(v).getTime()) ? "" : v;
  const d = new Date(v);
  return /^\d{4}-\d{2}-\d{2}T/.test(v) && !Number.isNaN(d.getTime()) ? local(d) : "";
}

const validDay = (v: string) => /^\d{4}-\d{2}-\d{2}$/.test(v) && !Number.isNaN(new Date(v + "T00:00").getTime());

export function readPrefill(p: URLSearchParams): Prefill {
  const [title, cutTitle] = cut(p.get("title") ?? "", MAX_TITLE);
  const [body, cutBody] = cut(p.get("body") ?? "", MAX_BODY);
  const link = (p.get("link") ?? "").replace(/\s+/g, " ").trim();
  const project = (p.get("project") ?? "").trim();
  const planned = (p.get("planned_for") ?? "").trim();
  const plannedFor = validDay(planned) ? planned : "";
  const deadline = parseDeadline(p.get("deadline") ?? "");
  return { title, body, link, project, plannedFor, deadline, cutTitle, cutBody, any: !!(title || body || link || project || plannedFor || deadline) };
}

export interface Vault { name: string; open_url?: string }

/** 登记链接型材料的位置（§12.8）：已登记笔记库内的 `vault:<库名>/<相对路径>` 或 http(s) 网址。其余写进正文。 */
export function registrable(link: string, vaults: Vault[]): boolean {
  if (/^https?:\/\/\S+$/i.test(link)) return true;
  const m = link.match(/^vault:([^/]+)\/(.+)$/);
  return !!m && vaults.some((v) => v.name === m[1]) && !m[2].startsWith("/") && !m[2].split("/").includes("..");
}

/** 材料标题取文件名（网址取最后一段，没有就取主机名）。 */
export function linkTitle(link: string): string {
  const path = link.replace(/^vault:[^/]+\//, "").replace(/^https?:\/\//i, "").split(/[?#]/)[0].replace(/\/+$/, "");
  const last = path.split("/").pop() || link;
  try {
    return decodeURIComponent(last);
  } catch {
    return last;
  }
}

/** 没有登记成材料的 link 作为一行写在正文末尾。 */
export const withLink = (body: string, link: string) => (link ? (body ? body + "\n\n" : "") + link : body);
