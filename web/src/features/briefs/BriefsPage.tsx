import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useNamed, useNames } from "../../app/names";
import { DAYS, browserZone, parseCron, toCron, type Days } from "./schedule";
import { strings } from "./strings";
import { Pick, Val } from "../../app/values";
import { RoutineForm, type Instance } from "./Routine";

// 形状对应 GET /v1/items?kind=brief（spec.period、spec.sections[{project,body}]）、GET/POST /v1/instances、PATCH /v1/instances/{id}（enabled 走 instance.toggle）、GET /v1/packs（附录 B.2、C.10）。
// 简报与复盘都是 brief 事项（packs/review 写回 brief kind，以 spec.period 区分周期），本页不猜类型，统一按周期列出；分节与定时校验都由服务端与包负责，本页只展示并搬运输入。
interface Section { project: string; body: string }
interface Brief { id: string; title: string; created_at?: string; spec?: { period?: string; sections?: Section[] } }
interface Pack { name: string }

const BRIEF_PACKS = ["brief", "review"]; // 产出 brief 事项的内置包
const ROUTINE = "routine"; // 例行任务：结果也进本页（§13.5）

/** 实例的定时：本页生成的写法显示成「工作日 08:00」，时区与这台设备不同时带上时区；其余原样显示。 */
function When({ schedule }: { schedule?: string }) {
  const t = useT(strings);
  if (!schedule) return <>—</>;
  const p = parseCron(schedule);
  if (!p) return <code>{schedule}</code>;
  return <>{t.days[p.days]} {p.time}{p.zone !== browserZone() ? t.zone(p.zone) : ""}</>;
}

function BriefCard({ b }: { b: Brief }) {
  const t = useT(strings);
  const projectName = useNames("project");
  const sections = b.spec?.sections ?? [];
  return (
    <article aria-label={t.brief(b.title)}>
      <h4>{b.title}</h4>
      <p className="meta">{[b.spec?.period, b.created_at && new Date(b.created_at).toLocaleString(locale())].filter(Boolean).join(" · ")}</p>
      {!sections.length && <p>{t.noSections}</p>}
      {sections.map((s, i) => (
        <section key={i}>
          <h5>{projectName(s.project)}</h5>
          <p>{s.body}</p>
        </section>
      ))}
    </article>
  );
}

