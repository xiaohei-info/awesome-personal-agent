import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { ApiError } from "../../app/api";
import { MATERIAL_RELATION_LABELS } from "../../app/relations";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { HostedMaterial } from "../materials-hosted/HostedMaterial";

// 形状对应 GET/POST /v1/materials、GET /v1/materials/{id}[/revisions]、POST /v1/materials/{id}/revisions|revert、
// POST /v1/revisions/{id}/confirm|reject、GET /v1/materials/search（附录 B.2）。
// 写入策略（direct/proposed）、基线冲突、规范生效只认 Web、授权都在服务端；本页只提交并如实展示服务端的回答。
// 托管型的只读展示与发起修改由 features/materials-hosted 负责。
interface Material {
  id: string;
  project: string;
  kind: string;
  title: string;
  tags?: string[];
  location?: string;
  current_rev?: string;
}
interface Revision {
  id: string;
  material: string;
  seq: number;
  content: string;
  actor: { type: string; principal: string };
  reason: string;
  based_on: string;
  state: "current" | "proposed" | "superseded";
  at: string;
}
interface Hit {
  id: string;
  title: string;
  kind: string;
  project: string;
  hosted: boolean;
  snippet?: string;
  path?: string;
}
interface Relation {
  type: string;
  from: string;
  to: string;
}
interface Detail {
  material: Material;
  relations: Relation[];
  titles: Record<string, string>;
  revision: Revision | null;
  revisions: Revision[];
}
interface Write {
  state: string;
}

// shortcut: kind 取内置集合（kernel/materials builtinKinds）；能力包扩展的 kind 暂无 HTTP 枚举接口，有了再改读接口。
const KINDS = ["minutes", "reference", "worknote", "requirement", "design", "guideline"];
// 规范的五个固定小节（§6.6），均可留空；空小节由服务端渲染时丢弃。
const GUIDELINE_TEMPLATE = ["处理规则", "架构边界", "开发规范", "设计约束", "验收要求"].map((h) => `## ${h}\n\n`).join("\n");

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

export interface DiffLine {
  op: " " | "-" | "+";
  text: string;
}

/** 按行 LCS 差异，只用于展示；修订内容本身不变（追加不改）。 */
export function diffLines(from: string, to: string): DiffLine[] {
  const a = from.split("\n");
  const b = to.split("\n");
  const t = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--)
    for (let j = b.length - 1; j >= 0; j--) t[i][j] = a[i] === b[j] ? t[i + 1][j + 1] + 1 : Math.max(t[i + 1][j], t[i][j + 1]);
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) out.push({ op: " ", text: a[i++] }), j++;
    else if (j < b.length && (i === a.length || t[i][j + 1] > t[i + 1][j])) out.push({ op: "+", text: b[j++] });
    else out.push({ op: "-", text: a[i++] });
  }
  return out;
}

function Diff({ id, from, to }: { id: string; from: string; to: string }) {
  return (
    <pre data-testid={`diff-${id}`}>
      {diffLines(from, to).map((l, k) => (
        <div key={k} data-op={l.op}>
          <span>{l.op}</span> <span>{l.text}</span>
        </div>
      ))}
    </pre>
  );
}

