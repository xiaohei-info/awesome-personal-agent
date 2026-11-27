import { useT } from "../../app/i18n";
import { Card, useDraft, type Doc, type Save } from "../../app/policy-cards";
import { strings } from "./strings";

const LEVELS = ["low", "medium", "high"] as const;
const DEFAULTS = { judge: "low", write: "medium", background: "medium" } as const;
type Key = keyof typeof DEFAULTS;

// 按用途设思考强度（设计 §8.1、§13.6）：值缺省时服务端给的就是生效的默认值。
export function ThinkingCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [v, setV, dirty] = useDraft<Record<Key, string>>(doc.thinking, () => ({ ...DEFAULTS, ...doc.thinking }));
  return (
    <Card title={t.thinking} hint={t.thinkingHint} dirty={dirty} onSave={() => save("policy", { thinking: v })} id="thinking">
      {(Object.keys(DEFAULTS) as Key[]).map((k) => (
        <label key={k}>
          {t.thinkingUse[k]}
          <select value={v[k]} onChange={(e) => setV({ ...v, [k]: e.target.value })}>
            {LEVELS.map((l) => <option key={l} value={l}>{t.thinkingLevel[l]}{l === DEFAULTS[k] ? t.thinkingDefault : ""}</option>)}
          </select>
        </label>
      ))}
    </Card>
  );
}
