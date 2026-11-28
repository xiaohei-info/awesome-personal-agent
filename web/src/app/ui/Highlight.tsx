import { Fragment } from "react";

// 检索结果里标出命中的词（规格：结果高亮命中）。查询按空白拆词，去掉引号；中文长词另按两字一组也标，
// 与检索的二元组切分一致（整句搜到的结果，原文里往往只有其中几段）。只做显示，不影响检索。
const CJK = /[\u3400-\u9fff]/;

export function terms(q: string): string[] {
  const out = new Set<string>();
  for (const w of q.replace(/["“”「」]/g, " ").split(/\s+/).filter(Boolean)) {
    out.add(w);
    if (CJK.test(w) && w.length > 2) for (let i = 0; i + 2 <= w.length; i++) out.add(w.slice(i, i + 2));
  }
  return [...out].sort((a, b) => b.length - a.length);
}

const esc = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

export function Highlight({ text, q }: { text: string; q: string }) {
  const ts = terms(q);
  if (!ts.length || !text) return <>{text}</>;
  const re = new RegExp("(" + ts.map(esc).join("|") + ")", "gi");
  return <>{text.split(re).map((part, i) => (i % 2 ? <mark key={i}>{part}</mark> : <Fragment key={i}>{part}</Fragment>))}</>;
}
