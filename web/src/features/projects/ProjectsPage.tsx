import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, Route, Routes, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { Go, PageHead } from "../../app/page";
import { loadDone, type Done } from "../done/DonePage";
import { hostClock } from "../items/schedule";
import { ProjectMaterials } from "../materials-hosted/ProjectMaterials";
import type { Item } from "../items/ItemsPage";
import { useT } from "../../app/i18n";
import { strings } from "./strings";
import { PROJECT_STATUS } from "../../app/ids";
import { Pick, Val } from "../../app/values";
import { ErrorNote } from "../../app/errors";
import { ScopeRadios } from "../sources/Outbound";

// 形状对应 GET/POST /v1/projects、GET/PATCH /v1/projects/{id}、POST /v1/projects/{id}/repos、
// GET /v1/executors、PUT /v1/settings/projects/{id}/allowed-executors（附录 B.2；body 带 executors 与 match）。
// 校验、授权、事件都在服务端；本页只搬运输入并显示服务端的错误原因。
interface Repo { remote: string; checkouts?: { host: string; path: string }[] }
export interface Project {
  id: string;
  name: string;
  intro?: string;
  tags?: string[];
  status: string;
  repos?: Repo[];
  allowed_executors: string[] | null;
  /** 按机器或宿主匹配的名单（§14.7）；与 allowed_executors 任一命中即允许。 */
  allowed_executor_match?: Match[];
  capture?: boolean;
  /** 对话增量送外部模型的范围；null 为还没选，没有缺省值（§6.5、§14.14）。 */
  capture_outbound?: string | null;
}

const splitTags = (s: string) => s.split(",").map((t) => t.trim()).filter(Boolean);

export function ProjectsPage() {
  return (
    <Routes>
      <Route index element={<ProjectList />} />
      <Route path=":id" element={<ProjectDetail />} />
    </Routes>
  );
}

function ProjectList() {
  const t = useT(strings);
  const { api } = useSession();
  const [projects, setProjects] = useState<Project[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [name, setName] = useState("");
  const [intro, setIntro] = useState("");
  const [tags, setTags] = useState("");

  const load = useCallback(
    () => api.get<{ projects: Project[] }>("/projects").then((r) => setProjects(r.projects), (e) => setError(e)),
    [api],
  );
  useEffect(() => void load(), [load]);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.post("/projects", { name, intro, tags: splitTags(tags) });
      setError(null);
      setName("");
      setIntro("");
      setTags("");
      await load();
    } catch (err) {
      setError(err);
    }
  };

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      {projects && !projects.length && error == null && <p className="empty">{t.none}</p>}
      {!!projects?.length && (
        <ul aria-label={t.list} className="rows">
          {projects.map((p) => (
            <li key={p.id}>
              <Link to={p.id}>{p.name}</Link> <span className={"badge" + (p.status === "active" ? " ok" : "")}><Val k="projectStatus" id={p.status} /></span>
              {p.tags?.length ? <span className="sub">{p.tags.map((t) => "#" + t).join(" ")}</span> : null}
            </li>
          ))}
        </ul>
      )}
      <h2>{t.create}</h2>
      <form className="panel" onSubmit={create}>
        <label>{t.name} <input value={name} onChange={(e) => setName(e.target.value)} /></label>
        <label>{t.intro} <input value={intro} onChange={(e) => setIntro(e.target.value)} /></label>
        <label>{t.tags} <input value={tags} onChange={(e) => setTags(e.target.value)} /></label>
        <button type="submit">{t.create}</button>
      </form>
    </main>
  );
}

