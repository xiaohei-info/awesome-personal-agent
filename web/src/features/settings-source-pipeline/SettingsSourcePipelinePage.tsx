import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { PACK_BAND } from "../../app/ids";
import { Card, PolicyPage, reject, useDraft } from "../../app/policy-cards";
import { AgentPick } from "../settings-agents/AgentPick";
import { Reminder, ReminderScope } from "../settings-notify/Reminder";
import { withBand, type Band } from "../judgment/JudgmentPage";
import { SourceRules } from "./SourceRules";
import { strings } from "./strings";

// 设置 › 来源处理（§9.3）：读写 GET/PUT /settings/source-pipeline，结果提醒复用 Reminder(source_result)，
// 两道题拿不准的范围复用 GET/PUT /settings/judgment 里 triage-screen、triage-urgent 的全局不确定带。校验以服务端为准。
export interface SourcePipeline {
  agent?: string;
  batch_size?: number;
  wait_minutes?: number;
  urgent_immediate?: boolean;
  max_batch?: number;
  max_retries?: number;
  daily_batch_limit?: number;
  judge_prompt?: string;
  batch_prompt?: string;
}

export const DEFAULT_BATCH = 10;
export const DEFAULT_WAIT = 30;
export const DEFAULT_MAX_BATCH = 20;
export const DEFAULT_RETRIES = 2;
const MAX_PROMPT = 12000;
const JUDGE_PACK = "/packs/inbound-triage/prompts/triage-screen";
const BATCH_PACK = "/packs/inbound-triage/prompts/source-batch";

type SavePipeline = (fields: Partial<SourcePipeline>) => Promise<number>;

export function SettingsSourcePipelinePage() {
  const t = useT(strings);
  const { api } = useSession();
  const [doc, setDoc] = useState<{ version: number; source_pipeline: SourcePipeline } | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      setDoc(await api.get<{ version: number; source_pipeline: SourcePipeline }>("/settings/source-pipeline"));
    } catch (e) {
      setDoc(null);
      setError(msg(e));
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  const save: SavePipeline = async (fields) => {
    const next = await api.put<{ version: number; source_pipeline: SourcePipeline }>("/settings/source-pipeline", {
      source_pipeline: { ...(doc?.source_pipeline ?? {}), ...fields },
    });
    setDoc(next);
    return next.version;
  };

  const sp = doc?.source_pipeline;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!sp} onRetry={() => void load()}>
      {sp && (
        <>
          <AgentCard sp={sp} save={save} />
          <ScheduleCard sp={sp} save={save} />
          <ReminderScope>
            <Reminder row="source_result" />
          </ReminderScope>
          <SourceRules />
          <PromptCard
            title={t.judgeTitle}
            hint={t.judgeHint}
            value={sp.judge_prompt ?? ""}
            packPath={JUDGE_PACK}
            field="criteria"
            onSave={(text) => save({ judge_prompt: text })}
          />
          <PromptCard
            title={t.batchPromptTitle}
            hint={t.batchPromptHint}
            value={sp.batch_prompt ?? ""}
            packPath={BATCH_PACK}
            field="body"
            onSave={(text) => save({ batch_prompt: text })}
          />
          <details className="panel">
            <summary>{t.advanced}</summary>
            <RetryCard sp={sp} save={save} />
            <BandCard template="triage-screen" title={t.bandTitle} hint={t.bandHint} />
            <BandCard template="triage-urgent" title={t.urgentBandTitle} hint={t.urgentBandHint} />
          </details>
        </>
      )}
    </PolicyPage>
  );
}

function AgentCard({ sp, save }: { sp: SourcePipeline; save: SavePipeline }) {
  const t = useT(strings);
  const [agent, setAgent, dirty] = useDraft(sp.agent ?? "", () => sp.agent ?? "");
  return (
    <Card title={t.agentTitle} hint={t.agentHint} dirty={dirty} onSave={() => save({ agent })}>
      <AgentPick value={agent} onChange={setAgent} label={t.agentLabel} />
    </Card>
  );
}

