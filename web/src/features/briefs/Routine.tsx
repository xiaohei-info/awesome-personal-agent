import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { EntityPick } from "../../app/names";
import { PERM } from "../../app/ids";
import { Pick } from "../../app/values";
import { DAYS, browserZone, parseCron, toCron, type Days } from "./schedule";
import { AgentPick, type AgentView } from "../settings-agents/AgentPick";
import { strings } from "./strings";

// 例行任务实例（routine 包，§13.5）：POST /v1/instances {pack:"routine", project?, params}；修改走 PATCH /v1/instances/{id} {project, params}
// （instance.update：params 整体替换，project 给了就改，空串表示不归项目；只有定时变了才重排）。
// params 的形状以 packs/routine/pack.yaml 的 instance_params 为准（schedule、mode 必填；title、instruction、access、output 可选），
// 服务端按它校验；任务说明在「到点运行」时由本页要求填写。
export const MODES = ["run", "create_item"] as const;
export const OUTPUTS = ["item", "item_and_material"] as const;
type Mode = (typeof MODES)[number];
type Output = (typeof OUTPUTS)[number];

export interface Instance { id: string; pack: string; agent_ready?: boolean; agent_reason?: string; project?: string; params: { agent_id?: string; schedule?: string; projects?: string[]; mode?: string; title?: string; instruction?: string; access?: string; output?: string }; enabled: boolean }

/** 到点运行时用哪个宿主、它的上限是多少（§13.6）：规则不指定宿主，所以取默认宿主；读不到时为 null，不提示。 */
function useHostCap(agent: string): { host: string; cap: string } | null {
  const { api } = useSession();
  const [v, setV] = useState<{ host: string; cap: string } | null>(null);
  useEffect(() => {
    let active = true;
    api.get<AgentView>("/agents?rule=routine%2Fclock_rules%2Fscheduled-routine&schedule=" + encodeURIComponent(agent)).then(view => {
      if (active && view.selected) setV({host: view.selected.agent_name ?? "", cap: view.selected.permission_cap ?? "read"});
    }, () => {});
    if (agent) return () => { active = false; };
    Promise.all([
      api.get<{ default_harness?: string }>("/settings/policy"),
      api.get<{ permission_cap?: { default?: string; by_harness?: Record<string, string> } }>("/settings/access-limits"),
    ]).then(
      ([p, l]) => {
        const host = p.default_harness ?? "";
        if (active) setV(previous => previous ?? { host, cap: l.permission_cap?.by_harness?.[host] ?? l.permission_cap?.default ?? "read" });
      },
      () => {},
    );
    return () => {active = false;};
  }, [api, agent]);
  return v;
}

/** 新建（无 edit）或修改一个实例；修改时按实例预填，调用方用 key 换实例。 */
export function RoutineForm({ edit, onDone, onCancel }: { edit?: Instance; onDone: (i: Instance) => void; onCancel?: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [agent,setAgent] = useState(edit?.params.agent_id ?? "");
  const host = useHostCap(agent);
  const p = edit?.params ?? {};
  const cron = p.schedule ? parseCron(p.schedule) : null;
  const [mode, setMode] = useState<Mode>((p.mode as Mode) ?? "create_item");
  const [title, setTitle] = useState(p.title ?? "");
  const [instruction, setInstruction] = useState(p.instruction ?? "");
  const [days, setDays] = useState<Days>(cron?.days ?? "1");
  const [time, setTime] = useState(cron?.time ?? "09:00");
  // 修改时定时没动过就原样带回，原来的时区不丢。
  // shortcut: 本页读不回的定时（CLI 写的复杂 cron）预填成默认的「每周一 09:00」，不动就不改；需要时加「直接写定时」。
  const [timed, setTimed] = useState(false);
  const [project, setProject] = useState(edit?.project ?? "");
  const [access, setAccess] = useState(p.access ?? "read");
  const [output, setOutput] = useState<Output>((p.output as Output) ?? "item");
  const [error, setError] = useState<unknown>(null);
  const run = mode === "run";
  const over = run && host && PERM.indexOf(access as (typeof PERM)[number]) > PERM.indexOf(host.cap as (typeof PERM)[number]);

  async function create(e: FormEvent) {
    e.preventDefault();
    if (run && !instruction.trim()) return setError(new Error(t.needInstruction));
    try {
      const params = {
        schedule: edit?.params.schedule && !timed ? edit.params.schedule : toCron(days, time, cron?.zone ?? browserZone()),
        mode,
        ...(title.trim() ? { title: title.trim() } : {}),
        ...(run && agent ? {agent_id: agent} : {}),
        ...(run ? { instruction: instruction.trim(), access, output } : {}),
      };
      if (edit) return onDone(await api.patch<Instance>(`/instances/${encodeURIComponent(edit.id)}`, { project, params }));
      onDone(await api.post<Instance>("/instances", { pack: "routine", ...(project ? { project } : {}), params }));
      setTitle("");
      setInstruction("");
      setError(null);
    } catch (err) {
      setError(err);
    }
  }

  return (
    <form className="panel" aria-label={edit ? t.routineEdit : t.routineNew} onSubmit={(e) => void create(e)}>
      <fieldset className="seg-radio" role="radiogroup" aria-label={t.routineMode}>
        <legend>{t.routineMode}</legend>
        {MODES.map((m) => (
          <label key={m}>
            <input type="radio" name="routine-mode" value={m} checked={mode === m} onChange={() => setMode(m)} />
            {t.modes[m]}
          </label>
        ))}
      </fieldset>
      <label>{t.routineTitle}<input value={title} onChange={(e) => setTitle(e.target.value)} placeholder={t.titlePlaceholder} /></label>
      {run && <label>{t.instruction}<textarea value={instruction} onChange={(e) => setInstruction(e.target.value)} /></label>}
      <fieldset>
        <legend>{t.schedule}</legend>
        <select aria-label={t.routineDays} value={days} onChange={(e) => (setDays(e.target.value as Days), setTimed(true))}>
          {DAYS.map((d) => <option key={d} value={d}>{t.days[d]}</option>)}
        </select>
        <input type="time" aria-label={t.routineTime} value={time} onChange={(e) => (setTime(e.target.value), setTimed(true))} />
      </fieldset>
      <label>{t.routineProject}<EntityPick kind="project" value={project} onChange={setProject} empty={t.noProject} /></label>
      {run && (
        <>
          <AgentPick value={agent} onChange={setAgent} rule="routine/clock_rules/scheduled-routine" />
          <label>{t.access}<Pick k="perm" value={access} onChange={setAccess} ids={[...PERM]} /></label>
          {host && !host.host && <p className="sub">{t.noHost} <Link to="/settings/agents">{t.setHost}</Link></p>}
          {over && <p className="sub">{t.overCap} <Link to="/settings/agents">{t.setHost}</Link></p>}
          <label>{t.output}
            <select value={output} onChange={(e) => setOutput(e.target.value as Output)}>
              {OUTPUTS.map((o) => <option key={o} value={o}>{t.outputs[o]}</option>)}
            </select>
          </label>
        </>
      )}
      <button type="submit">{edit ? t.save : t.routineCreate}</button>{onCancel && <> <button type="button" onClick={onCancel}>{t.cancel}</button></>}
      {error != null && <ErrorNote error={error} />}
    </form>
  );
}
