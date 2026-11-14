import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { entryOf, useLabel, Val } from "../../app/values";
import { useLang } from "../../app/i18n";
import { useRaw } from "../../app/raw";
import { strings } from "./strings";
import { backlogReason, when, type SourceEvent } from "./shared";

// 积压放开四步（§9.3、设计 §6.5）：只跑规则 → 试打分 → 定自动处理的线 → 放开重判。每步一个数字、一个主按钮。
// 进度存在这台浏览器里，刷新后从上一步的结果接着走；数字以服务端每步的回答为准。
const KEY = "apa.backlog";
type Progress = { step: number; rules?: { done: number; remaining: number }; score?: { scored: number; skipped: number }; release?: { requeued: number; deferred: number } };
const readProgress = (): Progress => {
  try {
    const p = JSON.parse(localStorage.getItem(KEY) ?? "");
    return typeof p?.step === "number" ? p : { step: 0 };
  } catch {
    return { step: 0 };
  }
};

/** 四步走到一半（做过第 1 步、还没放开）：积压数字归零时也继续显示，好接着走完。 */
export const inProgress = () => {
  const s = readProgress().step;
  return s > 0 && s < 4;
};

/** 模型的现状：有没有打分模型、有没有任何模型、是否暂停（GET /settings/model-endpoints）。 */
export interface Models {
  judge: boolean;
  any: boolean;
  paused: boolean;
}

const STEPS = ["rules", "score", "bands", "rejudge"] as const;

