import { useT } from "../../app/i18n";
import { Card, reject, useDraft, type Doc, type Save } from "../../app/policy-cards";
import { strings } from "./strings";

// 所有 Agent 共用的后台运行时限（设计 §13.6、§6.13）：唯一语义是防卡死，不影响事项本身。
// 服务端按秒存（background_run_timeout_sec），页面按小时编辑（允许小数如 0.5）；范围 0.25–24 小时，默认 4 小时。
export const DEFAULT_TIMEOUT_HOURS = 4;
export const MIN_TIMEOUT_HOURS = 0.25;
export const MAX_TIMEOUT_HOURS = 24;

function formatHours(sec: number): string {
  return String(Math.round((sec / 3600) * 100) / 100);
}

export function RunTimeoutCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const stored = typeof doc.background_run_timeout_sec === "number" && doc.background_run_timeout_sec > 0
    ? doc.background_run_timeout_sec
    : DEFAULT_TIMEOUT_HOURS * 3600;
  const [val, setVal, dirty] = useDraft(doc.background_run_timeout_sec, () => formatHours(stored));
  const isDefault = Number(val) === DEFAULT_TIMEOUT_HOURS;

  const onSave = () => {
    const trimmed = val.trim();
    const hours = Number(trimmed);
    if (!trimmed || !Number.isFinite(hours) || hours < MIN_TIMEOUT_HOURS || hours > MAX_TIMEOUT_HOURS) {
      return reject(t.runTimeoutBad);
    }
    return save("policy", { background_run_timeout_sec: Math.round(hours * 3600) });
  };

  return (
    <Card title={t.runTimeout} hint={t.runTimeoutHint} dirty={dirty} onSave={onSave} id="background-run-timeout">
      <div className="row">
        <label>
          {t.runTimeout}
          <input
            type="number"
            min={MIN_TIMEOUT_HOURS}
            max={MAX_TIMEOUT_HOURS}
            step="any"
            value={val}
            onChange={(e) => setVal(e.target.value)}
          />
          <span>{t.runTimeoutUnit}</span>
        </label>
        {isDefault
          ? <span className="badge ok">{t.runTimeoutDefault}</span>
          : <button type="button" onClick={() => setVal(String(DEFAULT_TIMEOUT_HOURS))}>{t.runTimeoutRestore}</button>}
      </div>
      <p className="sub">{t.runTimeoutNote}</p>
    </Card>
  );
}
