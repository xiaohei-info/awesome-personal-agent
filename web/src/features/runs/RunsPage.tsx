import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 GET /v1/runs?state=&limit=、GET /v1/runs/{id}（设计 §6.11）。只读展示，不做任何判定；失败处理走 needs_input，不提供重试。
interface Run {
  id: string;
  category: string;
  runner?: string;
  launch?: string;
  target: string;
  harness?: string;
  session_id?: string;
  access?: string;
  basis?: { policy_version: number; rule_id?: string };
  chain_depth: number;
  state: string;
  exit_code?: number;
  started_at?: string;
  finished_at?: string;
  timeout?: number;
}

const LIMIT = 200;
const STATE: Record<string, string> = { queued: "排队中", running: "运行中", succeeded: "成功", failed: "失败", timeout: "超时" };

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const at = (s?: string) => (s ? new Date(s).toLocaleString() : "—");

function Card({ r }: { r: Run }) {
  const { api } = useSession();
  const [detail, setDetail] = useState<Run | null>(null);
  const [error, setError] = useState<string | null>(null);
  const d = detail ?? r;

  const open = async () => {
    setError(null);
    try {
      setDetail(await api.get<Run>(`/runs/${encodeURIComponent(r.id)}`));
    } catch (e) {
      setError(msg(e));
    }
  };

  return (
    <article aria-label={`运行 ${r.id}`}>
      <h3>
        {r.category} → {r.target}
      </h3>
      <p className="meta">
        <strong className={"badge" + (r.state === "succeeded" ? " ok" : r.state === "failed" || r.state === "timeout" ? " bad" : "")}>{STATE[r.state] ?? r.state}</strong>
        {r.runner && ` · ${r.runner}/${r.launch ?? "—"}`}
        {r.harness && ` · ${r.harness}`}
      </p>
      <p>
        会话：{r.session_id ? <code>{r.session_id}</code> : "无"}
      </p>
      <p>权限档位：{r.access ? <code>{r.access}</code> : "无"}</p>
      {r.basis ? (
        <p>
          授权依据：规则 {r.basis.rule_id ?? "—"} · 策略版本 {r.basis.policy_version}
        </p>
      ) : (
        <p>授权依据：无</p>
      )}
      {!detail && <div className="actions"><button onClick={open}>详情</button></div>}
      {detail && (
        <ul aria-label="执行统计">
          <li>链路深度 {d.chain_depth}</li>
          <li>开始 {at(d.started_at)}</li>
          <li>结束 {at(d.finished_at)}</li>
          {d.exit_code !== undefined && <li>退出码 {d.exit_code}</li>}
          {d.timeout ? <li>超时 {d.timeout} 秒</li> : null}
        </ul>
      )}
      {!detail && r.exit_code !== undefined && <p>退出码 {r.exit_code}</p>}
      {error && <div role="alert">{error}</div>}
    </article>
  );
}

export function RunsPage() {
  const { api } = useSession();
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [state, setState] = useState("");
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const q = `?limit=${LIMIT}` + (state ? `&state=${state}` : "");
      setRuns((await api.get<{ runs: Run[] }>("/runs" + q)).runs);
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api, state]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <main>
      <PageHead>运行记录</PageHead>
      <div className="filters">
      <label>
        状态
        <select aria-label="状态" value={state} onChange={(e) => setState(e.target.value)}>
          <option value="">全部</option>
          {Object.entries(STATE).map(([k, v]) => (
            <option key={k} value={k}>
              {v}
            </option>
          ))}
        </select>
      </label>
      </div>
      {error && (
        <div role="alert">
          {error} <button onClick={load}>重新加载</button>
        </div>
      )}
      {runs === null ? (
        !error && <p role="status">加载中…</p>
      ) : runs.length === 0 ? (
        <p className="empty">没有运行记录</p>
      ) : (
        [...runs].reverse().map((r) => <Card key={r.id} r={r} />)
      )}
    </main>
  );
}
