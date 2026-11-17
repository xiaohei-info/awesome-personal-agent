import { Fragment, useState, type ReactNode } from "react";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

// 最小 Markdown 子集（标题、嵌套列表、表格、引用、分隔线、段落、围栏代码、行内粗体/斜体/代码/链接/图片），直接产出 React 元素，
// 从不走 innerHTML：正文里的 HTML 一律显示为文本，所以不会有来自正文的 <script>。
// 链接只放行 http(s)/mailto；图片是外部资源，点开之前不创建 <img>，因此打开详情不会向外发请求。
// shortcut: 不支持列表项内的多段与缩进代码块、表格单元格内的竖线转义以外的写法，需要时换成带净化的成熟库。
const SAFE_LINK = /^(https?:\/\/|mailto:)/i;
const INLINE = /(!?)\[([^\]]*)\]\(([^)\s]*)\)|`([^`]+)`|\*\*([^*]+)\*\*|\*([^*]+)\*|(https?:\/\/[^\s<>()]*[^\s<>().,;:!?，。；：！？])/g;

function ExternalImage({ alt, src }: { alt: string; src: string }) {
  const t = useT(strings);
  const [shown, setShown] = useState(false);
  if (!/^https?:\/\//i.test(src)) return <span>{alt}</span>;
  if (shown) return <img alt={alt} src={src} referrerPolicy="no-referrer" style={{ maxWidth: "100%" }} />;
  return <button type="button" onClick={() => setShown(true)}>{t.loadImage(alt || src)}</button>;
}

function inline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  for (const m of text.matchAll(INLINE)) {
    if (m.index! > last) out.push(text.slice(last, m.index));
    const k = m.index;
    if (m[3] !== undefined) {
      if (m[1]) out.push(<ExternalImage key={k} alt={m[2]} src={m[3]} />);
      else if (SAFE_LINK.test(m[3])) out.push(<a key={k} href={m[3]} target="_blank" rel="noopener noreferrer">{m[2]}</a>);
      else out.push(m[0]);
    } else if (m[4] !== undefined) out.push(<code key={k}>{m[4]}</code>);
    else if (m[5] !== undefined) out.push(<strong key={k}>{m[5]}</strong>);
    else if (m[7] !== undefined) out.push(<a key={k} href={m[7]} target="_blank" rel="noopener noreferrer">{m[7]}</a>);
    else out.push(<em key={k}>{m[6]}</em>);
    last = m.index! + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

const LI = /^(\s*)([-*+]|\d+\.)\s+(.*)$/;
const indentOf = (s: string) => s.replace(/\t/g, "    ").length - s.replace(/\t/g, "    ").trimStart().length;
const SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;
const cells = (l: string) => l.trim().replace(/^\|/, "").replace(/\|$/, "").split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
const isTable = (lines: string[], i: number) => lines[i].includes("|") && i + 1 < lines.length && lines[i + 1].includes("-") && SEP.test(lines[i + 1]);

/** 从 lines[i] 起解析一组同缩进的列表，更深缩进的行成为上一项的子列表。 */
function list(lines: string[], i: number, base: number): [ReactNode, number] {
  const ordered = /\d/.test(lines[i].match(LI)![2]);
  const items: ReactNode[][] = [];
  while (i < lines.length) {
    const m = lines[i].match(LI);
    if (!m) break;
    const ind = indentOf(lines[i]);
    if (ind < base || (ind === base && /\d/.test(m[2]) !== ordered)) break;
    if (ind > base) {
      if (!items.length) break;
      let sub: ReactNode;
      [sub, i] = list(lines, i, ind);
      items[items.length - 1].push(sub);
      continue;
    }
    items.push(inline(m[3]));
    i++;
  }
  const rows = items.map((c, n) => <li key={n}>{c}</li>);
  return [ordered ? <ol key={i}>{rows}</ol> : <ul key={i}>{rows}</ul>, i];
}

/**
 * from：「#」对应的标题级别（嵌在页面里时下移，避免与页面标题抢层级）；
 * dropTitle：正文开头若恰好重复了所在栏目的标题，就不再画一遍。
 */
export function Markdown({ source, from = 1, dropTitle }: { source: string; from?: number; dropTitle?: string }) {
  const lines = source.replace(/\r\n?/g, "\n").split("\n");
  const blocks: ReactNode[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const key = blocks.length;
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (!line.trim()) i++;
    else if (line.startsWith("```")) {
      const code: string[] = [];
      for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
      i++;
      blocks.push(<pre key={key}><code>{code.join("\n")}</code></pre>);
    } else if (h) {
      i++;
      if (blocks.length === 0 && dropTitle !== undefined && h[2].trim() === dropTitle.trim()) continue;
      const T = `h${Math.min(6, h[1].length - 1 + from)}` as "h1";
      blocks.push(<T key={key}>{inline(h[2])}</T>);
    } else if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line) && !LI.test(line)) {
      blocks.push(<hr key={key} />);
      i++;
    } else if (LI.test(line)) {
      let node: ReactNode;
      [node, i] = list(lines, i, indentOf(line));
      blocks.push(<Fragment key={key}>{node}</Fragment>);
    } else if (isTable(lines, i)) {
      const head = cells(line);
      const rows: string[][] = [];
      for (i += 2; i < lines.length && lines[i].trim() && lines[i].includes("|"); i++) rows.push(cells(lines[i]));
      blocks.push(
        <div key={key} className="md-table">
          <table>
            <thead><tr>{head.map((c, n) => <th key={n}>{inline(c)}</th>)}</tr></thead>
            <tbody>{rows.map((r, n) => <tr key={n}>{head.map((_, k) => <td key={k}>{inline(r[k] ?? "")}</td>)}</tr>)}</tbody>
          </table>
        </div>,
      );
    } else if (/^\s*>/.test(line)) {
      const q: string[] = [];
      for (; i < lines.length && /^\s*>/.test(lines[i]); i++) q.push(lines[i].replace(/^\s*>\s?/, ""));
      blocks.push(<blockquote key={key}><Markdown source={q.join("\n")} from={from} /></blockquote>);
    } else {
      const para: string[] = [];
      for (; i < lines.length && lines[i].trim() && !/^(#{1,6}\s|```|\s*>)/.test(lines[i]) && !LI.test(lines[i]) && !isTable(lines, i); i++) para.push(lines[i]);
      blocks.push(<p key={key}>{para.map((p, n) => <Fragment key={n}>{n > 0 && <br />}{inline(p)}</Fragment>)}</p>);
    }
  }
  return <div className="markdown">{blocks}</div>;
}
