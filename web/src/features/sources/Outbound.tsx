import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useLang, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { entryOf, Val } from "../../app/values";
import { strings } from "./strings";
import { SCOPES, when, type Outbound, type Source, type SourceEvent } from "./shared";

// 外发范围（§14.14，设计 §6.1）：两档各一组单选，改了先看一条真实消息会被送出什么，点「就这样发」才写回。
// 裁剪在服务端做（GET /source-events/{id}/outbound-preview，与真实外发同源）；未选择与「不发送」的语义不变。

interface Tier {
  event_type: string;
  subject: string;
  sender: string;
  occurred_at: string;
  link: string;
  material: string;
}
type Preview = Record<"full" | "excerpt" | "meta", Tier>;

function ScopeRadios({ name, legend, value, onChange }: { name: string; legend: string; value: string | null; onChange: (v: string) => void }) {
  const lang = useLang();
  return (
    <fieldset className="seg-radio" role="radiogroup" aria-label={legend}>
      <legend>{legend}</legend>
      {SCOPES.map((v) => (
        <label key={v}>
          <input type="radio" name={name} value={v} checked={value === v} onChange={() => onChange(v)} />
          {entryOf(lang, "outboundScope", v)?.label ?? v}
        </label>
      ))}
    </fieldset>
  );
}

/** 一条消息在某档外发范围下会送出的样子；被裁掉的部分划线标出。 */
function PreviewView({ scope, p }: { scope: string; p: Preview | "empty" | null }) {
  const t = useT(strings);
  if (p === null) return <p role="status" className="out-preview">{t.loading}</p>;
  if (p === "empty") return <p className="out-preview">{t.previewEmpty}</p>;
  if (scope === "none") return <p className="out-preview">{t.previewNone}</p>;
  const tier = p[scope as keyof Preview];
  if (!tier) return null;
  const cut: string[] = [];
  if (!tier.material && p.full.material) cut.push(t.cutBody);
  else if (tier.material.length < p.full.material.length) cut.push(t.cutRest);
  if (tier.link !== p.full.link) cut.push(t.cutLink);
  return (
    <div className="out-preview" aria-label={t.previewLabel}>
      <p className="sub">{t.previewLabel}</p>
      <p>
        {tier.subject && <><strong>{t.fieldSubject}</strong> {tier.subject} · </>}
        {tier.sender && <><strong>{t.fieldSender}</strong> {tier.sender} · </>}
        <strong>{t.fieldAt}</strong> {when(tier.occurred_at)}
        {tier.link && <> · <strong>{t.fieldLink}</strong> {tier.link}</>}
      </p>
      {tier.material && <pre>{tier.material}</pre>}
      {cut.length > 0 && <p><s>{cut.join(t.listSep)}</s> {t.cut}</p>}
    </div>
  );
}

export function OutboundEditor({ source, waiting, open, onOpen, onSaved }: {
  source: Source;
  /** 「发给外部模型时」还没选时，有多少条消息在等它（GET /health 的积压明细，全部来源合计）。 */
  waiting: number;
  open: boolean;
  onOpen: (open: boolean) => void;
  onSaved: () => Promise<void>;
}) {
  const t = useT(strings);
  const { api } = useSession();
  const saved: Outbound = source.outbound ?? { local: "full", external: null };
  const [draft, setDraft] = useState<Outbound>(saved);
  const [last, setLast] = useState<"local" | "external">("external");
  const [preview, setPreview] = useState<Preview | "empty" | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const changed = draft.local !== saved.local || draft.external !== saved.external;
  const shown = open || saved.external === null;

  useEffect(() => setDraft(source.outbound ?? { local: "full", external: null }), [source.outbound?.local, source.outbound?.external]); // eslint-disable-line react-hooks/exhaustive-deps

  // 第一次改动时取一条真实消息的预览（取这个来源最近一页里最新的一条）。
  useEffect(() => {
    if (!changed || preview !== null) return;
    let live = true;
    (async () => {
      const page = await api.get<{ events: SourceEvent[] }>(`/sources/${encodeURIComponent(source.id)}/events?limit=500`);
      const ev = page.events[page.events.length - 1];
      if (!ev) return "empty" as const;
      return api.get<Preview>(`/source-events/${encodeURIComponent(ev.id)}/outbound-preview`);
    })().then(
      (p) => live && setPreview(p),
      (e) => live && setError(e),
    );
    return () => {
      live = false;
    };
  }, [changed, preview, api, source.id]);

  const send = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.put(`/settings/sources/${encodeURIComponent(source.id)}/outbound`, draft);
      onOpen(false);
      await onSaved();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  if (!shown)
    return (
      <div className="sub full" role="group" aria-label={t.outbound(source.id)}>
        {t.toLocal} <Val k="outboundScope" id={saved.local} /> · {t.toExternal} <Val k="outboundScope" id={saved.external} />{" "}
        <button className="quiet" onClick={() => onOpen(true)}>{t.change}</button>
      </div>
    );
  const scope = last === "local" ? draft.local : draft.external;
  return (
    <div className="full out-editor" role="group" aria-label={t.outbound(source.id)}>
      <ScopeRadios name={`${source.id}-local`} legend={t.toLocal} value={draft.local} onChange={(v) => { setDraft({ ...draft, local: v }); setLast("local"); }} />{" "}
      <ScopeRadios name={`${source.id}-external`} legend={t.toExternal} value={draft.external} onChange={(v) => { setDraft({ ...draft, external: v }); setLast("external"); }} />
      {saved.external === null && (
        <p><span className="badge warn">{t.notChosen}{waiting > 0 ? ` · ${t.waiting(waiting)}` : ""}</span></p>
      )}
      {changed && scope && <PreviewView scope={scope} p={preview} />}
      {error !== null && <ErrorNote error={error} />}
      <p>
        <button className="primary" disabled={!changed || busy} onClick={send}>{t.sendThis}</button>{" "}
        {open && <button className="quiet" onClick={() => { setDraft(saved); onOpen(false); }}>{t.cancel}</button>}
      </p>
    </div>
  );
}
