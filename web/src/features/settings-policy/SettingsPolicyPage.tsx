import { useState } from "react";
import { useLang, useT } from "../../app/i18n";
import { ITEM_KIND, MATERIAL_KIND, WEB_ONLY } from "../../app/ids";
import { EntityPick } from "../../app/names";
import { Pick, Val, useLabel } from "../../app/values";
import { vocab } from "../../app/vocab";
import { Card, PolicyPage, Seg, useDraft, usePolicyDocs, type Doc, type Save } from "./shared";
import { strings } from "./strings";

// 设置 › 我来拍板（路径 /settings，沿用原「策略与人格」的地址）：web_only 类别与匹配规则、材料写入策略。
// 打扰策略在「提醒与打扰」，权限上限与两份白名单在「后台权限」，人格在「通用」。
const ROUTES = ["policy"] as const;

export function SettingsPolicyPage() {
  const t = useT(strings);
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const p = docs?.policy;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!p} onRetry={() => void load()}>
      {p && (
        <>
          <WebOnlyCard doc={p} save={save} />
          <MaterialCard doc={p} save={save} />
        </>
      )}
    </PolicyPage>
  );
}

interface Rule { kind?: string; tag?: string; project?: string; category: string }
type RuleRow = [kind: string, tag: string, project: string, category: string];

// 类别保持文档里的顺序；勾上内置类别时追加到末尾（服务端会排序规整）。规则四个字段照旧逐一发送，空的发空串。
function WebOnlyCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [cats, setCats] = useDraft(doc.web_only, () => [...(doc.web_only.categories as string[])]);
  const [rules, setRules] = useDraft(doc.web_only, () => (doc.web_only.rules as Rule[]).map((r): RuleRow => [r.kind ?? "", r.tag ?? "", r.project ?? "", r.category]));
  const [custom, setCustom] = useState("");
  const all = [...WEB_ONLY, ...cats.filter((c) => !(WEB_ONLY as readonly string[]).includes(c))];
  const toggle = (c: string) => setCats(cats.includes(c) ? cats.filter((x) => x !== c) : [...cats, c]);
  const addCustom = () => {
    const c = custom.trim();
    if (c && !cats.includes(c)) setCats([...cats, c]);
    setCustom("");
  };
  const setRule = (i: number, j: number, v: string) => setRules(rules.map((r, n) => (n === i ? (r.map((x, m) => (m === j ? v : x)) as RuleRow) : r)));
  const kinds = (v: string) => (v && !(ITEM_KIND as readonly string[]).includes(v) ? [...ITEM_KIND, v] : [...ITEM_KIND]);
  const catsFor = (v: string) => (v && !cats.includes(v) ? [...cats, v] : cats);
  return (
    <Card title={t.webOnly} onSave={() => save("policy", {
      web_only: { categories: cats, rules: rules.map(([kind, tag, project, category]) => ({ kind, tag, project, category })) },
    })}>
      <div className="chips">
        {all.map((c) => (
          <label key={c}>
            <input type="checkbox" checked={cats.includes(c)} onChange={() => toggle(c)} />
            <Val k="webOnly" id={c} />
          </label>
        ))}
        <span className="sentence">
          <input aria-label={t.custom} placeholder={t.custom} value={custom} onChange={(e) => setCustom(e.target.value)} onKeyDown={(e) => e.key === "Enter" && (e.preventDefault(), addCustom())} />
          <button type="button" onClick={addCustom} disabled={!custom.trim()}>{t.addCustom}</button>
        </span>
      </div>
      <p className="sub">{t.rulesHead}</p>
      {rules.map((r, i) => (
        <div key={i} role="group" aria-label={t.rule(i + 1)} className="sentence">
          <span>{t.when}</span>
          <Pick k="itemKind" label={t.ruleKind(i + 1)} value={r[0]} ids={kinds(r[0])} empty={t.any} onChange={(v) => setRule(i, 0, v)} />
          <span>{t.tag}</span>
          <input aria-label={t.ruleTag(i + 1)} placeholder={t.any} value={r[1]} onChange={(e) => setRule(i, 1, e.target.value)} />
          <span>{t.project}</span>
          <EntityPick kind="project" label={t.ruleProject(i + 1)} value={r[2]} empty={t.any} onChange={(v) => setRule(i, 2, v)} />
          <span>→ {t.counts}</span>
          <Pick k="webOnly" label={t.ruleCategory(i + 1)} value={r[3]} ids={catsFor(r[3])} onChange={(v) => setRule(i, 3, v)} />
          <button type="button" aria-label={t.delRule(i + 1)} onClick={() => setRules(rules.filter((_, n) => n !== i))}>✕</button>
        </div>
      ))}
      <div>
        <button type="button" onClick={() => setRules([...rules, ["", "", "", cats[0] ?? ""]])}>＋ {t.addRule}</button>
      </div>
    </Card>
  );
}

// 每种文档一行：显示的值是它登记的策略，没登记的取「其他类型」（default）；只有改过的行才写进 kinds，没改的原样带回。
// 规范（guideline）的写入策略在内核里恒为 proposed、不受策略放宽（kernel/materials），所以这一行锁定，不给开关。
function MaterialCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const lang = useLang();
  const label = useLabel();
  const [def, setDef] = useDraft(doc.material_write, () => doc.material_write.default as string);
  const [kinds, setKinds] = useDraft(doc.material_write, () => ({ ...(doc.material_write.kinds as Record<string, string>) }));
  const rows = [...MATERIAL_KIND, ...Object.keys(kinds).filter((k) => !(MATERIAL_KIND as readonly string[]).includes(k))];
  const opts: [string, string][] = (["direct", "proposed"] as const).map((w) => [w, vocab[lang].write[w].label]);
  return (
    <Card title={t.materials} hint={t.materialsHint} onSave={() => save("policy", { material_write: { default: def, kinds } })}>
      <div>
        {rows.map((k) => (
          <div key={k} className="line">
            <span><Val k="materialKind" id={k} /></span>
            {k === "guideline" ? (
              <span className="badge plain">🔒 {t.locked}</span>
            ) : (
              <Seg label={label("materialKind", k)} value={kinds[k] ?? def} options={opts} onChange={(v) => setKinds({ ...kinds, [k]: v })} />
            )}
          </div>
        ))}
        <div className="line">
          <span>{t.other}</span>
          <Seg label={t.other} value={def} options={opts} onChange={setDef} />
        </div>
      </div>
    </Card>
  );
}
