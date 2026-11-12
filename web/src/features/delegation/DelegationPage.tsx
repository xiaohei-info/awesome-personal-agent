import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 GET /v1/items?kind=delegation、GET /v1/audit/events?type=delegation.reported、GET /v1/executors?project=、
// PATCH /v1/items/{id}（改派，If-Match）、POST /v1/delegations（重新委派）。
// 项目允许的执行者（§14.7）由服务端校验；本页只显示"是否允许"提示与服务端拒绝的原因，不在前端做判定。
interface Child {
  id: string;
  phase: string;
  version: number;
  title: string;
  status_label?: string;
  project?: string;
  assignee?: string;
}
interface Evidence { commit?: string; pr?: string; branch?: string; link?: string }
interface Report { outcome: string; summary: string; evidence: Evidence }
interface Step { seq: number; type: string; object: string; at: string; payload?: { report?: Report } }
interface Executor { id: string; kind: string; host?: string; allowed?: boolean }

const PAGE = 500;
const TERMINAL = new Set(["done", "dropped"]);
const PHASE: Record<string, string> = { open: "进行中", waiting: "等待中", needs_input: "待本人", done: "已完成", dropped: "已丢弃" };
const OUTCOME: Record<string, string> = { progress: "进展", done: "完成", blocked: "阻塞" };

const msg = (e: unknown) => {
  if (!(e instanceof ApiError)) return e instanceof Error ? e.message : "request failed";
  const allowed = e.body.allowed ? `；该项目允许的执行者：${e.body.allowed.length ? e.body.allowed.join("、") : "仅本人"}` : "";
  return `${e.body.why}（${e.body.next}）${allowed}`;
};

function Row({ c, report, onDone }: { c: Child; report?: Step; onDone: () => Promise<void> }) {
  const { api } = useSession();
  const [mode, setMode] = useState<"reassign" | "redelegate" | null>(null);
  const [execs, setExecs] = useState<Executor[]>([]);
  const [pick, setPick] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const r = report?.payload?.report;
  const ev = r?.evidence ?? {};
  const terminal = TERMINAL.has(c.phase);

  const open = async (m: "reassign" | "redelegate") => {
    setError(null);
    setMode(m);
    setPick("");
    try {
      const q = c.project ? `?project=${encodeURIComponent(c.project)}` : "";
      setExecs((await api.get<{ executors: Executor[] }>("/executors" + q)).executors);
    } catch (e) {
      setError(msg(e));
    }
  };

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      if (mode === "reassign") {
        await api.patch(`/items/${encodeURIComponent(c.id)}`, { assignee: pick, reason: "reassigned from web" }, { "If-Match": String(c.version) });
      } else {
        const { relations } = await api.get<{ relations: { type: string; from: string; to: string }[] }>(`/items/${encodeURIComponent(c.id)}`);
        const parent = relations.find((x) => x.type === "parent" && x.from === c.id)?.to;
        if (!parent) throw new Error("找不到父事项，无法重新委派");
        await api.post("/delegations", { parent, assignee: pick, title: c.title, reason: "re-delegated from web" });
      }
      setMode(null);
      await onDone();
    } catch (e) {
      setError(msg(e)); // 含 21（名单外）、409（版本冲突）：以服务端判定为准
    } finally {
      setBusy(false);
    }
  };

  return (
    <article aria-label={c.title}>
      <h3>{c.title}</h3>
      <p className="meta">
        执行者：<strong>{c.assignee ?? "未指派"}</strong> · 状态：{PHASE[c.phase] ?? c.phase}
        {c.project && ` · 项目 ${c.project}`}
      </p>
      <p>
        进度：{c.status_label || "暂无回执"}
        {r && ` （${OUTCOME[r.outcome] ?? r.outcome} · ${new Date(report!.at).toLocaleString()}）`}
      </p>
      {(ev.commit || ev.pr || ev.branch || ev.link) && (
        <ul aria-label="证据">
          {ev.commit && <li>commit：{ev.commit}</li>}
          {ev.pr && <li>PR：{ev.pr}</li>}
          {ev.branch && <li>分支：{ev.branch}</li>}
          {ev.link && <li>链接：{ev.link}</li>}
        </ul>
      )}
      {!mode && (
        <div className="actions">
          <button onClick={() => open(terminal ? "redelegate" : "reassign")}>{terminal ? "重新委派" : "改派"}</button>
        </div>
      )}
      {mode && (
        <div className="actions">
          <select aria-label="目标执行者" value={pick} onChange={(e) => setPick(e.target.value)}>
            <option value="">选择执行者…</option>
            {execs.map((x) => (
              <option key={x.id} value={x.id}>
                {x.id}
                {x.allowed === false ? "（项目不允许）" : ""}
              </option>
            ))}
          </select>
          <button disabled={!pick || busy} onClick={submit}>
            确认{mode === "reassign" ? "改派" : "重新委派"}
          </button>
          <button disabled={busy} onClick={() => setMode(null)}>
            取消
          </button>
        </div>
      )}
      {error && <div role="alert">{error}</div>}
    </article>
  );
}

export function DelegationPage() {
  const { api } = useSession();
  const [items, setItems] = useState<Child[] | null>(null);
  const [reports, setReports] = useState<Map<string, Step>>(new Map());
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const { items } = await api.get<{ items: Child[] }>("/items?kind=delegation");
      const latest = new Map<string, Step>();
      for (let after = 0; ; ) {
        const { events } = await api.get<{ events: Step[] }>(`/audit/events?type=delegation.reported&after=${after}&limit=${PAGE}`);
        for (const s of events) latest.set(s.object, s); // 序号升序，后者覆盖前者
        if (events.length < PAGE) break;
        after = events[events.length - 1].seq;
      }
      setReports(latest);
      setItems(items);
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <main>
      <PageHead>任务委派</PageHead>
      {error && (
        <div role="alert">
          {error} <button onClick={load}>重试</button>
        </div>
      )}
      {items === null ? (
        !error && <p role="status">加载中…</p>
      ) : items.length === 0 ? (
        <p className="empty">没有委派</p>
      ) : (
        items.map((c) => <Row key={c.id} c={c} report={reports.get(c.id)} onDone={load} />)
      )}
    </main>
  );
}
