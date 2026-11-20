import { useLang } from "./i18n";
import { vocab, type NoticeParams } from "./vocab";

// 内核给的类别码说人话（界面设计 §5.2、§8.2）：问题的 ask.code、运行的 fault.code 都按词表渲染。
// 判定在服务端：这里只查表，查不到（旧格式、更新的内核）时返回 undefined，调用方照原文显示。

export interface Fault {
  code: string;
  params?: Record<string, string>;
}

/** 问题的一句话；没有类别码或词表里没有时为 undefined。 */
export function useAskSay(): (a?: { code?: string; params?: NoticeParams }) => string | undefined {
  const say = vocab[useLang()].askCode as Record<string, (p: NoticeParams) => string>;
  return (a) => (a?.code && say[a.code] ? say[a.code](a.params) : undefined);
}

/** 运行故障：出了什么事、下一步做什么；没有类别码或词表里没有时为 undefined。 */
export function useFaultSay(): (f?: Fault | null) => { say: string; next: string } | undefined {
  const t = vocab[useLang()].runFault as Record<string, { say: (p: NoticeParams) => string; next: string }>;
  return (f) => (f?.code && t[f.code] ? { say: t[f.code].say(f.params), next: t[f.code].next } : undefined);
}