function ScheduleCard({ sp, save }: { sp: SourcePipeline; save: SavePipeline }) {
  const t = useT(strings);
  const init = () => ({
    batch: String(sp.batch_size || DEFAULT_BATCH),
    wait: String(sp.wait_minutes || DEFAULT_WAIT),
    urgent: !!sp.urgent_immediate,
    maxBatch: String(sp.max_batch || DEFAULT_MAX_BATCH),
  });
  const [d, setD, dirty] = useDraft([sp.batch_size, sp.wait_minutes, sp.urgent_immediate, sp.max_batch], init);
  const onSave = () => {
    const batch = Number(d.batch);
    const wait = Number(d.wait);
    const maxBatch = Number(d.maxBatch);
    if (!Number.isInteger(batch) || batch < 1 || batch > 100) return reject(t.batchBad);
    if (!Number.isInteger(wait) || wait < 1 || wait > 1440) return reject(t.waitBad);
    if (!Number.isInteger(maxBatch) || maxBatch < 1 || maxBatch > 100) return reject(t.maxBatchBad);
    return save({ batch_size: batch, wait_minutes: wait, urgent_immediate: d.urgent, max_batch: maxBatch });
  };
  return (
    <Card title={t.scheduleTitle} hint={t.scheduleHint} dirty={dirty} onSave={onSave}>
      <p className="sentence">
        <label>
          {t.batchLabel}{" "}
          <input type="number" min={1} max={100} value={d.batch} onChange={(e) => setD({ ...d, batch: e.target.value })} />{" "}
          {t.batchUnit}
        </label>
        <label>
          {t.waitLabel}{" "}
          <input type="number" min={1} max={1440} value={d.wait} onChange={(e) => setD({ ...d, wait: e.target.value })} />{" "}
          {t.waitUnit}
        </label>
      </p>
      <fieldset>
        <legend>{t.urgentLabel}</legend>
        <label className="row">
          <input type="radio" name="urgent_immediate" checked={d.urgent} onChange={() => setD({ ...d, urgent: true })} />
          <span>{t.urgentOn}</span>
        </label>
        <label className="row">
          <input type="radio" name="urgent_immediate" checked={!d.urgent} onChange={() => setD({ ...d, urgent: false })} />
          <span>{t.urgentOff}</span>
        </label>
      </fieldset>
      <p>
        <label>
          {t.maxBatchLabel}{" "}
          <input type="number" min={1} max={100} value={d.maxBatch} onChange={(e) => setD({ ...d, maxBatch: e.target.value })} />{" "}
          {t.batchUnit}
        </label>
      </p>
    </Card>
  );
}

type PromptProps = { title: string; hint: string; value: string; onSave: (text: string) => Promise<number> };

/** 提示词框里就是当前生效的全文：没改过是内置默认（取能力包提示词的 field 字段），先读到默认才能编辑。 */
function PromptCard({ packPath, field, ...p }: PromptProps & { packPath: string; field: "body" | "criteria" }) {
  const t = useT(strings);
  const { api } = useSession();
  const [builtin, setBuiltin] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    api.get<Record<string, string>>(packPath).then(
      (r) => setBuiltin((r[field] ?? "").trim()),
      () => setFailed(true),
    );
  }, [api, packPath, field]);
  if (builtin !== null) return <PromptEditor {...p} builtin={builtin} />;
  return (
    <section aria-label={p.title} className="panel">
      <h2>{p.title}</h2>
      <p className="sub" role={failed ? "alert" : "status"}>{failed ? t.builtinFailed : t.builtinLoading}</p>
    </section>
  );
}

function PromptEditor({ title, hint, value, builtin, onSave }: PromptProps & { builtin: string }) {
  const t = useT(strings);
  const [text, setText, dirty] = useDraft(value, () => value || builtin);

  const submit = () => {
    const trimmed = text.trim();
    if (trimmed.length > MAX_PROMPT) return reject(t.promptTooLong(MAX_PROMPT));
    // 与内置默认相同按没改过存，以后内置默认升级能跟着走。
    return onSave(trimmed === builtin ? "" : trimmed);
  };

  return (
    <Card title={title} hint={hint} dirty={dirty} onSave={submit}>
      <textarea rows={16} aria-label={title} value={text} onChange={(e) => setText(e.target.value)} />
      <div className="actions">
        {text.trim() !== builtin && (
          <button type="button" className="quiet" onClick={() => setText(builtin)}>{t.resetDefault}</button>
        )}
      </div>
    </Card>
  );
}

