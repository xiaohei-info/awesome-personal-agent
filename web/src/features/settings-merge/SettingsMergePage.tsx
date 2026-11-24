import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText, useT } from "../../app/i18n";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { BudgetNote, useBudgetBad, useWritingModel } from "../../app/tokenBudget/BudgetNote";
import { budgetOk, DEFAULT_BUDGET_TOKENS as DEFAULT_BUDGET, MAX_BUDGET_TOKENS as MAX_BUDGET, MIN_BUDGET_TOKENS as MIN_BUDGET } from "../../app/tokenBudget/constants";
import { strings } from "./strings";

// 设置 › 跨来源合并（§9.3）：/settings/merge 的 merge_judge 与 merge 两段。每张卡片单独保存，保存时带上整段 merge：
// 已保存的原样带回，只换这张卡片的字段。校验以服务端为准（缺段的提示词 400、不生效），页面先按同一规则挡一次，免得白发请求。
const ROUTES = ["merge"] as const;
const PACK_PROMPT = "/packs/inbound-triage/prompts/merge-batch";
const ACTIONS = ["attach", "merge", "duplicate", "keep"];
const DEFAULT_BATCH = 100;
const DEFAULT_WAIT = 30;

interface Merge { exclude_sources?: string[] | null; skip_existing?: boolean; batch_size?: number; wait_minutes?: number; budget_tokens?: number; prompt?: string; prompt_at?: string; similarity?: string }

// 相似度档位（§9.3）：与内核 policy.SimilarityMin 同值，由 kernel/policy 的 TestWebSimilarityTiersMatchKernel 锁住。空串是默认档。
export const SIMILARITY_TIERS = [
  { id: "loose", min: 0.25 },
  { id: "", min: 0.3 },
  { id: "strict", min: 0.4 },
] as const;

export function SettingsMergePage() {
  const t = useT(strings);
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const d = docs?.merge;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!d} onRetry={() => void load()}>
      {d && (
        <>
          <AgentCard doc={d} save={save} />
          <SourcesCard doc={d} save={save} />
          <ExistingCard doc={d} save={save} />
          <SimilarityCard doc={d} save={save} />
          <BatchCard doc={d} save={save} />
          <PromptCard doc={d} save={save} />
          <Cost />
        </>
      )}
    </PolicyPage>
  );
}

const putMerge = (doc: Doc, save: Save, fields: Merge) => save("merge", { merge: { ...(doc.merge as Merge), ...fields } });

function AgentCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const value = doc.merge_judge === "off" ? "off" : "same";
  const [pick, setPick, dirty] = useDraft(value, () => value);
  return (
    <Card title={t.agentTitle} hint={t.agentNote} dirty={dirty} onSave={() => save("merge", { merge_judge: pick })}>
      <fieldset>
        {(["same", "off"] as const).map((v) => (
          <label key={v} className="row">
            <input type="radio" name="merge_judge" value={v} checked={pick === v} onChange={() => setPick(v)} />
            <span>{t.agentChoice[v]}</span>
            <small className="sub">{t.agentHint[v]}</small>
          </label>
        ))}
      </fieldset>
    </Card>
  );
}

function SourcesCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const { api } = useSession();
  const [sources, setSources] = useState<{ id: string; name: string }[] | null>(null);
  useEffect(() => {
    api.get<{ sources: { id: string; name: string }[] }>("/sources").then((r) => setSources(r.sources), () => setSources([]));
  }, [api]);
  const m = doc.merge as Merge;
  const [off, setOff, dirty] = useDraft(m.exclude_sources, () => m.exclude_sources ?? []);
  return (
    <Card title={t.sourcesTitle} hint={t.sourcesHint} dirty={dirty} onSave={() => putMerge(doc, save, { exclude_sources: off })}>
      {sources?.length === 0 && <p className="sub">{t.sourcesNone}</p>}
      <div role="group" aria-label={t.sourcesTitle}>
        {sources?.map((s) => (
          <label key={s.id} className="row">
            <input type="checkbox" checked={!off.includes(s.id)} onChange={(e) => setOff(e.target.checked ? off.filter((x) => x !== s.id) : [...off, s.id])} />
            <span>{s.name || s.id}</span>
          </label>
        ))}
      </div>
    </Card>
  );
}

function ExistingCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const m = doc.merge as Merge;
  const [on, setOn, dirty] = useDraft(m.skip_existing, () => !m.skip_existing);
  return (
    <Card title={t.existingTitle} hint={t.existingHint} dirty={dirty} onSave={() => putMerge(doc, save, { skip_existing: !on })}>
      <label className="row">
        <input type="checkbox" checked={on} onChange={(e) => setOn(e.target.checked)} />
        <span>{t.existingLabel}</span>
      </label>
    </Card>
  );
}

function SimilarityCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const m = doc.merge as Merge;
  const value = m.similarity ?? "";
  const [pick, setPick, dirty] = useDraft(value, () => value);
  return (
    <Card title={t.simTitle} hint={t.simHint} dirty={dirty} onSave={() => putMerge(doc, save, { similarity: pick || undefined })}>
      <fieldset>
        {SIMILARITY_TIERS.map((x) => (
          <label key={x.id} className="row">
            <input type="radio" name="similarity" value={x.id} checked={pick === x.id} onChange={() => setPick(x.id)} />
            <span>{t.simChoice(t.simName[x.id], x.min.toFixed(2))}</span>
            <small className="sub">{t.simWhy[x.id]}</small>
          </label>
        ))}
      </fieldset>
    </Card>
  );
}

function BatchCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const m = doc.merge as Merge;
  const model = useWritingModel();
  const budgetBad = useBudgetBad();
  const init = () => ({ size: String(m.batch_size || DEFAULT_BATCH), wait: String(m.wait_minutes || DEFAULT_WAIT), budget: String(m.budget_tokens ?? DEFAULT_BUDGET) });
  const [v, setV, dirty] = useDraft([m.batch_size, m.wait_minutes, m.budget_tokens], init);
  const onSave = () => {
    const size = Number(v.size), wait = Number(v.wait), budget = Number(v.budget);
    if (!Number.isInteger(size) || size < 2 || size > 100 || !Number.isInteger(wait) || wait < 5 || wait > 1440) return reject(t.batchBad);
    if (!budgetOk(v.budget)) return reject(budgetBad);
    // 等于默认值时条数与分钟存 0、预算不带这个键：以后默认值调整时跟着走。
    return putMerge(doc, save, { batch_size: size === DEFAULT_BATCH ? 0 : size, wait_minutes: wait === DEFAULT_WAIT ? 0 : wait, budget_tokens: budget === DEFAULT_BUDGET ? undefined : budget });
  };
  return (
    <Card title={t.batchTitle} hint={t.batchHint(Number(v.size) || DEFAULT_BATCH, Number(v.wait) || DEFAULT_WAIT)} dirty={dirty} onSave={onSave}>
      <div className="row">
        <label>{t.batchSize} <input type="number" min={2} max={100} value={v.size} onChange={(e) => setV({ ...v, size: e.target.value })} /></label>
        <label>{t.batchWait} <input type="number" min={5} max={1440} value={v.wait} onChange={(e) => setV({ ...v, wait: e.target.value })} /></label>
        <label>{t.budget} <input type="number" min={MIN_BUDGET} max={MAX_BUDGET} step={1000} value={v.budget} onChange={(e) => setV({ ...v, budget: e.target.value })} /></label>
      </div>
      <BudgetNote what={t.budgetWhat} model={model} />
    </Card>
  );
}

/** 与内核 policy.CheckMergePrompt 同一规则：内置提示词的每个 ## 段都在且有内容、四个结论词都写到、不超过 8000 字。返回问题，没有问题返回空串。 */
export function promptProblem(body: string, builtin: string, t: typeof strings.zh): string {
  if (body.trim() === "") return "";
  if ([...body].length > 8000) return t.promptLong([...body].length);
  for (const h of builtin.split("\n").filter((l) => l.startsWith("## ")).map((l) => l.trim())) {
    const at = body.indexOf(h);
    if (at < 0) return t.promptMissing(h);
    const rest = body.slice(at + h.length);
    const end = rest.indexOf("\n## ");
    if ((end < 0 ? rest : rest.slice(0, end)).trim() === "") return t.promptEmpty(h);
  }
  const missing = ACTIONS.filter((a) => !body.includes(a));
  return missing.length ? t.promptMissing(missing.join("、")) : "";
}

function PromptCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const { api } = useSession();
  const m = doc.merge as Merge;
  const [builtin, setBuiltin] = useState<string | null>(null);
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    api.get<{ body: string }>(PACK_PROMPT).then((r) => setBuiltin(r.body.trim()), () => setBuiltin(""));
  }, [api]);
  const [text, setText, dirty] = useDraft([m.prompt, builtin], () => m.prompt || builtin || "");
  const custom = !!m.prompt;
  const onSave = () => {
    const problem = promptProblem(text, builtin ?? "", t);
    if (problem) return reject(problem);
    // 和内置默认逐字相同就存空串：仍算"用内置默认"，内置版本更新时跟着走。
    return putMerge(doc, save, { prompt: text.trim() === builtin ? "" : text.trim() });
  };
  const restore = async () => {
    setNote(null);
    try {
      await putMerge(doc, save, { prompt: "" });
      setText(builtin ?? "");
      setNote({ ok: true, text: t.restored });
    } catch (e) {
      setNote({ ok: false, text: errorText(e) });
    }
  };
  return (
    <Card title={t.promptTitle} hint={t.promptHint} dirty={dirty} onSave={onSave}>
      <p className="sub" role="note">{t.promptActive(custom, m.prompt_at ? new Date(m.prompt_at).toLocaleString() : "")}</p>
      {builtin === null ? <p role="status">{t.defaultLoading}</p> : (
        <textarea aria-label={t.promptLabel} rows={16} value={text} onChange={(e) => setText(e.target.value)} />
      )}
      <div>
        <button type="button" disabled={!custom && !dirty} onClick={() => void restore()}>{t.restore}</button>
        {note && <span role={note.ok ? "status" : "alert"} className={"saved-note" + (note.ok ? "" : " bad")}>{note.text}</span>}
      </div>
    </Card>
  );
}

function Cost() {
  const t = useT(strings);
  const { api } = useSession();
  const [a, setA] = useState<{ pending: number; model_calls: number; chars: number; batches: unknown[] } | null>(null);
  useEffect(() => {
    api.get<{ pending: number; model_calls: number; chars: number; batches: unknown[] }>("/merge/activity?days=7").then(setA, () => setA(null));
  }, [api]);
  if (!a) return null;
  return (
    <section className="panel" aria-label={t.costTitle}>
      <h2>{t.costTitle}</h2>
      <p>{t.cost(a.batches.length, a.model_calls, a.chars, a.pending)}</p>
      <p className="sub"><Link to="/proposals">{t.costSee}</Link></p>
    </section>
  );
}
