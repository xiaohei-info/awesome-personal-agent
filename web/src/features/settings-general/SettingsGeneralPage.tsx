import { useState } from "react";
import { PageHead } from "../../app/page";
import { browserLang, setChoice, storedChoice, useT, type Choice } from "../../app/i18n";
import { LANG_NAMES } from "../../app/strings";
import { strings } from "./strings";
import { setRawShown, useRaw } from "../../app/raw";

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
      <PageHead lede={t.lede}>{t.title}</PageHead>
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
    </main>
  );
}
