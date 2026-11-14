import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { EntityPick } from "../../app/names";
import { strings } from "./strings";
import { when, type Source, type SourceEvent } from "./shared";

/** 来源的默认项目（§9.1）：PATCH /sources/{id} 只带 default_project，空串清除。项目是否存在由服务端校验。 */
export function DefaultProject({ source, onSaved }: { source: Source; onSaved: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [error, setError] = useState<unknown>(null);
  const save = async (id: string) => {
    setError(null);
    try {
      await api.patch(`/sources/${encodeURIComponent(source.id)}`, { default_project: id });
      onSaved();
    } catch (e) {
      setError(e);
    }
  };
  return (
    <>
      <label>
        {t.defaultProject}
        <EntityPick kind="project" value={source.default_project ?? ""} onChange={(id) => void save(id)} empty={t.noProject} />
      </label>
      {error !== null && <ErrorNote error={error} />}
    </>
  );
}

/** 只进了简报「知会」一节的消息（disposition=digest，§9.3）：可逐条转成建议。 */
export function Digest({ source }: { source: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<SourceEvent[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<unknown>(null);
  const base = `/sources/${encodeURIComponent(source)}/events?disposition=digest`;
  const load = useCallback(
    (after?: number) =>
      api.get<{ events: SourceEvent[]; next_after?: number }>(base + (after ? `&after=${after}` : "")).then(
        (r) => {
          setItems((cur) => (after && cur ? [...cur, ...r.events] : r.events));
          setNext(r.next_after ?? null);
        },
        (e) => setError(e),
      ),
    [api, base],
  );
  useEffect(() => {
    load();
  }, [load]);
  const propose = async (id: string) => {
    setError(null);
    try {
      await api.post(`/source-events/${encodeURIComponent(id)}/propose`);
    } catch (e) {
      setError(e);
    }
    await load(); // 以服务端状态为准：转成建议的不再是 digest
  };
  return (
    <section role="region" aria-label={t.digest}>
      <h4>{t.digest}</h4>
      {error !== null && <ErrorNote error={error} />}
      {items === null ? (
        error === null && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noDigest}</p>
      ) : (
        <ul className="rows">
          {items.map((v) => (
            <li key={v.id}>
              <span>{v.subject || v.id}</span> · {when(v.occurred_at)}
              {v.summary && <p>{v.summary}</p>}
              <div role="group" aria-label={t.handle(v.id)}>
                <button onClick={() => void propose(v.id)}>{t.propose}</button>
              </div>
            </li>
          ))}
        </ul>
      )}
      {next !== null && <button onClick={() => load(next)}>{t.more}</button>}
    </section>
  );
}