function ProjectDetail() {
  const t = useT(strings);
  const { id = "" } = useParams();
  const { api } = useSession();
  const [p, setP] = useState<Project | null>(null);
  const [loadErr, setLoadErr] = useState<unknown>(null);

  const load = useCallback(
    () => api.get<Project>("/projects/" + encodeURIComponent(id)).then(setP),
    [api, id],
  );
  useEffect(() => {
    setP(null);
    setLoadErr(null);
    load().catch((e) => setLoadErr(e));
  }, [load]);

  if (loadErr != null) return <main><ErrorNote error={loadErr} /><Link to="/projects">{t.back}</Link></main>;
  if (!p) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <p className="eyebrow"><Link to="/projects">{t.back}</Link></p>
      <h1>{p.name}</h1>
      {p.intro && <p className="lede">{p.intro}</p>}
      <ProjectItems id={p.id} />
      <ProjectMaterials project={p.id} />
      {/* 进项目是来看事的：名称、仓库、执行者这些设置低频，收在页底，仍是它们唯一的编辑处。 */}
      <details>
        <summary>{t.settings}</summary>
        <EditForm key={p.id + p.name} p={p} onSaved={setP} />
        <Repos p={p} onChange={setP} />
        <Executors p={p} onSaved={setP} />
      </details>
    </main>
  );
}

// 每个写操作的错误显示在各自区块，成功后用服务端返回值覆盖本地状态。
function useAct() {
  const [error, setError] = useState<unknown>(null);
  const run = async (fn: () => Promise<void>) => {
    try {
      await fn();
      setError(null);
    } catch (e) {
      setError(e);
    }
  };
  return { error, run };
}

