import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/items?kind=brief（spec.period、spec.sections[{project,body}]）、GET/POST /v1/instances、GET /v1/packs（附录 B.2、C.10）。
// 简报与复盘都是 brief 事项（packs/review 写回 brief kind，以 spec.period 区分周期），本页不猜类型，统一按周期列出；分节与定时校验都由服务端与包负责，本页只展示并搬运输入。
interface Section { project: string; body: string }
interface Brief { id: string; title: string; created_at?: string; spec?: { period?: string; sections?: Section[] } }
interface Instance { id: string; pack: string; params: { schedule?: string; projects?: string[] }; enabled: boolean }
interface Pack { name: string }

const BRIEF_PACKS = ["brief", "review"]; // 产出 brief 事项的内置包

function BriefCard({ b }: { b: Brief }) {
  const t = useT(strings);
  const sections = b.spec?.sections ?? [];
  return (
    <article aria-label={t.brief(b.title)}>
      <h4>{b.title}</h4>
      <p className="meta">{[b.spec?.period, b.created_at && new Date(b.created_at).toLocaleString(locale())].filter(Boolean).join(" · ")}</p>
      {!sections.length && <p>{t.noSections}</p>}
      {sections.map((s, i) => (
        <section key={i}>
          <h5>{s.project}</h5>
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
  const [error, setError] = useState<string | null>(null);
  const [pack, setPack] = useState("");
  const [schedule, setSchedule] = useState("");
  const [projects, setProjects] = useState("");

  const load = useCallback(async () => {
    try {
      const [b, i, p] = await Promise.all([
        api.get<{ items: Brief[] }>("/items?kind=brief"),
        api.get<{ instances: Instance[] }>("/instances"),
        api.get<{ packs: Pack[] }>("/packs"),
      ]);
      setBriefs(b.items);
      setInstances(i.instances.filter((x) => BRIEF_PACKS.includes(x.pack)));
      setPacks(p.packs.map((x) => x.name).filter((n) => BRIEF_PACKS.includes(n)));
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  async function create(e: FormEvent) {
    e.preventDefault();
    if (!schedule.trim()) return setError(t.needSchedule);
    const list = projects.split(",").map((s) => s.trim()).filter(Boolean);
    try {
      const created = await api.post<Instance>("/instances", {
        pack: pack || packs[0],
        params: { schedule: schedule.trim(), ...(list.length ? { projects: list } : {}) },
      });
      setInstances((l) => [...(l ?? []), created]);
      setSchedule("");
      setProjects("");
      setError(null);
    } catch (err) {
      setError(msg(err));
    }
  }

  if (!briefs || !instances) return <main>{error ? <p role="alert">{error}</p> : <p role="status">{t.loading}</p>}</main>;

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error && <p role="alert">{error}</p>}
      <section aria-label={t.title}>
        <h2>{t.briefs}</h2>
        {!briefs.length && <p className="sub">{t.none}</p>}
        {briefs.map((b) => <BriefCard key={b.id} b={b} />)}
      </section>
      <section aria-label={t.instances}>
        <h2>{t.scheduled}</h2>
        <ul aria-label={t.instanceList} className={instances.length ? "rows" : "plain"}>
          {instances.map((i) => (
            <li key={i.id}>{i.id} · {i.pack} · {i.params.schedule ?? "—"}{i.params.projects?.length ? ` · ${i.params.projects.join(t.listSep)}` : ""}{i.enabled ? "" : t.disabled}</li>
          ))}
        </ul>
        <form className="panel" onSubmit={(e) => void create(e)}>
          <label>{t.pack}
            <select value={pack || packs[0] || ""} onChange={(e) => setPack(e.target.value)}>
              {packs.map((p) => <option key={p}>{p}</option>)}
            </select>
          </label>
          <label>{t.schedule}<input value={schedule} onChange={(e) => setSchedule(e.target.value)} placeholder="0 8 * * 1-5" /></label>
          <label>{t.projects}<input value={projects} onChange={(e) => setProjects(e.target.value)} /></label>
          <button type="submit" disabled={!packs.length}>{t.create}</button>
        </form>
      </section>
    </main>
  );
}
