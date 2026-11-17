import { useMemo, useState, type ComponentProps } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

// 解析与渲染交给 react-markdown + remark-gfm（表格、任务清单、嵌套列表、列表项多段、缩进/围栏代码、嵌套行内、引用）；
// remark-breaks 保留单个换行（简报按「每条一行」写）。不装 raw HTML 插件、不用 innerHTML：正文里的 HTML 被转成文本显示，
// 所以不会有来自正文的 <script>。链接只放行 http(s)/mailto，其余（javascript:、data:、相对路径）只显示文字；
// 图片是外部资源，点开之前不创建 <img>，因此打开详情不会向外发请求。
const SAFE_LINK = /^(https?:\/\/|mailto:)/i;

type Node = { type: string; value?: string; depth?: number; children?: Node[] };
const text = (n: Node): string => n.value ?? (n.children ?? []).map(text).join("");

/** 原始 HTML 节点改成文本节点，让它原样显示而不是被丢掉。 */
function htmlAsText() {
  const walk = (n: Node) => {
    if (n.type === "html") n.type = "text";
    n.children?.forEach(walk);
  };
  return walk;
}

/** 正文开头若恰好重复了所在栏目的标题，就删掉这一个标题。 */
const dropFirstTitle = (title: string) => () => (root: Node) => {
  const h = root.children?.[0];
  if (h?.type === "heading" && text(h).trim() === title.trim()) root.children!.shift();
};

function ExternalImage({ alt, src }: { alt: string; src: string }) {
  const t = useT(strings);
  const [shown, setShown] = useState(false);
  if (!/^https?:\/\//i.test(src)) return <span>{alt}</span>;
  if (shown) return <img alt={alt} src={src} referrerPolicy="no-referrer" style={{ maxWidth: "100%" }} />;
  return <button type="button" onClick={() => setShown(true)}>{t.loadImage(alt || src)}</button>;
}

const base: Components = {
  a: ({ href, children }) => (href && SAFE_LINK.test(href) ? <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> : <>{children}</>),
  img: ({ src, alt }) => <ExternalImage alt={alt ?? ""} src={typeof src === "string" ? src : ""} />,
  table: ({ children }) => <div className="md-table"><table>{children}</table></div>,
};
// 组件对象按 from 缓存：每次渲染换新函数会让 React 重建节点，丢掉「点开才加载的图片」等局部状态。
const byFrom = new Map<number, Components>();
function componentsFor(from: number): Components {
  let c = byFrom.get(from);
  if (!c) {
    c = { ...base };
    for (let n = 1; n <= 6; n++) {
      const T = `h${Math.min(6, n - 1 + from)}` as "h1";
      c[`h${n}` as "h1"] = ({ children }: ComponentProps<"h1">) => <T>{children}</T>;
    }
    byFrom.set(from, c);
  }
  return c;
}

/**
 * from：「#」对应的标题级别（嵌在页面里时下移，避免与页面标题抢层级）；
 * dropTitle：正文开头若恰好重复了所在栏目的标题，就不再画一遍。
 */
export function Markdown({ source, from = 1, dropTitle }: { source: string; from?: number; dropTitle?: string }) {
  const plugins = useMemo(() => [remarkGfm, remarkBreaks, htmlAsText, ...(dropTitle !== undefined ? [dropFirstTitle(dropTitle)] : [])], [dropTitle]);
  return (
    <div className="markdown">
      <ReactMarkdown components={componentsFor(from)} remarkPlugins={plugins} urlTransform={(u) => u}>
        {source.replace(/\r\n?/g, "\n")}
      </ReactMarkdown>
    </div>
  );
}
