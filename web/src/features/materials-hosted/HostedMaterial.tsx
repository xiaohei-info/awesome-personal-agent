import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/materials/{id}、GET /v1/materials/{id}/content、POST /v1/materials/{id}/changes（附录 B.2）。
// 读取是异步的（reading → ready|unavailable），"暂不可读"是降级结果不是错误；
// 允许的执行者校验、父事项来源都在服务端，本页只提交并如实展示服务端的回答。
export interface HostedMaterialInfo {
  id: string;
  title: string;
  kind: string;
  project: string;
  location?: string;
}
type Content =
  | { state: "reading" }
  | { state: "ready"; content: string; commit: string; repo: string; path: string }
  | { state: "unavailable"; reason: string; repo: string; path: string };
interface Registered {
  repo: string;
  path: string;
  commit?: string;
}


export function HostedMaterial({ material, pollMs = 3000 }: { material: HostedMaterialInfo; pollMs?: number }) {
  const { api } = useSession();
  const t = useT(strings);
  const id = encodeURIComponent(material.id);
  const [reg, setReg] = useState<Registered | null>(null);
  const [content, setContent] = useState<Content | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let dead = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    api.get<{ hosted?: Registered }>(`/materials/${id}`).then((r) => !dead && setReg(r.hosted ?? null), () => {});
    const poll = async () => {
      try {
        const c = await api.get<Content>(`/materials/${id}/content`);
        if (dead) return;
        setError(null);
        setContent(c);
        if (c.state === "reading") timer = setTimeout(poll, pollMs);
      } catch (e) {
        if (!dead) setError(msg(e));
      }
    };
    void poll();
    return () => {
      dead = true;
      clearTimeout(timer);
    };
  }, [api, id, pollMs]);

  const repo = content && content.state !== "reading" ? content.repo : reg?.repo;
  const path = content && content.state !== "reading" ? content.path : reg?.path;
  const commit = content?.state === "ready" ? content.commit : reg?.commit;

  return (
    <section role="region" aria-label={t.detail} className="panel">
      <h3>
        {material.title} <small>{material.kind} · {material.project || t.global} · {t.hosted}</small>
      </h3>
      <p className="sub">
        {t.registered(repo ?? material.location ?? "")}
        {path ? ` · ${path}` : ""}
        {commit ? t.lastCommit(commit) : ""}
      </p>
      {error && <div role="alert">{error}</div>}
      {content?.state === "reading" && <p role="status">{t.reading}</p>}
      {content?.state === "unavailable" && <p role="status">{t.unavailable(content.reason)}</p>}
      {content?.state === "ready" && (
        <>
          <p className="sub">{t.defaultBranch(content.commit)}</p>
          <pre data-testid="hosted-content">{content.content}</pre>
        </>
      )}
      <ChangeForm material={material} />
    </section>
  );
}

function ChangeForm({ material }: { material: HostedMaterialInfo }) {
  const { api } = useSession();
  const t = useT(strings);
  const [executors, setExecutors] = useState<{ id: string }[]>([]);
  const [assignee, setAssignee] = useState("");
  const [request, setRequest] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<{ item: string; parent: string } | null>(null);
  // 结果未知时的重试复用同一个键，服务端重放首次结果而不建第二套事项；成功或明确被拒后换新键。
  const key = useRef(crypto.randomUUID());

  useEffect(() => {
    api.get<{ executors: { id: string }[] }>("/settings/executors").then((r) => setExecutors(r.executors), (e) => setError(msg(e)));
  }, [api]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      const r = await api.post<{ item: string; parent: string }>(
        `/materials/${encodeURIComponent(material.id)}/changes`,
        { assignee, request: request.trim() },
        { "Idempotency-Key": key.current },
      );
      key.current = crypto.randomUUID();
      setError(null);
      setDone(r);
      setRequest("");
    } catch (err) {
      // 4xx 是明确被拒，服务端没有记录这个键；换新键，否则改正后重提会被当作旧请求重放。结果未知（网络错误、5xx）则保留键以便安全重试。
      if (err instanceof ApiError && err.status >= 400 && err.status < 500) key.current = crypto.randomUUID();
      setDone(null);
      setError(msg(err));
    }
  };

  return (
    <form aria-label={t.change} onSubmit={submit}>
      <h4>{t.change}</h4>
      <p className="sub">{t.changeHint}</p>
      <label>
        {t.executor}
        <select value={assignee} onChange={(e) => setAssignee(e.target.value)}>
          <option value="">{t.choose}</option>
          {executors.map((x) => (
            <option key={x.id}>{x.id}</option>
          ))}
        </select>
      </label>
      <label>
        {t.request}
        <textarea value={request} onChange={(e) => setRequest(e.target.value)} rows={4} />
      </label>
      <button type="submit">{t.change}</button>
      {error && <p role="alert">{error}</p>}
      {done && (
        <p role="status">
          {t.started}
          <Link to={"/items/" + done.item}>{done.item}</Link>
          {t.parent}
          <Link to={"/items/" + done.parent}>{done.parent}</Link>
        </p>
      )}
    </form>
  );
}
