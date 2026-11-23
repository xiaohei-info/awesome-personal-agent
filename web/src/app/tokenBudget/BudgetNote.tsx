import { useEffect, useState } from "react";
import { useSession } from "../session";
import { useT } from "../i18n";
import { CHARS_PER_TOKEN, DEFAULT_BUDGET_TOKENS, MAX_BUDGET_TOKENS, MIN_BUDGET_TOKENS } from "./constants";
import { strings } from "./strings";

/** 写作模型的名字（GET /settings/model-endpoints 的 generate_endpoint.model）；没配或读不到为空串。 */
export function useWritingModel(): string {
  const { api } = useSession();
  const [name, setName] = useState("");
  useEffect(() => {
    api.get<{ generate_endpoint?: { model?: string } | null }>("/settings/model-endpoints").then(
      (d) => setName(d.generate_endpoint?.model ?? ""),
      () => setName(""),
    );
  }, [api]);
  return name;
}

/** 资料上限输入框下的说明：先是这个数管什么（what，各页自写），再是估算系数、默认与范围，最后是所配模型与超出的后果。 */
export function BudgetNote({ what, model }: { what: string; model: string }) {
  const t = useT(strings);
  return (
    <p className="sub" role="note">
      {what}<br />{t.count(CHARS_PER_TOKEN)}<br />{t.range(DEFAULT_BUDGET_TOKENS, DEFAULT_BUDGET_TOKENS * CHARS_PER_TOKEN, MIN_BUDGET_TOKENS, MAX_BUDGET_TOKENS)}<br />{model ? t.model(model) : t.noModel}<br />{t.fit}<br />{t.over}
    </p>
  );
}

export const useBudgetBad = () => {
  const t = useT(strings);
  return t.bad(MIN_BUDGET_TOKENS, MAX_BUDGET_TOKENS);
};
