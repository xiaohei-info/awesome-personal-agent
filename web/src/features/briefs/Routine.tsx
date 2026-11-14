import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { EntityPick } from "../../app/names";
import { PERM } from "../../app/ids";
import { Pick } from "../../app/values";
import { DAYS, browserZone, toCron, type Days } from "./schedule";
import { strings } from "./strings";

// 例行任务实例（routine 包，§13.5）：POST /v1/instances {pack:"routine", project?, params}。
// params 的形状以 packs/routine/pack.yaml 的 instance_params 为准（schedule、mode 必填；title、instruction、access、output 可选），
// 服务端按它校验；任务说明在「到点运行」时由本页要求填写。
export const MODES = ["run", "create_item"] as const;
export const OUTPUTS = ["item", "item_and_material"] as const;
type Mode = (typeof MODES)[number];
type Output = (typeof OUTPUTS)[number];

export interface Instance { id: string; pack: string; project?: string; params: { schedule?: string; projects?: string[]; mode?: string; title?: string; access?: string }; enabled: boolean }

/** 到点运行时用哪个宿主、它的上限是多少（§13.6）：规则不指定宿主，所以取默认宿主；读不到时为 null，不提示。 */
function useHostCap(): { host: string; cap: string } | null {
  const { api } = useSession();
  const [v, setV] = useState<{ host: string; cap: string } | null>(null);
  useEffect(() => {
    Promise.all([
      api.get<{ default_harness?: string }>("/settings/policy"),
      api.get<{ permission_cap?: { default?: string; by_harness?: Record<string, string> } }>("/settings/access-limits"),
    ]).then(
      ([p, l]) => {
        const host = p.default_harness ?? "";
        setV({ host, cap: l.permission_cap?.by_harness?.[host] ?? l.permission_cap?.default ?? "read" });
      },
      () => setV(null),
    );
  }, [api]);
  return v;
}

export function RoutineForm({ onCreated }: { onCreated: (i: Instance) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const host = useHostCap();
  const [mode, setMode] = useState<Mode>("create_item");
  const [title, setTitle] = useState("");
  const [instruction, setInstruction] = useState("");
  const [days, setDays] = useState<Days>("1");
  const [time, setTime] = useState("09:00");
  const [project, setProject] = useState("");
  const [access, setAccess] = useState("read");
  const [output, setOutput] = useState<Output>("item");
  const [error, setError] = useState<unknown>(null);
  const run = mode === "run";
  const over = run && host && PERM.indexOf(access as (typeof PERM)[number]) > PERM.indexOf(host.cap as (typeof PERM)[number]);

  async function create(e: FormEvent) {
    e.preventDefault();
    if (run && !instruction.trim()) return setError(new Error(t.needInstruction));
    try {
      const params = {
        schedule: toCron(days, time, browserZone()),
        mode,
        ...(title.trim() ? { title: title.trim() } : {}),
        ...(run ? { instruction: instruction.trim(), access, output } : {}),
      };
      onCreated(await api.post<Instance>("/instances", { pack: "routine", ...(project ? { project } : {}), params }));
      setTitle("");
      setInstruction("");
      setError(null);
    } catch (err) {
      setError(err);
    }
  }

  return (
    <form className="panel" aria-label={t.routineNew} onSubmit={(e) => void create(e)}>
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
        <select aria-label={t.routineDays} value={days} onChange={(e) => setDays(e.target.value as Days)}>
          {DAYS.map((d) => <option key={d} value={d}>{t.days[d]}</option>)}
        </select>
        <input type="time" aria-label={t.routineTime} value={time} onChange={(e) => setTime(e.target.value)} />
      </fieldset>
      <label>{t.routineProject}<EntityPick kind="project" value={project} onChange={setProject} empty={t.noProject} /></label>
      {run && (
        <>
          <label>{t.access}<Pick k="perm" value={access} onChange={setAccess} ids={[...PERM]} /></label>
          {host && !host.host && <p className="sub">{t.noHost} <Link to="/settings/access">{t.setHost}</Link></p>}
          {over && <p className="sub">{t.overCap} <Link to="/settings/access">{t.setHost}</Link></p>}
          <label>{t.output}
            <select value={output} onChange={(e) => setOutput(e.target.value as Output)}>
              {OUTPUTS.map((o) => <option key={o} value={o}>{t.outputs[o]}</option>)}
            </select>
          </label>
        </>
      )}
      <button type="submit">{t.routineCreate}</button>
      {error != null && <ErrorNote error={error} />}
    </form>
  );
}