function EditForm({ p, onSaved }: { p: Project; onSaved: (p: Project) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { error, run } = useAct();
  const [name, setName] = useState(p.name);
  const [intro, setIntro] = useState(p.intro ?? "");
  const [tags, setTags] = useState((p.tags ?? []).join(", "));
  const [status, setStatus] = useState(p.status);
  const [capture, setCapture] = useState(!!p.capture);

  const save = (e: FormEvent) => {
    e.preventDefault();
    // 不带 allowed_executors：它只能经 settings 路由修改（§14.7）。
    return run(async () => onSaved(await api.patch<Project>("/projects/" + p.id, { name, intro, tags: splitTags(tags), status, capture })));
  };
  return (
    <form className="panel" onSubmit={save} aria-label={t.props}>
      <label>{t.name} <input value={name} onChange={(e) => setName(e.target.value)} /></label>
      <label>{t.intro} <input value={intro} onChange={(e) => setIntro(e.target.value)} /></label>
      <label>{t.tags} <input value={tags} onChange={(e) => setTags(e.target.value)} /></label>
      <label>
        {t.status}
        <Pick k="projectStatus" value={status} onChange={setStatus} ids={PROJECT_STATUS} />
      </label>
      <label><input type="checkbox" checked={capture} onChange={(e) => setCapture(e.target.checked)} /> {t.capture}</label>
      <button type="submit">{t.saveProject}</button>
      {error != null && <ErrorNote error={error} />}
    </form>
  );
}

/** 从聊天里记下的内容发给外部模型时送多少：PUT /settings/projects/{id}/capture-outbound {value}，只 Web；不预选。
 * 在「设置 › 模型」的「模型端点能收哪些来源」里编辑（§14.14），项目页不再逐个配。 */
export function CaptureOutbound({ p, onSaved }: { p: Project; onSaved: (p: Project) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { error, run } = useAct();
  const saved = p.capture_outbound ?? null;
  const [draft, setDraft] = useState<string | null>(saved);
  const save = () =>
    run(async () => {
      const r = await api.put<{ value: string | null }>(`/settings/projects/${p.id}/capture-outbound`, { value: draft });
      onSaved({ ...p, capture_outbound: r.value });
    });
  return (
    <section className="panel" aria-label={t.captureOut}>
      <ScopeRadios name={`${p.id}-capture`} legend={t.captureOut} value={draft} onChange={setDraft} />
      {saved === null && <span className="badge warn">{t.notChosen}</span>}{" "}
      <button type="button" disabled={draft === saved} onClick={() => void save()}>{t.save}</button>
      {error != null && <ErrorNote error={error} />}
    </section>
  );
}

function Repos({ p, onChange }: { p: Project; onChange: (p: Project) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { error, run } = useAct();
  const [remote, setRemote] = useState("");
  const op = (body: Record<string, string>) => run(async () => onChange(await api.post<Project>(`/projects/${p.id}/repos`, body)));

  return (
    <section aria-label={t.repos}>
      <h2>{t.repos}</h2>
      {error != null && <ErrorNote error={error} />}
      {!!p.repos?.length && (
        <ul className="rows">
          {p.repos.map((r) => <RepoRow key={r.remote} r={r} op={op} />)}
        </ul>
      )}
      <div className="actions">
        <label>{t.remote} <input value={remote} onChange={(e) => setRemote(e.target.value)} /></label>
        <button onClick={() => op({ op: "add_remote", remote }).then(() => setRemote(""))}>{t.addRepo}</button>
      </div>
    </section>
  );
}

function RepoRow({ r, op }: { r: Repo; op: (b: Record<string, string>) => Promise<void> }) {
  const t = useT(strings);
  const [host, setHost] = useState("");
  const [path, setPath] = useState("");
  return (
    <li>
      <code>{r.remote}</code>
      <button className="danger" onClick={() => op({ op: "remove_remote", remote: r.remote })}>{t.removeRepo}</button>
      {!!r.checkouts?.length && (
        <ul className="plain">
          {r.checkouts.map((c) => (
            <li key={c.host + c.path}>
              <span>{c.host}:{c.path}</span>{" "}
              <button className="quiet" onClick={() => op({ op: "remove_checkout", remote: r.remote, host: c.host, path: c.path })}>{t.removeCheckout}</button>
            </li>
          ))}
        </ul>
      )}
      {/* 本机检出：主机名须等于该机的主机名（§6.5），平时由 agent 维护，收在高级里。 */}
      <details>
        <summary>{t.advancedCheckout}</summary>
        <div className="actions">
          <label>{t.host} <input value={host} onChange={(e) => setHost(e.target.value)} /></label>
          <label>{t.path} <input value={path} onChange={(e) => setPath(e.target.value)} /></label>
          <button onClick={() => op({ op: "set_checkout", remote: r.remote, host, path })}>{t.setCheckout}</button>
        </div>
      </details>
    </li>
  );
}

interface Executor { id: string; kind: string; host?: string; harness?: string; label?: string }
interface Match { host?: string; harness?: string }

function Executors({ p, onSaved }: { p: Project; onSaved: (p: Project) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { error, run } = useAct();
  const [list, setList] = useState<Executor[] | null>(null);
  const match = p.allowed_executor_match ?? [];
  const [limited, setLimited] = useState(p.allowed_executors !== null || match.length > 0);
  const [sel, setSel] = useState<string[]>(p.allowed_executors ?? []);
  const [hosts, setHosts] = useState<string[]>(match.filter((m) => m.host && !m.harness).map((m) => m.host!));
  const [harnesses, setHarnesses] = useState<string[]>(match.filter((m) => m.harness && !m.host).map((m) => m.harness!));
  const both = match.filter((m) => m.host && m.harness); // 页面不新建这种组合，原样保留
  const [loadErr, setLoadErr] = useState<unknown>(null);

  useEffect(() => {
    api.get<{ executors: Executor[] }>("/executors").then((r) => setList(r.executors), (e) => setLoadErr(e));
  }, [api]);

  // 名单里已有但名录里没有的 id、机器、宿主也要显示，否则保存会悄悄丢掉它。
  const ids = [...new Set([...(list ?? []).map((e) => e.id), ...(p.allowed_executors ?? [])])];
  const allHosts = [...new Set([...(list ?? []).map((e) => e.host ?? ""), ...hosts].filter(Boolean))].sort();
  const allHarnesses = [...new Set([...(list ?? []).map((e) => e.harness ?? ""), ...harnesses].filter(Boolean))].sort();
  const byId = new Map((list ?? []).map((e) => [e.id, e]));
  const flip = (set: (f: (s: string[]) => string[]) => void, v: string) => set((s) => (s.includes(v) ? s.filter((x) => x !== v) : [...s, v]));
  const save = () =>
    run(async () => {
      const m: Match[] = limited ? [...hosts.map((host) => ({ host })), ...harnesses.map((harness) => ({ harness })), ...both] : [];
      await api.put(`/settings/projects/${p.id}/allowed-executors`, { executors: limited ? sel : null, match: m });
      onSaved(await api.get<Project>("/projects/" + p.id));
    });
  const group = (title: string, values: string[], picked: string[], set: (f: (s: string[]) => string[]) => void, show: (v: string) => ReactNode) =>
    values.length > 0 && (
      <fieldset className="plain">
        <legend>{title}</legend>
        <ul className="plain">
          {values.map((v) => (
            <li key={v}><label><input type="checkbox" checked={picked.includes(v)} onChange={() => flip(set, v)} />{show(v)}</label></li>
          ))}
        </ul>
      </fieldset>
    );

  return (
    <section aria-label={t.executors}>
      <h2>{t.executors}</h2>
      {loadErr != null && <ErrorNote error={loadErr} />}
      <div className="panel">
      <label><input type="radio" name="lim" checked={!limited} onChange={() => setLimited(false)} />{t.unlimited}</label>
      <label><input type="radio" name="lim" checked={limited} onChange={() => setLimited(true)} />{t.limited}</label>
      {limited && group(t.byHost, allHosts, hosts, setHosts, (v) => v)}
      {limited && group(t.byHarness, allHarnesses, harnesses, setHarnesses, (v) => v)}
      {limited && group(t.byExecutor, ids, sel, setSel, (id) => <ExecutorName e={byId.get(id) ?? { id, kind: "" }} />)}
      {limited && both.length > 0 && <p className="sub">{both.map((m) => `${m.harness} @ ${m.host}`).join(", ")}</p>}
      {limited && sel.length === 0 && hosts.length === 0 && harnesses.length === 0 && both.length === 0 && <p className="sub">{t.onlyYou}</p>}
      {limited && (hosts.length > 0 || harnesses.length > 0) && <p className="sub">{t.selfReported}</p>}
      <div><button className="primary" onClick={save}>{t.saveExecutors}</button></div>
      </div>
      {error != null && <ErrorNote error={error} />}
    </section>
  );
}

/** 执行者：显示名 · 种类 · 机器（机器由宿主自报，§6.17）。 */
function ExecutorName({ e }: { e: Executor }) {
  return (
    <>
      {e.label || e.id}
      {e.kind && <span className="sub"> · <Val k="executorKind" id={e.kind} /></span>}
      {e.host && <span className="sub"> · {e.host}</span>}
    </>
  );
}

// 项目里的事按阶段分组（界面设计 §9.13）：等你决定、进行中、在等在前；已完成默认收起，最近完成的在前。
// 建议不在这里处理，只给条数并指向建议页；不做了的不列。
const GROUPS = ["needs_input", "open", "waiting"] as const;

function ProjectItems({ id }: { id: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<Item[] | null>(null);
  const [done, setDone] = useState<Done[]>([]);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    api.get<{ items: Item[] }>("/items?project=" + encodeURIComponent(id)).then((r) => setItems(r.items), (e) => setError(e));
    hostClock(api).then((c) => loadDone(api, c.utc_offset)).then((ds) => setDone(ds.filter((d) => d.item.project === id)), () => setDone([]));
  }, [api, id]);
  const proposed = items?.filter((i) => i.phase === "proposed").length ?? 0;
  const active = GROUPS.map((ph) => [ph, items?.filter((i) => i.phase === ph) ?? []] as const).filter(([, l]) => l.length);
  const row = (i: Item, sub?: string) => (
    <li key={i.id}><Go to={"/items/" + encodeURIComponent(i.id)}>{i.title}</Go>{sub && <span className="sub"> {sub}</span>}</li>
  );
  return (
    <section aria-label={t.items}>
      {error != null && <ErrorNote error={error} />}
      {items && !items.length && <p className="sub">{t.noItems}</p>}
      {!!items?.length && !active.length && <p className="sub">{t.noActive}</p>}
      {active.map(([ph, l]) => (
        <section key={ph} aria-label={t.phaseGroup[ph]}>
          <h2>{t.phaseGroup[ph]} <span className="sub">{l.length}</span></h2>
          <ul className="rows">{l.map((i) => row(i, i.planned_for && t.planned(i.planned_for)))}</ul>
        </section>
      ))}
      {proposed > 0 && <p className="sub">{t.proposed(proposed)} · <Link to="/proposals">{t.toProposals}</Link></p>}
      {done.length > 0 && (
        <details>
          <summary>{t.doneGroup(done.length)}</summary>
          <ul className="rows">{done.map((d) => row(d.item, t.doneOn(d.day)))}</ul>
        </details>
      )}
    </section>
  );
}
