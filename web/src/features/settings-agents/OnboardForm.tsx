import { useId, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { useRaw } from "../../app/raw";
import { useLabel } from "../../app/values";
import { check, commandLine, emptyForm, harnessOf, TIERS, toDeclaration, type Field, type OnboardForm, type Tier } from "./declaration-form";
import { strings } from "./strings";

// 接入新的程序（附录 A.2；界面设计 §4 并进「添加 Agent」的第一步）：逐项填写，声明由 declaration-form.ts 生成；「直接编辑声明」与表单二选一，不双向同步——
// 表单只覆盖声明的一部分字段，反解任意 JSON 会丢掉其余字段。提交走 executor.register，内核校验是最终把关。
export function Onboard({ taken, reload, startOpen }: { taken: string[]; reload: () => Promise<void>; startOpen?: boolean }) {
  const t = useT(strings);
  const { api } = useSession();
  const raw = useRaw();
  const label = useLabel();
  const id = useId();
  const [open, setOpen] = useState(!!startOpen);
  const [f, setF] = useState<OnboardForm>(emptyForm);
  const [tier, setTier] = useState<Tier>("read");
  const [json, setJson] = useState<string | null>(null);
  const [tried, setTried] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const set = (p: Partial<OnboardForm>) => setF((x) => ({ ...x, ...p }));
  const setTierOf = (k: Tier, p: Partial<OnboardForm["tiers"][Tier]>) => setF((x) => ({ ...x, tiers: { ...x.tiers, [k]: { ...x.tiers[k], ...p } } }));

  const problems = check(f, taken);
  const decl = toDeclaration(f);
  const touched: Record<Field, boolean> = {
    name: !!(f.zh || f.en), harness: !!f.harness, command: !!f.command, tiers: TIERS.some((k) => f.tiers[k].on),
    output: !!f.output, assign: !!f.assign, resumeArgs: !!f.resumeArgs,
  };
  const err = (field: Field) => {
    const ps = problems.filter((x) => x.field === field);
    return ps.length > 0 && (tried || touched[field]) ? <p className="field-err" role="alert">{ps.map((p) => t.problem[p.code] + (p.detail ?? "")).join(t.listSep)}</p> : null;
  };
  const shownTier = decl.headless.access[tier] ? tier : (TIERS.find((k) => f.tiers[k].on) ?? "read");

  const submit = async () => {
    setError(null);
    let body: unknown = decl;
    if (json !== null) {
      try {
        body = JSON.parse(json);
      } catch {
        return setError(t.badJSON);
      }
    } else if (problems.length) return setTried(true);
    try {
      await api.post("/settings/commands", body);
    } catch (e) {
      return setError(msg(e));
    }
    setF(emptyForm());
    setJson(null);
    setTried(false);
    setOpen(false);
    await reload();
  };

  if (!open)
    return (
      <section aria-label={t.onboard} className="panel stack">
        <h2>{t.onboard}</h2>
        <div><button type="button" onClick={() => setOpen(true)}>{t.onboardOpen}</button></div>
      </section>
    );
  const insert = (tok: string) => set({ command: (f.command.trim() ? f.command.trimEnd() + " " : "") + tok });
  return (
    <section aria-label={t.onboard} className="panel stack">
      <h2>{t.onboard}</h2>
      <fieldset disabled={json !== null} className="onboard">
        <div className="two">
          <label>{t.nameZh}<input value={f.zh} onChange={(e) => set({ zh: e.target.value })} /></label>
          <label>{t.nameEn}<input value={f.en} onChange={(e) => set({ en: e.target.value })} /></label>
        </div>
        {err("name")}
        {raw && <label>{t.harnessId}<input value={f.harness} placeholder={harnessOf(f)} onChange={(e) => set({ harness: e.target.value })} /></label>}
        {raw && err("harness")}
        <label>{t.command}<input value={f.command} placeholder={t.commandHint} onChange={(e) => set({ command: e.target.value })} /></label>
        <div className="tokens">
          <button type="button" className="quiet" onClick={() => insert("{prompt}")}>{t.tokenPrompt}</button>
          <button type="button" className="quiet" onClick={() => insert("{run_dir}")}>{t.tokenRunDir}</button>
        </div>
        {err("command")}
        <fieldset>
          <legend>{t.tiersLegend}</legend>
          {TIERS.map((k) => (
            <div key={k} className="tier-row">
              <label><input type="checkbox" checked={f.tiers[k].on} onChange={(e) => setTierOf(k, { on: e.target.checked })} />{label("perm", k)}</label>
              <input aria-label={label("perm", k) + " · " + t.tierArgs} placeholder={t.tierArgs} disabled={!f.tiers[k].on} value={f.tiers[k].args} onChange={(e) => setTierOf(k, { args: e.target.value })} />
            </div>
          ))}
        </fieldset>
        {err("tiers")}
        <label><input type="checkbox" checked={f.resume} onChange={(e) => set({ resume: e.target.checked })} />{t.resume}</label>
        {f.resume && (
          <>
            <fieldset className="seg-radio" role="radiogroup" aria-label={t.sessionFrom}>
              <legend>{t.sessionFrom}</legend>
              {(["output", "assign"] as const).map((v) => (
                <label key={v}><input type="radio" name={id + "-session"} checked={f.sessionFrom === v} onChange={() => set({ sessionFrom: v })} />{t.sessionHow[v]}</label>
              ))}
            </fieldset>
            {f.sessionFrom === "output" ? (
              <label>{t.outputArgs}<input value={f.output} placeholder="--output-format json" onChange={(e) => set({ output: e.target.value })} /></label>
            ) : (
              <label>{t.assignArgs}<input value={f.assign} placeholder="--session-id {session_id}" onChange={(e) => set({ assign: e.target.value })} /></label>
            )}
            {err("output")}
            {err("assign")}
            <label>{t.resumeArgs}<input value={f.resumeArgs} placeholder="--resume {session_id}" onChange={(e) => set({ resumeArgs: e.target.value })} /></label>
            {err("resumeArgs")}
          </>
        )}
        <label><input type="checkbox" checked={f.wake} onChange={(e) => set({ wake: e.target.checked })} />{t.wakeBox}</label>
        {f.command.trim() && problems.every((p) => p.field !== "command") && (
          <div className="stack-tight" data-testid="onboard-preview">
            <div className="seg" role="group" aria-label={t.previewTier}>
              <span className="seg-label">{t.previewTier}</span>
              {TIERS.map((k) => (
                <button key={k} type="button" aria-pressed={shownTier === k} disabled={!f.tiers[k].on} onClick={() => setTier(k)}>{label("perm", k)}</button>
              ))}
            </div>
            <pre className="preview">{t.previewNew}{commandLine(decl, shownTier)}{f.resume ? "\n" + t.previewResume + commandLine(decl, shownTier, true) : ""}</pre>
          </div>
        )}
      </fieldset>
      <details>
        <summary>{t.editJSON}</summary>
        {json === null ? (
          <>
            <pre data-testid="onboard-json">{JSON.stringify(decl, null, 2)}</pre>
            <div><button type="button" onClick={() => setJson(JSON.stringify(decl, null, 2))}>{t.toJSON}</button></div>
          </>
        ) : (
          <>
            <p className="sub">{t.jsonMode}</p>
            <textarea aria-label={t.editJSON} rows={12} value={json} onChange={(e) => setJson(e.target.value)} />
            <div><button type="button" onClick={() => setJson(null)}>{t.toForm}</button></div>
          </>
        )}
      </details>
      {error && <p role="alert">{error}</p>}
      <div className="actions">
        <button type="button" className="primary" onClick={() => void submit()}>{t.submit}</button>
        <button type="button" onClick={() => setOpen(false)}>{t.cancel2}</button>
      </div>
    </section>
  );
}
