import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { COUNTS_EVENT, PageHead } from "../../app/page";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useNames } from "../../app/names";
import { useLabel } from "../../app/values";
import { useRaw } from "../../app/raw";
import { strings } from "./strings";

// 形状对应 GET /v1/judgments/spot-check、POST /v1/judgments/{id}/review 与 /v1/settings/judgment 的 spot_check 段（附录 B.2，主文档 §8.10）。
// 「该看」由服务端把事件转成建议并记为误丢；本页只搬运结论。抽查条数是一次策略变更，只写 spot_check 这一段，其余判断设置不动。
interface Entry { judgment: string; event: string; source: string; subject?: string; link?: string; template: string; judged_at: string; results: string[] }
interface Settings { spot_check: { per_brief: number } }

export function SpotCheckPage() {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const { api } = useSession();
  const source = useNames("source");
  const focus = useSearchParams()[0].get("judgment");
  const [pool, setPool] = useState<Entry[] | null>(null);
  const [per, setPer] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const on = useRef<HTMLLIElement>(null);

  const load = useCallback(async () => {
    const [p, s] = await Promise.all([
      api.get<{ spot_check: Entry[] }>("/judgments/spot-check"),
      api.get<Settings>("/settings/judgment"),
    ]);
    setPool(p.spot_check);
    setPer(String(s.spot_check.per_brief));
  }, [api]);
  const guard = useCallback(async (f: () => Promise<void>) => {
    try {
      await f();
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, []);
  useEffect(() => { void guard(load); }, [guard, load]);
  // 从简报里的抽检链接（?judgment=…）进来时，把那一条滚到眼前。
  useEffect(() => on.current?.scrollIntoView?.({ block: "center" }), [pool]);

  const name = (r: string) => (r === "should_see" ? t.shouldSee : r === "correct" ? t.correct : label("spotResult", r));
  const review = (e: Entry, result: string) =>
    guard(async () => {
      setNotice(null);
      await api.post(`/judgments/${encodeURIComponent(e.judgment)}/review`, { result });
      await load();
      window.dispatchEvent(new Event(COUNTS_EVENT));
      setNotice(t.reviewed(e.subject || t.noSubject, label("spotResult", result)));
    });
  const savePer = () =>
    guard(async () => {
      setNotice(null);
      const n = Number(per);
      if (per.trim() === "" || !Number.isInteger(n) || n < 0) throw new Error(t.perBriefInvalid);
      const s = await api.put<Settings>("/settings/judgment", { spot_check: { per_brief: n } });
      setPer(String(s.spot_check.per_brief));
      setNotice(t.perBriefSaved(s.spot_check.per_brief));
    });

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      {notice && <p role="status">{notice}</p>}
      {pool === null ? (
        error == null && <p role="status">{t.loading}</p>
      ) : pool.length === 0 ? (
        <p className="empty">{t.done}</p>
      ) : (
        <ul aria-label={t.list} className="rows spot">
          {pool.map((e) => (
            <li key={e.judgment} ref={e.judgment === focus ? on : undefined} className={e.judgment === focus ? "on" : undefined}>
              <div>
                <strong>{e.link ? <a href={e.link} target="_blank" rel="noreferrer noopener">{e.subject || t.noSubject}</a> : e.subject || t.noSubject}</strong>
                <p className="sub">
                  {source(e.source)} · {new Date(e.judged_at).toLocaleString(locale())}
                  {raw && <code className="raw-id">{`${e.event} · ${e.template} · ${e.judgment}`}</code>}
                </p>
              </div>
              <div className="actions">
                {e.results.map((r) => (
                  <button key={r} className={r === "should_see" ? "primary" : undefined} aria-label={`${name(r)} ${e.subject || t.noSubject}`} onClick={() => void review(e, r)}>
                    {name(r)}
                  </button>
                ))}
              </div>
            </li>
          ))}
        </ul>
      )}
      <p className="inline-field">
        <label>
          {t.perBrief}
          <input type="number" min={0} value={per} onChange={(e) => setPer(e.target.value)} />
          {t.perBriefUnit}
        </label>{" "}
        <button onClick={() => void savePer()}>{t.save}</button>
      </p>
    </main>
  );
}
