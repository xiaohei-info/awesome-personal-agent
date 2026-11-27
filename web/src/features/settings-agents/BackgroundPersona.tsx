import { useT } from "../../app/i18n";
import { Card, useDraft, type Doc, type Save } from "../../app/policy-cards";
import { strings } from "./strings";

// 后台运行是否带 Agent 平时的人格和记忆（设计 §13.6）：默认关，本人自己开。
export function BackgroundPersonaCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [on, setOn, dirty] = useDraft(doc.background_persona, () => doc.background_persona === true);
  return (
    <Card title={t.persona} hint={t.personaHint} dirty={dirty} onSave={() => save("policy", { background_persona: on })} id="background-persona">
      <label className="row">
        <input type="checkbox" checked={on} onChange={(e) => setOn(e.target.checked)} />
        {t.personaLabel}
      </label>
      <p className="sub">{t.personaNote}</p>
    </Card>
  );
}
