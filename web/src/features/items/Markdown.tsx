import { Fragment, useState, type ReactNode } from "react";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

// 最小 Markdown 子集（标题、列表、段落、围栏代码、行内粗体/斜体/代码/链接/图片），直接产出 React 元素，
// 从不走 innerHTML：正文里的 HTML 一律显示为文本，所以不会有来自正文的 <script>。
// 链接只放行 http(s)/mailto；图片是外部资源，点开之前不创建 <img>，因此打开详情不会向外发请求。
// shortcut: 不支持嵌套列表、表格、引用，需要时换成带净化的成熟库。
const SAFE_LINK = /^(https?:\/\/|mailto:)/i;
const INLINE = /(!?)\[([^\]]*)\]\(([^)\s]*)\)|`([^`]+)`|\*\*([^*]+)\*\*|\*([^*]+)\*/g;

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
    else out.push(<em key={k}>{m[6]}</em>);
    last = m.index! + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function Markdown({ source }: { source: string }) {
  const lines = source.replace(/\r\n?/g, "\n").split("\n");
  const blocks: ReactNode[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const key = blocks.length;
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    const li = line.match(/^\s*([-*+]|\d+\.)\s+/);
    if (!line.trim()) i++;
    else if (line.startsWith("```")) {
      const code: string[] = [];
      for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
      i++;
      blocks.push(<pre key={key}><code>{code.join("\n")}</code></pre>);
    } else if (h) {
      const T = `h${h[1].length}` as "h1";
      blocks.push(<T key={key}>{inline(h[2])}</T>);
      i++;
    } else if (li) {
      const ordered = /\d/.test(li[1]);
      const rows: string[] = [];
      for (; i < lines.length; i++) {
        const m = lines[i].match(/^\s*([-*+]|\d+\.)\s+(.*)$/);
        if (!m || /\d/.test(m[1]) !== ordered) break;
        rows.push(m[2]);
      }
      const items = rows.map((r, n) => <li key={n}>{inline(r)}</li>);
      blocks.push(ordered ? <ol key={key}>{items}</ol> : <ul key={key}>{items}</ul>);
    } else {
      const para: string[] = [];
      for (; i < lines.length && lines[i].trim() && !/^(#{1,6}\s|```)/.test(lines[i]) && !/^\s*([-*+]|\d+\.)\s+/.test(lines[i]); i++) para.push(lines[i]);
      blocks.push(<p key={key}>{para.map((p, n) => <Fragment key={n}>{n > 0 && <br />}{inline(p)}</Fragment>)}</p>);
    }
  }
  return <div className="markdown">{blocks}</div>;
}
