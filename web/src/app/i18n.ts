import { useSyncExternalStore } from "react";
import { ApiError, type ApiErrorBody } from "./api";
import { GENERIC_ERROR } from "./ids";
import { vocab, type Entry } from "./vocab";

// 界面语言（设计 §4.1）：只切换 Web 界面文案与日期格式；服务端的 why/next、通知、简报与内置提示词不随它变。
// 选择存在这台浏览器的 localStorage，不进账本、不跨设备同步；没选过时跟随浏览器语言。
export type Lang = "zh" | "en";
export type Choice = Lang | "auto";
/** 一份文案的两种语言。en 的类型写成 `typeof zh`，少一个键或签名不对时 tsc 报错。 */
export type Dict<T> = { zh: T; en: T };

const KEY = "apa.lang";

/** 按浏览器的语言偏好顺序取第一个 zh* 或 en*；都没有时用英文。 */
export function browserLang(langs: readonly string[] = typeof navigator === "undefined" ? [] : navigator.languages?.length ? navigator.languages : [navigator.language]): Lang {
  for (const l of langs) {
    const v = (l ?? "").toLowerCase();
    if (v.startsWith("zh")) return "zh";
    if (v.startsWith("en")) return "en";
  }
  return "en";
}

export function storedChoice(): Choice {
  const v = typeof localStorage === "undefined" ? null : localStorage.getItem(KEY);
  return v === "zh" || v === "en" ? v : "auto";
}

const resolve = (c: Choice): Lang => (c === "auto" ? browserLang() : c);
let current: Lang = resolve(storedChoice());
const listeners = new Set<() => void>();

function apply() {
  if (typeof document !== "undefined") document.documentElement.lang = current === "zh" ? "zh-CN" : "en";
}

function emit() {
  const next = resolve(storedChoice());
  if (next === current) return;
  current = next;
  apply();
  listeners.forEach((f) => f());
}

/** 本人在设置里选语言；"auto" 表示跟随浏览器（清掉本地记录）。 */
export function setChoice(c: Choice) {
  if (c === "auto") localStorage.removeItem(KEY);
  else localStorage.setItem(KEY, c);
  emit();
}

/** 启动时调用一次：设置 <html lang>，跟随浏览器语言变化与其他标签页的切换。 */
export function followLang() {
  apply();
  window.addEventListener("languagechange", emit);
  window.addEventListener("storage", (e) => e.key === KEY && emit());
}

const subscribe = (f: () => void) => {
  listeners.add(f);
  return () => listeners.delete(f);
};

/** 当前界面语言；非组件代码（回调里的错误文案）用它。 */
export const lang = () => current;
/** 日期时间格式用的 locale。 */
export const locale = () => (current === "zh" ? "zh-CN" : "en");

export function useLang(): Lang {
  return useSyncExternalStore(subscribe, lang, lang);
}

/** 取当前语言的那一份文案；语言切换时组件重新渲染。 */
export function useT<T>(d: Dict<T>): T {
  return d[useLang()];
}

/** 错误码的显示名（设计 §4.2）；词表里没有这个码时为 undefined。 */
export function codeLabel(code: string): string | undefined {
  return (vocab[current].error as Partial<Record<string, Entry>>)[code]?.label;
}

const withWhy = (label: string, why: string) => (current === "zh" ? `${label}（${why}）` : `${label} (${why})`);

/** 服务端错误的一行文字：已知错误码显示「当前语言的一句话（服务端 why）」，未知码原样显示 why 与 next。
 *  next 是给 CLI 与 agent 的下一步，不进这一行；ErrorNote 把 why、next 全文收在「详情」里。 */
export function errorText(e: unknown): string {
  if (e instanceof ApiError) return codeText(e.body);
  return e instanceof Error ? e.message : "request failed";
}

export function codeText(b: Pick<ApiErrorBody, "code" | "why" | "next">): string {
  const label = codeLabel(b.code);
  return label ? withWhy(label, b.why) : whyNext(b);
}

/** ErrorNote 的主行：错误码说得清的只显示那句话；只说了类别的码（请求有误、冲突、服务出错）仍要带上 why。 */
export function codeHeadline(b: Pick<ApiErrorBody, "code" | "why" | "next">): string {
  const label = codeLabel(b.code);
  if (!label) return whyNext(b);
  return GENERIC_ERROR.includes(b.code) ? withWhy(label, b.why) : label;
}

export const whyNext = (b: { why: string; next: string }) => (current === "zh" ? `${b.why}（${b.next}）` : `${b.why} (${b.next})`);
