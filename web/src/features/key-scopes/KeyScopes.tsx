import { useEffect, useId, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 钥匙给谁用与逐来源档位（§14.2、§14.14）。档位不在表里就是不给；给谁用不预选（与端点位置同一原则）。
// 判定都在服务端：这里只收集本人的选择，PUT /settings/{tokens|ext-keys}/{id}/scope 或签发时一并提交。

export type Scopes = Record<string, string>;
export interface ScopedKey { id: string; kind: string; audience?: string; source_scopes?: Scopes; revoked_at?: string }
interface Source { id: string; name: string }

export const TIERS = ["", "meta", "excerpt", "full"] as const;

/** 已登记的来源（本人在 Web 看得到全部）。取不到时为空表，表格显示「还没有来源」。 */
export function useSources(): Source[] | null {
  const { api } = useSession();
  const [list, setList] = useState<Source[] | null>(null);
  useEffect(() => {
    api.get<{ sources: Source[] }>("/sources").then(
      (r) => setList(r.sources.map((s) => ({ id: s.id, name: s.name || s.id }))),
      () => setList([]),
    );
  }, [api]);
  return list;
}

function tierLabel(t: typeof strings.zh, tier: string) {
  return { "": t.tierNone, meta: t.tierMeta, excerpt: t.tierExcerpt, full: t.tierFull }[tier] ?? tier;
}

/** 给谁用：内部或外部，不预选。 */
export function AudienceChoice({ value, onChange }: { value: string | null; onChange: (a: string) => void }) {
  const t = useT(strings);
  const id = useId();
  return (
    <fieldset className="choice">
      <legend>{t.audience}</legend>
      {(["internal", "external"] as const).map((a) => (
        <label key={a}>
          <input type="radio" name={id} checked={value === a} onChange={() => onChange(a)} />
          <span>{a === "internal" ? t.internal : t.external}<small>{a === "internal" ? t.internalCan : t.externalCan}</small></span>
        </label>
      ))}
    </fieldset>
  );
}

/** 逐来源档位表：每个来源一组单选，表头「全部设为」只填现有行，不存通配。 */
export function SourceScopeTable({ sources, value, onChange }: { sources: Source[] | null; value: Scopes; onChange: (s: Scopes) => void }) {
  const t = useT(strings);
  const id = useId();
  if (sources === null) return <p role="status">{t.loading}</p>;
  const set = (src: string, tier: string) => {
    const next = { ...value };
    if (tier) next[src] = tier;
    else delete next[src];
    onChange(next);
  };
  return (
    <fieldset className="stack" aria-label={t.sources}>
      <legend>{t.sources}</legend>
      <p className="sub">{t.sourcesHint}</p>
      {sources.length === 0 ? (
        <p className="sub">{t.noSources}</p>
      ) : (
        <>
          <label>
            {t.setAll}{" "}
            <select value="-" onChange={(e) => onChange(Object.fromEntries(e.target.value ? sources.map((s) => [s.id, e.target.value]) : []))}>
              <option value="-" disabled>—</option>
              {TIERS.map((tier) => <option key={tier} value={tier}>{tierLabel(t, tier)}</option>)}
            </select>
          </label>
          <ul className="rows">
            {sources.map((s) => (
              <li key={s.id} role="radiogroup" aria-label={s.name}>
                <span>{s.name}</span>{" "}
                {TIERS.map((tier) => (
                  <label key={tier}>
                    <input type="radio" name={`${id}-${s.id}`} checked={(value[s.id] ?? "") === tier} onChange={() => set(s.id, tier)} />
                    {tierLabel(t, tier)}
                  </label>
                ))}
              </li>
            ))}
          </ul>
        </>
      )}
    </fieldset>
  );
}

/** 一句话说清这把钥匙读得到什么。 */
export function ScopeSummary({ k, sources }: { k: ScopedKey; sources: Source[] | null }) {
  const t = useT(strings);
  if (k.audience === "internal") return <span className="sub">{t.readsAll}</span>;
  const given = Object.entries(k.source_scopes ?? {});
  if (given.length === 0) return <span className="sub">{t.readsNothing}</span>;
  const name = (id: string) => sources?.find((s) => s.id === id)?.name ?? id;
  const rest = sources ? sources.length - given.length : 0;
  const list = given.map(([id, tier]) => `${name(id)} ${tierLabel(t, tier)}`).join(t.sep);
  return <span className="sub">{t.reads(list)}{rest > 0 ? t.sep + t.rest(rest) : ""}</span>;
}

/** 改一把已签发钥匙的给谁用与档位；ext 钥匙只能是外部（canInternal 为假时不显示给谁用）。 */
export function ScopeEditor({ k, group, canInternal, sources, onSaved }: { k: ScopedKey; group: "tokens" | "ext-keys"; canInternal: boolean; sources: Source[] | null; onSaved: () => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const [open, setOpen] = useState(false);
  const [audience, setAudience] = useState<string | null>(k.audience ?? "external");
  const [scopes, setScopes] = useState<Scopes>(k.source_scopes ?? {});
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  if (k.revoked_at) return null;
  if (!open) return <button type="button" className="quiet" onClick={() => setOpen(true)}>{t.edit}</button>;
  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.put(`/settings/${group}/${encodeURIComponent(k.id)}/scope`, { audience, source_scopes: audience === "internal" ? {} : scopes });
      setOpen(false);
      await onSaved();
    } catch (e) {
      setError(msg(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="panel stack" role="group" aria-label={t.sources}>
      {canInternal && <AudienceChoice value={audience} onChange={setAudience} />}
      {audience === "external" && <SourceScopeTable sources={sources} value={scopes} onChange={setScopes} />}
      {error && <p role="alert">{error}</p>}
      <p>
        <button type="button" className="primary" disabled={busy || !audience} onClick={() => void save()}>{t.saveScope}</button>{" "}
        <button type="button" className="quiet" onClick={() => setOpen(false)}>{t.cancel}</button>
      </p>
    </div>
  );
}