export function MaterialsPage() {
  const { api } = useSession();
  const [projects, setProjects] = useState<{ id: string; name?: string }[]>([]);
  const [project, setProject] = useState("");
  const [kind, setKind] = useState("");
  const [q, setQ] = useState("");
  const [item, setItem] = useState("");
  const [materials, setMaterials] = useState<Material[] | null>(null);
  const [hits, setHits] = useState<Hit[] | null>(null);
  // ?id= 让事项页的材料链接直达详情。
  const [selected, setSelected] = useState<string | null>(useSearchParams()[0].get("id"));
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const reload = useCallback(
    (p: string) =>
      api.get<{ materials: Material[] }>(p ? `/materials?project=${encodeURIComponent(p)}` : "/materials").then(
        (r) => (setMaterials(r.materials), setHits(null)),
        (e) => setError(msg(e)),
      ),
    [api],
  );
  useEffect(() => void reload(project), [reload, project]);
  useEffect(() => void api.get<{ projects: { id: string; name?: string }[] }>("/projects").then((r) => setProjects(r.projects), () => {}), [api]);

  const search = async () => {
    setError(null);
    if (!q.trim() && !item.trim()) return reload(project);
    const sp = new URLSearchParams();
    if (q.trim()) sp.set("q", q.trim());
    if (item.trim()) sp.set("item", item.trim());
    if (project) sp.set("project", project);
    if (kind) sp.set("kind", kind);
    try {
      setHits((await api.get<{ hits: Hit[] }>("/materials/search?" + sp)).hits);
    } catch (e) {
      setError(msg(e));
    }
  };

  const shown = materials?.filter((m) => !kind || m.kind === kind);
  const done = (text: string) => (setError(null), setNotice(text));
  const failed = (e: unknown) => (setNotice(null), setError(msg(e)));

  return (
    <main>
      <PageHead>材料</PageHead>
      {error && <div role="alert">{error}</div>}
      {notice && <div role="status">{notice}</div>}
      <div className="filters">
        <label>
          按项目筛选
          <select value={project} onChange={(e) => setProject(e.target.value)}>
            <option value="">全部项目</option>
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name ?? p.id}
              </option>
            ))}
          </select>
        </label>
        <label>
          按类型筛选
          <select value={kind} onChange={(e) => setKind(e.target.value)}>
            <option value="">全部类型</option>
            {KINDS.map((k) => (
              <option key={k}>{k}</option>
            ))}
          </select>
        </label>
        <label>
          搜索材料
          <input value={q} onChange={(e) => setQ(e.target.value)} />
        </label>
        <label>
          关联事项
          <input value={item} onChange={(e) => setItem(e.target.value)} />
        </label>
        <div className="actions">
          <button onClick={search}>搜索</button>
          <button onClick={() => setCreating(true)}>新建材料</button>
        </div>
      </div>

      {creating && (
        <CreateForm
          projects={projects}
          onCancel={() => setCreating(false)}
          onDone={(s) => {
            setCreating(false);
            done(s);
            void reload(project);
          }}
          onError={failed}
        />
      )}

      {hits ? (
        <ul className="rows">
          {hits.map((h) => (
            <li key={h.id} aria-label={h.id}>
              <button className="link" onClick={() => setSelected(h.id)}>{h.title}</button> <span className="sub">{h.kind} · {h.project || "全局"} · {h.hosted ? "托管型" : "自有型"}
              {h.path && <> · {h.path}</>}</span>
              {h.snippet && <p className="sub">{h.snippet}</p>}
            </li>
          ))}
          {!hits.length && <p className="empty">没有命中</p>}
        </ul>
      ) : (
        <>
          {materials && !shown?.length && !error && <p className="empty">还没有材料</p>}
          {!!shown?.length && (
            <ul className="rows">
              {shown.map((m) => (
                <li key={m.id}>
                  <button className="link" onClick={() => setSelected(m.id)}>{m.title}</button> <span className="sub">{m.kind} · {m.project || "全局"} · {m.location ? "托管型" : "自有型"}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      )}

      {selected && <MaterialDetail key={selected} id={selected} onNotice={done} onError={failed} onChanged={() => reload(project)} />}
    </main>
  );
}

function CreateForm({ projects, onCancel, onDone, onError }: { projects: { id: string; name?: string }[]; onCancel: () => void; onDone: (s: string) => void; onError: (e: unknown) => void }) {
  const { api } = useSession();
  const [f, setF] = useState({ kind: "minutes", project: "", title: "", content: "", reason: "" });
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const pickKind = (e: { target: { value: string } }) => {
    const k = e.target.value;
    setF({ ...f, kind: k, content: k === "guideline" && !f.content ? GUIDELINE_TEMPLATE : f.content === GUIDELINE_TEMPLATE ? "" : f.content });
  };
  const submit = async () => {
    try {
      const r = await api.post<{ state: string }>("/materials", { kind: f.kind, project: f.project, title: f.title.trim(), content: f.content, reason: f.reason.trim(), based_on: "" });
      onDone(written(r));
    } catch (e) {
      onError(e);
    }
  };
  return (
    <form
      className="panel"
      aria-label="新建材料"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <label>
        类型
        <select value={f.kind} onChange={pickKind}>
          {KINDS.map((k) => (
            <option key={k}>{k}</option>
          ))}
        </select>
      </label>
      <label>
        项目
        <select value={f.project} onChange={set("project")}>
          <option value="">全局（无项目）</option>
          {projects.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name ?? p.id}
            </option>
          ))}
        </select>
      </label>
      <label>
        标题
        <input value={f.title} onChange={set("title")} />
      </label>
      <label>
        Markdown 内容
        <textarea value={f.content} onChange={set("content")} rows={10} />
      </label>
      <label>
        修改原因
        <input value={f.reason} onChange={set("reason")} />
      </label>
      <div className="actions">
        <button type="submit">提交</button> <button type="button" onClick={onCancel}>取消</button>
      </div>
    </form>
  );
}

