import { Route, Routes, useLocation } from "react-router-dom";
import { ErrorBoundary, Skeleton } from "../../app/ui";
import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useNamed, useNames } from "../../app/names";
import { DAYS, browserZone, toCron, type Days } from "./schedule";
import { strings } from "./strings";
import { Pick } from "../../app/values";
import { RoutineForm, type Instance } from "./Routine";
import { ScheduleCard, When } from "./ScheduleCard";
import { InstanceReminders, ReminderScope } from "../settings-notify/Reminder";
import { BriefDetail } from "./BriefDetail";
import { Library } from "./Library";
import type { Brief } from "./BriefBody";

// 形状对应 GET /v1/items?kind=brief（spec.period、spec.sections[{project|title,body}]、origin.source）、GET/POST /v1/instances（列表带 agent_ready）、
// PATCH /v1/instances/{id}（enabled 走 instance.toggle）、POST /v1/instances/{id}/run、GET /v1/packs（default_instruction）（附录 B.2、C.10）。
// 简报与复盘都是 brief 事项（packs/review 写回 brief kind，以 spec.period 区分周期），本页不猜类型，统一按周期列出；分节与定时校验都由服务端与包负责，本页只展示并搬运输入。
// 简报与复盘开箱各有一个默认安排（§7.3），每个实例一张卡；例行任务由本人自己建。
interface Pack { name: string; default_instruction?: string }

const BRIEF_PACKS = ["brief", "review"]; // 产出 brief 事项的内置包
const ROUTINE = "routine"; // 例行任务：结果也进本页（§13.5）

/** 已有安排时「再安排一个」收起；一个都没有时直接展开表单。 */
function More({ open, label, children }: { open: boolean; label: string; children: ReactNode }) {
  return open ? <>{children}</> : <details><summary>{label}</summary>{children}</details>;
}

/** 简报页：/briefs 读已生成的（最新 + 历史），/briefs/:id 一份完整简报，/briefs/schedule 管定时安排。 */
export function BriefsPage() {
  return (
    <Routes>
      <Route index element={<Library />} />
      <Route path="schedule" element={<Schedule />} />
      <Route path=":id" element={<BriefDetail />} />
    </Routes>
  );
}

/** 自动规则 › 定时安排（/briefs/schedule，地址不变）：定时简报与例行任务两节。 */
export function Schedule() {
  const { api } = useSession();
  const t = useT(strings);
  const [briefs, setBriefs] = useState<Brief[] | null>(null);
  const [instances, setInstances] = useState<Instance[] | null>(null);
  const [packs, setPacks] = useState<string[]>([]);
  const [defaults, setDefaults] = useState<Record<string, string>>({});
  const [agentReady, setAgentReady] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [pack, setPack] = useState("");
  const [days, setDays] = useState<Days>("1-5");
  const [time, setTime] = useState("08:00");
  const [raw, setRaw] = useState(""); // 高级：直接写 cron 或时长，填了就用它
  const [projects, setProjects] = useState<string[]>([]);
  const [editingRoutine, setEditingRoutine] = useState<string | null>(null);
  const allProjects = useNamed("project");
  const projectName = useNames("project");
  const submitRef = useRef<HTMLButtonElement>(null);
  const nav = useLocation().state as { suggest?: boolean; open?: string } | null;
  const fromEmpty = nav?.suggest;
  // 空状态的下一步：把表单预填成「每天 08:00 的简报」，本人再点一次「安排」才创建。
  const suggest = () => {
    setPack("brief");
    setDays("*");
    setTime("08:00");
    setRaw("");
    submitRef.current?.focus();
  };

  useEffect(() => { if (fromEmpty) suggest(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const load = useCallback(async () => {
    try {
      const [b, i, p] = await Promise.all([
        api.get<{ items: Brief[] }>("/items?kind=brief"),
        api.get<{ instances: Instance[]; agent_ready?: boolean }>("/instances"),
        api.get<{ packs: Pack[] }>("/packs"),
      ]);
      setBriefs(b.items);
      setInstances(i.instances.filter((x) => BRIEF_PACKS.includes(x.pack) || x.pack === ROUTINE));
      setPacks(p.packs.map((x) => x.name).filter((n) => BRIEF_PACKS.includes(n) || n === ROUTINE));
      setDefaults(Object.fromEntries(p.packs.map((x) => [x.name, x.default_instruction ?? ""])));
      setAgentReady(!!i.agent_ready);
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  const replace = (i: Instance) => setInstances((l) => (l ?? []).map((x) => (x.id === i.id ? i : x)));
  // 某个实例最近一次建的简报（origin.source 是 instance:<id>）。
  const lastOf = (id: string) => (briefs ?? []).filter((b) => b.origin?.source === `instance:${id}`)
    .sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""))[0];

  async function create(e: FormEvent) {
    e.preventDefault();
    const schedule = raw.trim() || (time ? toCron(days, time, browserZone()) : "");
    if (!schedule) return setError(new Error(t.needSchedule));
    try {
      const created = await api.post<Instance>("/instances", {
        pack: pack || briefPacks[0],
        params: { schedule, ...(projects.length ? { projects } : {}) },
      });
      setInstances((l) => [...(l ?? []), created]);
      setRaw("");
      setProjects([]);
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
  const scheduled = (instances ?? []).filter((x) => x.pack !== ROUTINE)
    .sort((a, b) => BRIEF_PACKS.indexOf(a.pack) - BRIEF_PACKS.indexOf(b.pack)); // 简报在前，复盘在后
  const routines = (instances ?? []).filter((x) => x.pack === ROUTINE);

  if (!briefs || !instances) return <main>{error != null ? <ErrorNote error={error} /> : <Skeleton shape="card" label={t.loading} />}</main>;

  return (
    <main>
      <PageHead>{t.tabSchedule}</PageHead>
      {error != null && <ErrorNote error={error} />}
      <ReminderScope instances={instances}>
      <section aria-label={t.instances}>
        <h2>{t.scheduled}</h2>
        <div aria-label={t.instanceList}>
          {scheduled.map((i) => {
            const last = lastOf(i.id);
            return <ErrorBoundary key={i.id}><ScheduleCard i={i} fallback={defaults[i.pack] ?? ""} agentReady={agentReady} open={nav?.open === i.id} last={last && { id: last.id, at: last.created_at }} onChange={replace} /></ErrorBoundary>;
          })}
        </div>
        <More open={!scheduled.length} label={t.more}>
        <form className="panel" aria-label={t.newSchedule} onSubmit={(e) => void create(e)}>
          <label>{t.pack}
            <Pick k="pack" value={pack || briefPacks[0] || ""} onChange={setPack} ids={briefPacks} />
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
          <button type="submit" ref={submitRef} disabled={!briefPacks.length}>{t.create}</button>
        </form>
        </More>
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
                <InstanceReminders i={i} />
              </li>
            ))}
          </ul>
          <RoutineForm onDone={(i) => setInstances((l) => [...(l ?? []), i])} />
        </section>
      )}
      </ReminderScope>
    </main>
  );
}