export function BriefsPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [briefs, setBriefs] = useState<Brief[] | null>(null);
  const [instances, setInstances] = useState<Instance[] | null>(null);
  const [packs, setPacks] = useState<string[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [pack, setPack] = useState("");
  const [days, setDays] = useState<Days>("1-5");
  const [time, setTime] = useState("08:00");
  const [raw, setRaw] = useState(""); // 高级：直接写 cron 或时长，填了就用它
  const [projects, setProjects] = useState<string[]>([]);
  const [editing, setEditing] = useState<Instance | null>(null); // 正在修改的简报或复盘；表单与新建共用
  const [editingRoutine, setEditingRoutine] = useState<string | null>(null);
  const allProjects = useNamed("project");
  const projectName = useNames("project");
  const submitRef = useRef<HTMLButtonElement>(null);
  // 空状态的下一步：把表单预填成「每天 08:00 的简报」，本人再点一次「安排」才创建。
  const suggest = () => {
    setPack("brief");
    setDays("*");
    setTime("08:00");
    setRaw("");
    submitRef.current?.focus();
  };

  const load = useCallback(async () => {
    try {
      const [b, i, p] = await Promise.all([
        api.get<{ items: Brief[] }>("/items?kind=brief"),
        api.get<{ instances: Instance[] }>("/instances"),
        api.get<{ packs: Pack[] }>("/packs"),
      ]);
      setBriefs(b.items);
      setInstances(i.instances.filter((x) => BRIEF_PACKS.includes(x.pack) || x.pack === ROUTINE));
      setPacks(p.packs.map((x) => x.name).filter((n) => BRIEF_PACKS.includes(n) || n === ROUTINE));
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  const replace = (i: Instance) => setInstances((l) => (l ?? []).map((x) => (x.id === i.id ? i : x)));
  // 修改：表单按实例预填。定时是本页能读回的写法就填进哪几天与几点，否则放进「直接写定时」。
  function edit(i: Instance) {
    const p = i.params.schedule ? parseCron(i.params.schedule) : null;
    setEditing(i);
    setPack(i.pack);
    setDays(p?.days ?? "1-5");
    setTime(p?.time ?? "08:00");
    setRaw(p ? "" : i.params.schedule ?? "");
    setProjects(i.params.projects ?? []);
  }
  function reset() {
    setEditing(null);
    setRaw("");
    setProjects([]);
  }

  async function create(e: FormEvent) {
    e.preventDefault();
    const zone = (editing?.params.schedule && parseCron(editing.params.schedule)?.zone) || browserZone();
    const schedule = raw.trim() || (time ? toCron(days, time, zone) : "");
    if (!schedule) return setError(new Error(t.needSchedule));
    try {
      if (editing) {
        // instance.update 整体替换 params：其余参数原样带回，只换定时与项目。
        const { projects: _old, ...rest } = editing.params;
        replace(await api.patch<Instance>(`/instances/${encodeURIComponent(editing.id)}`, { params: { ...rest, schedule, ...(projects.length ? { projects } : {}) } }));
      } else {
        const created = await api.post<Instance>("/instances", {
          pack: pack || briefPacks[0],
          params: { schedule, ...(projects.length ? { projects } : {}) },
        });
        setInstances((l) => [...(l ?? []), created]);
      }
      reset();
      setError(null);
    } catch (err) {
      setError(err);
    }
  }

  // 启停：只发 enabled，服务端按 instance.toggle 处理并返回实例的当前状态。
  async function toggle(i: Instance) {
    try {
      replace(await api.patch<Instance>(`/instances/${encodeURIComponent(i.id)}`, { enabled: !i.enabled }));
      setError(null);
    } catch (err) {
      setError(err);
    }
  }
  const OnOff = ({ i }: { i: Instance }) => (
    <button type="button" aria-pressed={i.enabled} aria-label={t.toggle(i.params.title?.split("{date}").join(t.dateMark) || i.id)} onClick={() => void toggle(i)}>{i.enabled ? t.pause : t.resume}</button>
  );
  const Edit = ({ i, onClick }: { i: Instance; onClick: () => void }) => (
    <button type="button" aria-label={t.editOf(i.params.title?.split("{date}").join(t.dateMark) || i.id)} onClick={onClick}>{t.edit}</button>
  );
  const briefPacks = packs.filter((n) => BRIEF_PACKS.includes(n));
  const scheduled = (instances ?? []).filter((x) => x.pack !== ROUTINE);
  const routines = (instances ?? []).filter((x) => x.pack === ROUTINE);

  if (!briefs || !instances) return <main>{error != null ? <ErrorNote error={error} /> : <p role="status">{t.loading}</p>}</main>;

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      <section aria-label={t.title}>
        <h2>{t.briefs}</h2>
        {!briefs.length && (
          <p className="sub">
            {t.none} {!scheduled.length && packs.includes("brief") && <button type="button" onClick={suggest}>{t.suggest}</button>}
          </p>
        )}
        {briefs.map((b) => <BriefCard key={b.id} b={b} />)}
      </section>
      <section aria-label={t.instances}>
        <h2>{t.scheduled}</h2>
        <ul aria-label={t.instanceList} className={scheduled.length ? "rows" : "plain"}>
          {scheduled.map((i) => (
            <li key={i.id}><span><When schedule={i.params.schedule} /> · <Val k="pack" id={i.pack} />{i.params.projects?.length ? ` · ${i.params.projects.map(projectName).join(t.listSep)}` : ""}{i.enabled ? "" : t.disabled}</span> <span><Edit i={i} onClick={() => edit(i)} /> <OnOff i={i} /></span></li>
          ))}
        </ul>
        <form className="panel" aria-label={editing ? t.scheduledEdit : undefined} onSubmit={(e) => void create(e)}>
          <label>{t.pack}
            <Pick k="pack" value={pack || briefPacks[0] || ""} onChange={setPack} ids={briefPacks} disabled={!!editing} />
          </label>
          <fieldset disabled={!!raw.trim()}>
            <legend>{t.schedule}</legend>
            <select aria-label={t.whichDays} value={days} onChange={(e) => setDays(e.target.value as Days)}>
              {DAYS.map((d) => <option key={d} value={d}>{t.days[d]}</option>)}
            </select>
            <input type="time" aria-label={t.atTime} value={time} onChange={(e) => setTime(e.target.value)} />
          </fieldset>
          {allProjects.length > 0 && (
            <fieldset>
              <legend>{t.projects}</legend>
              {allProjects.map((p) => (
                <label key={p.id}>
                  <input type="checkbox" checked={projects.includes(p.id)} onChange={() => setProjects((l) => (l.includes(p.id) ? l.filter((x) => x !== p.id) : [...l, p.id]))} />
                  {p.name}
                </label>
              ))}
            </fieldset>
          )}
          <details>
            <summary>{t.advanced}</summary>
            <label>{t.rawSchedule}<input value={raw} onChange={(e) => setRaw(e.target.value)} placeholder="0 8 * * 1-5" /></label>
          </details>
          <button type="submit" ref={submitRef} disabled={!briefPacks.length}>{editing ? t.save : t.create}</button>
          {editing && <> <button type="button" onClick={reset}>{t.cancel}</button></>}
        </form>
      </section>
      {packs.includes(ROUTINE) && (
        <section aria-label={t.routines}>
          <h2>{t.routines}</h2>
          <ul aria-label={t.routineList} className={routines.length ? "rows" : "plain"}>
            {!routines.length && <li className="sub">{t.noRoutines}</li>}
            {routines.map((i) => (
              <li key={i.id}>
                <span>{i.params.title?.split("{date}").join(t.dateMark) || t.modes[i.params.mode as "run" | "create_item"] || i.id} · <When schedule={i.params.schedule} />{i.project ? ` · ${projectName(i.project)}` : ""}{i.enabled ? "" : t.disabled}</span> <span><Edit i={i} onClick={() => setEditingRoutine(i.id)} /> <OnOff i={i} /></span>
                {editingRoutine === i.id && <RoutineForm key={i.id} edit={i} onDone={(n) => (replace(n), setEditingRoutine(null))} onCancel={() => setEditingRoutine(null)} />}
              </li>
            ))}
          </ul>
          <RoutineForm onDone={(i) => setInstances((l) => [...(l ?? []), i])} />
        </section>
      )}
    </main>
  );
}
