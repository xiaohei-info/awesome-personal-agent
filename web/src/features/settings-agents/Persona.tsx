import { useEffect, useRef, useState } from "react";
import { useLang, useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { LANG_NAMES } from "../../app/strings";
import { Card, Seg, useDraft, type Doc, type Save } from "../../app/policy-cards";
import { strings } from "./strings";

// 所有 Agent 共用：对我说话的语言与语气（人格，§6.13、§11.2）。存在策略里，只在 Web 改；上限约 400 token 由服务端判定，
// 这里的计数只是按同一规则的估算。原在「设置 › 通用」，按界面设计 §4 移到 Agent 列表页，那里不再出现。
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

export function Persona({ doc, save }: { doc: Doc; save: Save }) {
  const { api } = useSession();
  const lang = useLang();
  const said = (doc.persona_lang as string | undefined) ?? "";
  // 内置默认原文由内核给出（§11.2）：已存的语言优先，「跟随系统」时按界面语言；取不到时按空文本处理，编辑框仍可写自己的。
  const [def, setDef] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    api.get<{ default?: string }>("/context/persona?locale=" + lang).then((r) => live && setDef(r.default ?? ""), () => live && setDef(""));
    return () => { live = false; };
  }, [api, lang, said]);
  return def !== null ? (
    <>
      <PersonaLangCard doc={doc} save={save} />
      <PersonaCard doc={doc} def={def} save={save} />
    </>
  ) : null;
}

// 说哪种语言：与界面语言分开；「跟随系统」按运行 Agent 那台机器的区域设置。单选，可随时改回，即改即存（§6.1）。
function PersonaLangCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [v, setV, dirty] = useDraft(doc.persona_lang, () => (doc.persona_lang as string | undefined) ?? "");
  return (
    <Card title={t.personaLang} hint={t.personaLangHint} dirty={dirty} onSave={() => save("policy", { persona_lang: v })}>
      <Seg label={t.personaLang} value={v} onChange={setV} options={[["", t.personaLangSystem], ["zh", LANG_NAMES.zh], ["en", LANG_NAMES.en]]} />
    </Card>
  );
}

// 策略里的人格为空 = 用内置默认：编辑框显示默认文本并标「默认」；改了就成为自定义，文本等于默认时存回空串，继续跟随新版默认。
function PersonaCard({ doc, def, save }: { doc: Doc; def: string; save: Save }) {
  const t = useT(strings);
  const stored = ((doc.persona as string | undefined) ?? "").trim() !== "";
  const [text, setText, dirty] = useDraft(doc.persona, () => (stored ? (doc.persona as string) : def));
  // 默认原文换了语言：还停在默认上的文字跟着换，本人已改的不动。
  const prev = useRef(def);
  useEffect(() => {
    const was = prev.current;
    prev.current = def;
    setText((x) => (x === was ? def : x));
  }, [def]); // eslint-disable-line react-hooks/exhaustive-deps
  const n = estimateTokens(text);
  const isDefault = text === def;
  return (
    <Card title={t.persona} hint={t.personaHint} dirty={dirty} onSave={() => save("policy", { persona: isDefault ? "" : text })}>
      <div className="row">
        {isDefault ? <span className="badge ok">{t.personaDefault}</span> : <button type="button" onClick={() => setText(def)}>{t.personaRestore}</button>}
      </div>
      <textarea aria-label={t.persona} value={text} onChange={(e) => setText(e.target.value)} />
      <div className="meter" aria-hidden="true"><b style={{ width: `${Math.min(100, (n / PERSONA_MAX) * 100)}%` }} /></div>
      <p className="sub">{t.personaUsed(n, PERSONA_MAX)}</p>
    </Card>
  );
}
