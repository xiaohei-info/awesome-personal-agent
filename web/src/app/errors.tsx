import { useEffect, useRef } from "react";
import { ApiError } from "./api";
import { codeHeadline, errorText, useLang, whyNext } from "./i18n";
import { common } from "./strings";

/** 一条错误（界面设计 §8.2；设计 §4.2）：主行是当前语言的一句话；服务端的原因说明（why、next）收在可展开的「详情」里。 */
export function ErrorNote({ error, reveal }: { error: unknown; reveal?: boolean }) {
  // reveal：操作失败的提示可能在视口之外，出现时滚到眼前并聚焦。
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!reveal) return;
    ref.current?.focus({ preventScroll: true });
    ref.current?.scrollIntoView?.({ block: "nearest" });
  }, [error, reveal]);
  const t = common[useLang()];
  const api = error instanceof ApiError ? error.body : null;
  const text = api ? codeHeadline(api) : errorText(error);
  const detail = api ? whyNext(api) : "";
  return (
    <div ref={ref} tabIndex={reveal ? -1 : undefined} role="alert" className="error-note">
      <p>{text}</p>
      {detail && detail !== text && (
        <details>
          <summary>{t.details}</summary>
          <p lang="en">{detail}</p>
        </details>
      )}
    </div>
  );
}
