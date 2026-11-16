import { useT } from "../../app/i18n";
import { strings } from "./strings";

// 链接型材料（§12.8）：位置只读；"打开"在库内是服务端渲染好的 open_url，网址型是网址本身。
export interface LinkInfo {
  form?: string;
  link?: string;
  open_url?: string;
}

export const isLink = (m: LinkInfo) => m.form === "link";

export function LinkWhere({ m }: { m: LinkInfo }) {
  const t = useT(strings);
  const href = m.open_url || (/^https?:\/\//.test(m.link ?? "") ? m.link : "");
  return (
    <>
      <code>{m.link}</code>{" "}
      {href ? (
        <a href={href} target="_blank" rel="noopener noreferrer">{t.open}</a>
      ) : (
        <span className="sub">{t.noOpen}</span>
      )}
    </>
  );
}
