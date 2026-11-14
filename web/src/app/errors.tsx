import { ApiError } from "./api";
import { codeHeadline, errorText, useLang, whyNext } from "./i18n";
import { common } from "./strings";

/** 一条错误（设计 §2、§4.2）：主行是当前语言的一句话；服务端的原因说明（why、next）收在可展开的「详情」里。 */
export function ErrorNote({ error }: { error: unknown }) {
  const t = common[useLang()];
  const api = error instanceof ApiError ? error.body : null;
  const text = api ? codeHeadline(api) : errorText(error);
  const detail = api ? whyNext(api) : "";
  return (
    <div role="alert" className="error-note">
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