const written = (r: Write) => (r.state === "current" ? "已生效" : "已提交，待本人确认后生效");

function MaterialDetail(props: { id: string; onNotice: (s: string) => void; onError: (e: unknown) => void; onChanged: () => void }) {
  const { api } = useSession();
  const { id } = props;
  // 回调每次父渲染都是新函数；用 ref 持有，使重载只在 id 变化或显式写入后发生，不冲掉未保存的草稿。
  const cb = useRef(props);
  cb.current = props;
  const onNotice = (s: string) => cb.current.onNotice(s);
  const onError = (e: unknown) => cb.current.onError(e);
  const onChanged = () => cb.current.onChanged();
  const [d, setD] = useState<Detail | null>(null);
  const [hosted, setHosted] = useState(false);
  const [draft, setDraft] = useState("");
  const [reason, setReason] = useState("");
  const [open, setOpen] = useState<Record<string, "diff" | "content">>({});

  const load = useCallback(async () => {
    try {
      const r = await api.get<{ material: Material; revision: Revision | null; relations?: Relation[] }>(`/materials/${encodeURIComponent(id)}`);
      // 关联事项只取以本材料为终点的边（事项 → 材料）；标题读不到时退回 id。
      const relations = (r.relations ?? []).filter((x) => x.to === id);
      const titles: Record<string, string> = {};
      if (relations.length) await api.get<{ items: { id: string; title: string }[] }>("/items").then((l) => l.items.forEach((i) => (titles[i.id] = i.title)), () => {});
      if (r.material.location) return setHosted(true), setD({ material: r.material, relations, titles, revision: null, revisions: [] });
      const h = await api.get<{ revisions: Revision[] }>(`/materials/${encodeURIComponent(id)}/revisions`);
      const revisions = [...h.revisions].sort((a, b) => b.seq - a.seq);
      setD({ material: r.material, relations, titles, revision: r.revision, revisions });
      setDraft(r.revision?.content ?? "");
    } catch (e) {
      cb.current.onError(e);
    }
  }, [api, id]);
  useEffect(() => void load(), [load]);

  if (!d) return null;
  const m = d.material;
  const base = encodeURIComponent(m.id);
  const contentOf = (rid?: string) => d.revisions.find((r) => r.id === rid)?.content ?? "";
  const current = m.current_rev ?? "";
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    try {
      await fn();
      setReason("");
      onNotice(ok);
      await load();
      onChanged();
    } catch (e) {
      onError(e);
    }
  };

  const related = d.relations.length > 0 && (
    <section role="region" aria-label="关联事项">
      <h4>关联事项</h4>
      <ul>
        {d.relations.map((r) => (
          <li key={r.type + r.from}>
            {MATERIAL_RELATION_LABELS[r.type] ?? r.type} · <Link to={"/items/" + encodeURIComponent(r.from)}>{d.titles[r.from] ?? r.from}</Link>
          </li>
        ))}
      </ul>
    </section>
  );

  if (hosted)
    return (
      <>
        <HostedMaterial material={m} />
        {related}
      </>
    );

  const save = async () => {
    try {
      const r = await api.post<Write>(`/materials/${base}/revisions`, { content: draft, reason: reason.trim(), based_on: current });
      setReason("");
      onNotice(written(r));
      await load();
      onChanged();
    } catch (e) {
      onError(e);
    }
  };
  const pending = d.revisions.filter((r) => r.state === "proposed");
  const confirm = (r: Revision, on_stale: string, ok: string) => run(() => api.post(`/revisions/${encodeURIComponent(r.id)}/confirm`, on_stale ? { on_stale } : {}), ok);

  return (
    <section role="region" aria-label="材料详情" className="panel">
      <h3>
        {m.title} <small>{m.kind} · {m.project || "全局"}</small>
      </h3>

      {related}

      {pending.length > 0 && (
        <section role="region" aria-label="待确认修订">
          <h4>待确认修订</h4>
          {pending.map((r) => {
            const stale = r.based_on !== current;
            return (
              <div key={r.id} className="proposal">
                <p className="sub">
                  #{r.seq} · {r.actor.principal} · {r.reason}
                </p>
                {stale && <p role="note">基线已变化：当前修订已不是此提议所基于的修订，请选择取舍。</p>}
                <Diff id={r.id} from={contentOf(m.current_rev)} to={r.content} />
                {stale ? (
                  <>
                    <button onClick={() => confirm(r, "adopt", "已确认，修订已成为当前版本")}>仍然采用</button>{" "}
                    <button onClick={() => confirm(r, "rebase", "已基于当前重提，等待再次确认")}>基于当前重提</button>
                  </>
                ) : (
                  <button onClick={() => confirm(r, "", "已确认，修订已成为当前版本")}>确认生效</button>
                )}{" "}
                <button onClick={() => run(() => api.post(`/revisions/${encodeURIComponent(r.id)}/reject`, {}), "已拒绝，内容保留在版本列表")}>拒绝</button>
              </div>
            );
          })}
        </section>
      )}

      <label>
        Markdown 内容
        <textarea value={draft} onChange={(e) => setDraft(e.target.value)} rows={12} />
      </label>
      <label>
        修改原因
        <input value={reason} onChange={(e) => setReason(e.target.value)} />
      </label>
      <div className="actions">
        <button className="primary" onClick={save}>保存修订</button>
      </div>

      <section role="region" aria-label="版本列表">
        <h4>版本列表</h4>
        <ul className="rows">
          {d.revisions.map((r) => {
            const prev = d.revisions.find((x) => x.seq === r.seq - 1);
            const v = open[r.id];
            return (
              <li key={r.id}>
                <span>#{r.seq} · {r.state} · {r.actor.principal} · {r.reason}
                {r.based_on && <> · 基于 {r.based_on}</>} · {r.at}</span>{" "}
                <button onClick={() => setOpen({ ...open, [r.id]: v === "diff" ? undefined! : "diff" })}>查看 diff</button>{" "}
                <button onClick={() => setOpen({ ...open, [r.id]: v === "content" ? undefined! : "content" })}>查看内容</button>
                {r.state === "superseded" && (
                  <>
                    {" "}
                    <button onClick={() => run(() => api.post(`/materials/${base}/revert`, { to: r.id, reason: `回退到 #${r.seq}` }), "已回退")}>回退到此版</button>
                  </>
                )}
                {v === "diff" && <Diff id={r.id} from={prev?.content ?? ""} to={r.content} />}
                {v === "content" && <pre data-testid={`content-${r.id}`}>{r.content}</pre>}
              </li>
            );
          })}
        </ul>
      </section>
    </section>
  );
}
