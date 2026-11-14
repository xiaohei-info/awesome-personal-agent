import { Fragment, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useLang } from "./i18n";
import { PageHead } from "./page";
import { settingsGroups, type Entry } from "./registry";
import { useSession } from "./session";

/** 设置首页（手机）：按组列出设置页，每项带当前值摘要，点进去就是那一页（设计 §3.1、§6.11）。 */
export function SettingsHome({ entry }: { entry: Entry }) {
  const lang = useLang();
  const { api } = useSession();
  const [sums, setSums] = useState<Record<string, string>>({});
  useEffect(() => {
    let live = true;
    entry.features.forEach((f) =>
      f.summary?.(api, lang).then(
        (s) => live && setSums((m) => ({ ...m, [f.id]: s })),
        () => undefined, // 摘要只是提示，取不到就不显示
      ),
    );
    return () => {
      live = false;
    };
  }, [api, entry, lang]);
  return (
    <main>
      <PageHead>{entry.title}</PageHead>
      {settingsGroups(entry, lang).map((g) => (
        <Fragment key={g.title}>
          <ul className="panel menu" aria-label={g.title}>
            {g.features.map((f) => (
              <li key={f.id}>
                <Link to={"/" + f.path} className={f.id === "purge" ? "danger" : undefined}>
                  <span>
                    {f.title[lang]}
                    {sums[f.id] && <small>{sums[f.id]}</small>}
                  </span>
                  <span aria-hidden="true">›</span>
                </Link>
              </li>
            ))}
          </ul>
        </Fragment>
      ))}
    </main>
  );
}
