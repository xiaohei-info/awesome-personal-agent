import { useState } from "react";
import { PageHead } from "../../app/page";
import { browserLang, setChoice, storedChoice, useT, type Choice } from "../../app/i18n";
import { LANG_NAMES } from "../../app/strings";
import { strings } from "./strings";
import { setRawShown, useRaw } from "../../app/raw";
import { Card, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";

// 界面语言（设计 §4.1）：存在这台浏览器的 localStorage，不进账本、不跨设备同步；默认跟随浏览器。
export function SettingsGeneralPage() {
  const t = useT(strings);
  const raw = useRaw();
  const [choice, setLocal] = useState<Choice>(storedChoice);
  const pick = (c: Choice) => {
    setLocal(c);
    setChoice(c);
  };
  const options: [Choice, string, string | undefined][] = [
    ["auto", t.auto + t.autoNow(LANG_NAMES[browserLang()]), undefined],
    ["zh", LANG_NAMES.zh, "zh-CN"],
    ["en", LANG_NAMES.en, "en"],
  ];
  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <section className="panel stack" aria-label={t.language}>
        <fieldset className="choice">
          <legend>{t.language}</legend>
          {options.map(([c, label, lang]) => (
            <label key={c}>
              <input type="radio" name="lang" value={c} checked={choice === c} onChange={() => pick(c)} />
              <span lang={lang}>
                <b>{label}</b>
              </span>
            </label>
          ))}
        </fieldset>
        <p className="sub">{t.scope}</p>
      </section>
      <section className="panel stack" aria-label={t.raw}>
        <label>
          <input type="checkbox" checked={raw} onChange={(e) => setRawShown(e.target.checked)} /> {t.raw}
        </label>
        <p className="sub">{t.rawHint}</p>
      </section>
      <Persona />
    </main>
  );
}

// 助手的语气（人格，§6.13、§11.2）：存在策略里，只在 Web 改；上限约 400 token 由服务端判定，这里的计数只是按同一规则的估算。
export const PERSONA_MAX = 400;
/** 与 kernel/policy.EstimateTokens 同一规则：非 ASCII 字符各计 1，其余非空白 ASCII 约每 4 个计 1。 */
export function estimateTokens(s: string): number {
  let wide = 0, ascii = 0;
  for (const ch of s) {
    if (ch.codePointAt(0)! > 0x7f) wide++;
    else if (!/\s/.test(ch)) ascii++;
  }
  return wide + Math.floor((ascii + 3) / 4);
}

function Persona() {
  const { docs, error, save } = usePolicyDocs(PERSONA_ROUTES);
  if (error) return <p role="alert">{error}</p>;
  return docs?.policy ? <PersonaCard doc={docs.policy} save={save} /> : null;
}
const PERSONA_ROUTES = ["policy"] as const;

function PersonaCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [text, setText] = useDraft(doc.persona, () => (doc.persona as string | undefined) ?? "");
  const n = estimateTokens(text);
  return (
    <Card title={t.persona} hint={t.personaHint} onSave={() => save("policy", { persona: text })}>
      <textarea aria-label={t.persona} value={text} onChange={(e) => setText(e.target.value)} />
      <div className="meter" aria-hidden="true"><b style={{ width: `${Math.min(100, (n / PERSONA_MAX) * 100)}%` }} /></div>
      <p className="sub">{t.personaUsed(n, PERSONA_MAX)}</p>
    </Card>
  );
}