function RetryCard({ sp, save }: { sp: SourcePipeline; save: SavePipeline }) {
  const t = useT(strings);
  const init = () => ({
    retries: String(sp.max_retries ?? DEFAULT_RETRIES),
    daily: String(sp.daily_batch_limit ?? 0),
  });
  const [d, setD, dirty] = useDraft([sp.max_retries, sp.daily_batch_limit], init);
  const onSave = () => {
    const retries = Number(d.retries);
    const daily = Number(d.daily);
    if (!Number.isInteger(retries) || retries < 0 || retries > 10) return reject(t.retriesBad);
    if (!Number.isInteger(daily) || daily < 0 || daily > 1000) return reject(t.dailyBad);
    return save({ max_retries: retries, daily_batch_limit: daily });
  };
  return (
    <Card title={t.retryTitle} dirty={dirty} onSave={onSave}>
      <p>
        <label>
          {t.retriesLabel}{" "}
          <input type="number" min={0} max={10} value={d.retries} onChange={(e) => setD({ ...d, retries: e.target.value })} />
        </label>
      </p>
      <p>
        <label>
          {t.dailyLabel}{" "}
          <input type="number" min={0} max={1000} value={d.daily} onChange={(e) => setD({ ...d, daily: e.target.value })} />
        </label>
      </p>
    </Card>
  );
}

/** 拿不准的范围：读写 /settings/judgment 里一个模板的全局不确定带（§8.10）。 */
function BandCard({ template, title, hint }: { template: "triage-screen" | "triage-urgent"; title: string; hint: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [bands, setBands] = useState<Band[] | null>(null);
  const load = useCallback(() => {
    api.get<{ bands?: Band[] }>("/settings/judgment").then(
      (r) => setBands(Array.isArray(r?.bands) ? r.bands : []),
      () => setBands(null),
    );
  }, [api]);
  useEffect(load, [load]);
  if (bands === null) return null;
  const hit = bands.find((b) => b.template === template && !(b.source ?? ""));
  const def = PACK_BAND[template];
  const low = hit?.low ?? def.low!;
  const high = hit?.high ?? def.high!;
  // 两张卡片写同一份清单：保存前重读，免得盖掉另一张刚存的值。
  const save = async (b: { low: number; high: number }) => {
    const cur = await api.get<{ bands?: Band[] }>("/settings/judgment");
    const r = await api.put<{ version: number; bands?: Band[] }>("/settings/judgment", { bands: withBand(cur.bands ?? [], { template, ...b }) });
    load();
    return r.version ?? 0;
  };
  return <BandEditor title={title} hint={hint + (hit ? "" : t.bandDefault)} low={low} high={high} onSave={save} />;
}

function BandEditor({ title, hint, low, high, onSave }: {
  title: string;
  hint: string;
  low: number;
  high: number;
  onSave: (b: { low: number; high: number }) => Promise<number>;
}) {
  const t = useT(strings);
  const [d, setD, dirty] = useDraft([low, high], () => ({ low: String(low), high: String(high) }));
  const submit = () => {
    const lo = Number(d.low);
    const hi = Number(d.high);
    if (!Number.isFinite(lo) || !Number.isFinite(hi) || lo < 0 || hi > 1 || lo > hi) return reject(t.bandBad);
    return onSave({ low: lo, high: hi });
  };
  return (
    <Card title={title} hint={hint} dirty={dirty} onSave={submit}>
      <p className="sentence">
        <label>
          {t.bandLow}{" "}
          <input type="number" min={0} max={1} step={0.01} value={d.low} onChange={(e) => setD({ ...d, low: e.target.value })} />
        </label>
        <label>
          {t.bandHigh}{" "}
          <input type="number" min={0} max={1} step={0.01} value={d.high} onChange={(e) => setD({ ...d, high: e.target.value })} />
        </label>
      </p>
    </Card>
  );
}