export function BacklogFlow({ total, models, onChanged, ignoreAll }: { total: number; models: Models; onChanged: () => void; ignoreAll: React.ReactNode }) {
  const t = useT(strings);
  const label = useLabel();
  const { api } = useSession();
  const [p, setP] = useState<Progress>(readProgress);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const save = (next: Progress) => {
    setP(next);
    localStorage.setItem(KEY, JSON.stringify(next));
  };

  const call = async <T,>(path: string, then: (r: T) => Progress) => {
    setBusy(true);
    setError(null);
    try {
      save(then(await api.post<T>(path, {})));
      onChanged();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  // 没有打分模型时分数和线都没有意义：第 2、3 步一起跳过。
  const skipTo = (step: number) => save({ ...p, step });

  const step = Math.min(p.step, STEPS.length);
  const left = p.rules?.remaining ?? total;
  const resume = <Link to="/settings/models">{t.resume}</Link>;
  let body: React.ReactNode;
  if (step === 0) {
    body = (
      <>
        <p className="big">{t.left(total)}</p>
        <button className="primary" disabled={busy} onClick={() => call<{ attached: number; disposed: number; remaining: number }>("/source-events/rules", (r) => ({ step: 1, rules: { done: r.attached + r.disposed, remaining: r.remaining } }))}>{t.start}</button>
      </>
    );
  } else if (step === 1) {
    body = (
      <>
        <p className="big">{t.left(left)}</p>
        {p.rules && <p className="sub">{t.rulesDone(p.rules.done)}</p>}
        {models.paused ? (
          <p><button className="primary" disabled>{t.startScore}</button> <span className="sub">{t.paused}</span> {resume}</p>
        ) : models.judge ? (
          <p>
            <button className="primary" disabled={busy} onClick={() => call<{ scored: number; skipped: number }>("/source-events/score", (r) => ({ ...p, step: 2, score: r }))}>{t.startScore}</button>{" "}
            <button className="quiet" onClick={() => skipTo(3)}>{t.skip}</button>
          </p>
        ) : (
          <p><span className="sub">{t.noJudge}</span> <button onClick={() => skipTo(3)}>{t.skip}</button></p>
        )}
      </>
    );
  } else if (step === 2) {
    body = (
      <>
        <p className="big">{t.left(left)}</p>
        {p.score && <p className="sub">{t.scored(p.score.scored, p.score.skipped)}</p>}
        <p>
          <Link className="btn" to="/judgment">{t.setLine}</Link> <button className="primary" onClick={() => skipTo(3)}>{t.lineSet}</button>
        </p>
      </>
    );
  } else if (step === 3) {
    body = (
      <>
        <p className="big">{t.left(left)}</p>
        {models.paused ? (
          <p><button className="primary" disabled>{t.release}</button> <span className="sub">{t.paused}</span> {resume}</p>
        ) : !models.any ? (
          <p><button className="primary" disabled>{t.release}</button> <span className="sub">{t.noModel}</span> <Link to="/settings/models">{t.connectModel}</Link></p>
        ) : (
          <button className="primary" disabled={busy} onClick={() => call<{ requeued: number; deferred: number }>("/source-events/rejudge", (r) => ({ ...p, step: 4, release: r }))}>{t.release}</button>
        )}
      </>
    );
  } else {
    const r = p.release ?? { requeued: 0, deferred: 0 };
    body = (
      <>
        <p className="big">{t.allDone}</p>
        <p className="sub">{r.deferred ? t.deferred(r.requeued, r.deferred) : t.released(r.requeued)}</p>
        <button onClick={() => save({ step: 0 })}>{t.again}</button>
      </>
    );
  }

  return (
    <section role="region" aria-labelledby="h-flow" className="panel">
      <h2 id="h-flow">{t.backlog(total)}</h2>
      <ol className="flow-steps" aria-label={t.flow}>
        {STEPS.map((s, i) => (
          <li key={s} className={i < step ? "done" : i === step ? "on" : ""} aria-current={i === step ? "step" : undefined}>
            {t.stepLabel(i + 1, label("backlogStep", s))}
          </li>
        ))}
      </ol>
      {error !== null && <ErrorNote error={error} />}
      {body}
      <p>{ignoreAll}</p>
    </section>
  );
}

// 两步确认的批量忽略：第一次点击只展开确认，避免误触一次忽略整批。
export function IgnoreAll({ label, body, onDone }: { label: string; body: Record<string, unknown>; onDone: (err: unknown, n: number) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [confirming, setConfirming] = useState(false);
  const go = async () => {
    setConfirming(false);
    try {
      const r = await api.post<{ ignored: number }>("/source-events/ignore", body);
      onDone(null, r.ignored);
    } catch (e) {
      onDone(e, 0);
    }
  };
  return confirming ? (
    <span role="group" aria-label={label}>
      <button className="danger" onClick={go}>{t.confirm(label)}</button> <button className="quiet" onClick={() => setConfirming(false)}>{t.cancel}</button>
    </span>
  ) : (
    <button onClick={() => setConfirming(true)}>{label}</button>
  );
}

const BADGE: Record<string, string> = { model_unavailable: "badge warn", outbound_unset: "badge warn", models_paused: "badge plain", restored: "badge plain", expired: "badge plain" };

/** 一个来源的积压（未判定与已过期）：逐条转建议或忽略；等外发范围的可点去选。 */
export function SourceBacklog({ source, onChooseOutbound }: { source: string; onChooseOutbound: () => void }) {
  const t = useT(strings);
  const lang = useLang();
  const raw = useRaw();
  const { api } = useSession();
  const [items, setItems] = useState<SourceEvent[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [byScore, setByScore] = useState(false);
  const base = `/sources/${encodeURIComponent(source)}/events?disposition=unjudged,expired${byScore ? "&sort=score" : ""}`;

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
    setItems(null);
    setNext(null);
    setError(null);
    load();
  }, [load]);

  const act = async (id: string, what: "propose" | "ignore") => {
    setError(null);
    setNotice(null);
    try {
      await api.post(`/source-events/${encodeURIComponent(id)}/${what}`);
    } catch (e) {
      setError(e);
    }
    await load(); // 以服务端状态为准：处理成功的不再留在积压里
  };

  const scored = items?.some((v) => v.score !== undefined);
  return (
    <section role="region" aria-label={t.unjudged}>
      <h4>{t.unjudged}</h4>
      {(scored || byScore) && (
        <label>
          <input type="checkbox" checked={byScore} onChange={(e) => setByScore(e.target.checked)} /> {t.byScore}
        </label>
      )}
      {error !== null && <ErrorNote error={error} />}
      {notice && <p role="status">{notice}</p>}
      {items === null ? (
        error === null && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noUnjudged}</p>
      ) : (
        <>
          <ul className="rows">
            {items.map((v) => {
              const why = backlogReason(v);
              const k = why === "expired" ? "disposition" : "unjudgedReason";
              const badge = <span className={BADGE[why] ?? "badge"} title={entryOf(lang, k, why)?.hint}><Val k={k} id={why} /></span>;
              return (
                <li key={v.id}>
                  <span>{v.subject || v.id}</span> · {when(v.occurred_at)} {raw && <code className="raw-id">{v.type}</code>}{" "}
                  {why === "outbound_unset" ? <button className="link" onClick={onChooseOutbound}>{badge}</button> : badge}{" "}
                  {v.score !== undefined && <span className="badge plain">{t.worth(`${Math.round(v.score * 100)}%`)}</span>}
                  {v.summary && <p>{v.summary}</p>}
                  <div role="group" aria-label={t.handle(v.id)}>
                    <button onClick={() => act(v.id, "propose")}>{t.propose}</button> <button onClick={() => act(v.id, "ignore")}>{t.ignore}</button>
                  </div>
                </li>
              );
            })}
          </ul>
          <IgnoreAll
            label={t.ignoreSource}
            body={{ source }}
            onDone={async (err, n) => {
              setError(err);
              if (!err) setNotice(t.ignored(n));
              await load();
            }}
          />
        </>
      )}
      {next !== null && <button onClick={() => load(next)}>{t.more}</button>}
    </section>
  );
}
