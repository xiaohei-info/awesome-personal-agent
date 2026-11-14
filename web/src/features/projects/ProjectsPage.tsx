import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link, Route, Routes, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { ProjectMaterials } from "../materials-hosted/ProjectMaterials";
import type { Item } from "../items/ItemsPage";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";
import { PROJECT_STATUS } from "../../app/ids";
import { Pick, Val } from "../../app/values";

// 形状对应 GET/POST /v1/projects、GET/PATCH /v1/projects/{id}、POST /v1/projects/{id}/repos、
// GET /v1/settings/executors、PUT /v1/settings/projects/{id}/allowed-executors（附录 B.2）。
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
  capture?: boolean;
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
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [intro, setIntro] = useState("");
  const [tags, setTags] = useState("");

  const load = useCallback(
    () => api.get<{ projects: Project[] }>("/projects").then((r) => setProjects(r.projects), (e) => setError(msg(e))),
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
      setError(msg(err));
    }
  };

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error && <p role="alert">{error}</p>}
      {projects && !projects.length && !error && <p className="empty">{t.none}</p>}
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
  const [loadErr, setLoadErr] = useState<string | null>(null);

  const load = useCallback(
    () => api.get<Project>("/projects/" + encodeURIComponent(id)).then(setP),
    [api, id],
  );
  useEffect(() => {
    setP(null);
    setLoadErr(null);
    load().catch((e) => setLoadErr(msg(e)));
  }, [load]);

  if (loadErr) return <main><p role="alert">{loadErr}</p><Link to="/projects">{t.back}</Link></main>;
  if (!p) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <p className="eyebrow"><Link to="/projects">{t.back}</Link></p>
      <h1>{p.name}</h1>
      <EditForm key={p.id + p.name} p={p} onSaved={setP} />
      <Repos p={p} onChange={setP} />
      <Executors p={p} onSaved={setP} />
      <ProjectItems id={p.id} />
      <ProjectMaterials project={p.id} />
    </main>
  );
}

// 每个写操作的错误显示在各自区块，成功后用服务端返回值覆盖本地状态。
function useAct() {
  const [error, setError] = useState<string | null>(null);
  const run = async (fn: () => Promise<void>) => {
    try {
      await fn();
      setError(null);
    } catch (e) {
      setError(msg(e));
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
      {error && <p role="alert">{error}</p>}
    </form>
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
      {error && <p role="alert">{error}</p>}
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
      <div className="actions">
        <label>{t.host} <input value={host} onChange={(e) => setHost(e.target.value)} /></label>
        <label>{t.path} <input value={path} onChange={(e) => setPath(e.target.value)} /></label>
        <button onClick={() => op({ op: "set_checkout", remote: r.remote, host, path })}>{t.setCheckout}</button>
      </div>
    </li>
  );
}

interface Executor { id: string; kind: string }

function Executors({ p, onSaved }: { p: Project; onSaved: (p: Project) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { error, run } = useAct();
  const [list, setList] = useState<Executor[] | null>(null);
  const [limited, setLimited] = useState(p.allowed_executors !== null);
  const [sel, setSel] = useState<string[]>(p.allowed_executors ?? []);
  const [loadErr, setLoadErr] = useState<string | null>(null);

  useEffect(() => {
    api.get<{ executors: Executor[] }>("/settings/executors").then((r) => setList(r.executors), (e) => setLoadErr(msg(e)));
  }, [api]);

  // 名单里已有但清单里没有的 id 也要显示，否则保存会悄悄丢掉它。
  const ids = [...new Set([...(list ?? []).map((e) => e.id), ...(p.allowed_executors ?? [])])];
  const toggle = (id: string) => setSel((s) => (s.includes(id) ? s.filter((x) => x !== id) : [...s, id]));
  const save = () =>
    run(async () => {
      await api.put(`/settings/projects/${p.id}/allowed-executors`, { executors: limited ? sel : null });
      onSaved(await api.get<Project>("/projects/" + p.id));
    });

  return (
    <section aria-label={t.executors}>
      <h2>{t.executors}</h2>
      {loadErr && <p role="alert">{loadErr}</p>}
      <div className="panel">
      <label><input type="radio" name="lim" checked={!limited} onChange={() => setLimited(false)} />{t.unlimited}</label>
      <label><input type="radio" name="lim" checked={limited} onChange={() => setLimited(true)} />{t.limited}</label>
      {limited && (
        <ul className="plain">
          {ids.map((id) => (
            <li key={id}><label><input type="checkbox" checked={sel.includes(id)} onChange={() => toggle(id)} />{id}</label></li>
          ))}
        </ul>
      )}
      <div><button className="primary" onClick={save}>{t.saveExecutors}</button></div>
      </div>
      {error && <p role="alert">{error}</p>}
    </section>
  );
}

function ProjectItems({ id }: { id: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    api.get<{ items: Item[] }>("/items?project=" + encodeURIComponent(id)).then((r) => setItems(r.items), (e) => setError(msg(e)));
  }, [api, id]);
  return (
    <section aria-label={t.items}>
      <h2>{t.items}</h2>
      {error && <p role="alert">{error}</p>}
      {items && !items.length && <p className="sub">{t.noItems}</p>}
      {!!items?.length && (
        <ul className="rows">
          {items.map((i) => (
            <li key={i.id}><Link to={"/items/" + i.id}>{i.title}</Link> <span className="badge plain"><Val k="phase" id={i.phase} /></span></li>
          ))}
        </ul>
      )}
    </section>
  );
}
