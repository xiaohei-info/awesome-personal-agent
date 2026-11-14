import { useT } from "../../app/i18n";
import { strings } from "./strings";
// web_only 初筛未返回时，内核只对 Web 凭据保留完整决策请求并置 ask.screening_pending（设计 §14.4）。
// 本人此时仍可直接回答；标记只提示"别的渠道暂时看不到、也不能代答"，不改变回答逻辑。
export function ScreeningBadge({ pending }: { pending?: boolean }) {
  const t = useT(strings);
  if (!pending) return null;
  return <span className="badge warn" title={t.screeningTip}>{t.screening}</span>;
}
