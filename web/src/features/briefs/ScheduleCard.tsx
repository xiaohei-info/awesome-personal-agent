import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useNamed } from "../../app/names";
import { useLabel } from "../../app/values";
import { useRaw } from "../../app/raw";
import { DAYS, browserZone, parseCron, toCron, type Days } from "./schedule";
import { strings } from "./strings";
import type { Instance } from "./Routine";

// 简报、复盘实例的卡片（§4.1 简报页）：收起时一行摘要，展开后改「什么时候、看哪些项目、跑什么」，并可启停、立即跑一次。
// 改动走 PATCH /v1/instances/{id}（instance.update 整体替换 params，其余参数原样带回）；立即跑一次走 POST /v1/instances/{id}/run。
// 「跑什么」是实例参数 instruction：没写时用包的 default_instruction（GET /v1/packs），随新版默认更新；写回与默认相同即视为没写。
// 资料信封、写回格式与权限是模板的固定部分，不在这里。
export const MAX_INSTRUCTION = 2000; // 与内核 packs.MaxInstructionRunes 一致

export function ScheduleCard({ i, fallback, agentReady, last, onChange }: {
  i: Instance;
  fallback: string; // 包的默认内容要求
  agentReady: boolean;
  last?: { id: string; at?: string };
  onChange: (n: Instance) => void;
}) {
  const { api } = useSession();
  const t = useT(strings);
  const label = useLabel();
  const allProjects = useNamed("project");
  const parsed = i.params.schedule ? parseCron(i.params.schedule) : null;
  const [days, setDays] = useState<Days>(parsed?.days ?? "1-5");
  const [time, setTime] = useState(parsed?.time ?? "08:30");
  const [raw, setRaw] = useState(parsed ? "" : i.params.schedule ?? "");
  const [projects, setProjects] = useState<string[]>(i.params.projects ?? []);
  const [text, setText] = useState(i.params.instruction || fallback);
  const [note, setNote] = useState("");
  const [error, setError] = useState<unknown>(null);
  const custom = text.trim() !== "" && text.trim() !== fallback.trim();
  const name = label("pack", i.pack);

  async function save(e: FormEvent) {
    e.preventDefault();
    // 没改哪几天与几点就原样带回；改了沿用原来的时区（开箱默认不带时区，即 APA 主机时区）。
    const same = parsed && days === parsed.days && time === parsed.time;
    const schedule = raw.trim() || (same ? i.params.schedule! : time ? toCron(days, time, parsed ? parsed.zone : browserZone()) : "");
    if (!schedule) return setError(new Error(t.needSchedule));
    const { projects: _p, instruction: _i, ...rest } = i.params;
    try {
      onChange(await api.patch<Instance>(`/instances/${encodeURIComponent(i.id)}`, {
        params: { ...rest, schedule, ...(projects.length ? { projects } : {}), ...(custom ? { instruction: text.trim() } : {}) },
      }));
      setNote(t.saved);
      setError(null);
    } catch (err) {
      setError(err);
    }
  }
  async function toggle() {
    try {
      onChange(await api.patch<Instance>(`/instances/${encodeURIComponent(i.id)}`, { enabled: !i.enabled }));
      setError(null);
    } catch (err) {
      setError(err);
    }
  }
  async function runNow() {
    try {
      await api.post(`/instances/${encodeURIComponent(i.id)}/run`, {});
      setNote(t.started);
      setError(null);
    } catch (err) {
      setError(err);
    }
  }

  return (
    <details className="panel" aria-label={name}>
      <summary>
        <strong>{name}</strong> <When schedule={i.params.schedule} /> · {i.enabled ? t.on : t.off}
        {!agentReady && i.enabled && <> · <span className="sub">{t.waitingAgent}</span> <Link to="/settings/agents">{t.setHost}</Link></>}
      </summary>
      {error != null && <ErrorNote error={error} />}
      <form aria-label={t.scheduleOf(name)} onSubmit={(e) => void save(e)}>
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
        <label>
          {t.what} {!custom && <span className="sub">{t.defaultTag}</span>}
          <textarea rows={3} maxLength={MAX_INSTRUCTION} value={text} onChange={(e) => setText(e.target.value)} />
        </label>
        {custom && <button type="button" onClick={() => setText(fallback)}>{t.restore}</button>}
        <details>
          <summary>{t.advanced}</summary>
          <label>{t.rawSchedule}<input value={raw} onChange={(e) => setRaw(e.target.value)} placeholder="0 8 * * 1-5" /></label>
        </details>
        <div className="actions">
          <button type="submit">{t.save}</button>
          <button type="button" aria-pressed={i.enabled} onClick={() => void toggle()}>{i.enabled ? t.pause : t.resume}</button>
          <button type="button" disabled={!agentReady} onClick={() => void runNow()}>{t.runNow}</button>
          {note && <span role="status" className="sub">{note}</span>}
        </div>
      </form>
      {last && <p className="sub"><Link to={`/briefs/${encodeURIComponent(last.id)}`}>{t.last(last.at ? new Date(last.at).toLocaleString(locale()) : "")}</Link></p>}
    </details>
  );
}

/** 实例的定时：本页生成的写法显示成「工作日 08:30」，时区与这台设备不同时带上时区；读不回的写法不露原串。 */
export function When({ schedule }: { schedule?: string }) {
  const t = useT(strings);
  const showRaw = useRaw();
  if (!schedule) return <>—</>;
  const p = parseCron(schedule);
  if (!p) return showRaw ? <code>{schedule}</code> : <>{t.customSchedule}</>;
  return <>{t.days[p.days]} {p.time}{p.zone && p.zone !== browserZone() ? t.zone(p.zone) : ""}</>;
}
