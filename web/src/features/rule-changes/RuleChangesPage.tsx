import { useCallback, useEffect, useState } from "react";
import { EmptyState, Skeleton } from "../../app/ui";
import { useSearchParams } from "react-router-dom";
import { ErrorNote } from "../../app/errors";
import { locale, useT } from "../../app/i18n";
import { PageHead } from "../../app/page";
import { useSession } from "../../app/session";
import { Val } from "../../app/values";
import type { AgentView } from "../settings-agents/AgentPick";
import { strings } from "./strings";

// 形状对应 GET /v1/audit/rule-changes（kernel/why.RuleChange）。能否撤回、为什么撤不回都由服务端判定，页面只显示。
interface Change {
  seq: number;
  at: string;
  actor: { type: string; principal: string };
  agent?: string;
  command: string;
  object: string;
  sections?: string[];
  before?: unknown;
  after?: unknown;
  revert: { ok: boolean; why?: string };
}

const PAGE = 100;
const show = (v: unknown, none: string) => (v === undefined || v === null ? none : JSON.stringify(v));

function Row({ c, who, onRevert }: { c: Change; who: string; onRevert: (c: Change) => Promise<void> }) {
  const t = useT(strings);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const what = c.command === "policy" ? (c.sections ?? []).map((s) => t.section[s] ?? s).join(t.listSep) : t.command[c.command] ?? c.command;
  return (
    <article aria-label={`${what} #${c.seq}`}>
      <h3>{what}{c.command !== "policy" && c.object ? <span className="sub"> · {c.object}</span> : null}</h3>
      <p className="meta">{who} · {new Date(c.at).toLocaleString(locale())}</p>
      <dl className="pair">
        <div><dt>{t.before}</dt><dd><code>{show(c.before, t.none)}</code></dd></div>
        <div><dt>{t.after}</dt><dd><code>{show(c.after, t.none)}</code></dd></div>
      </dl>
      {!c.revert.ok ? (
        <p className="sub" data-testid={"why-" + c.seq}><Val k="revertWhy" id={c.revert.why} /></p>
      ) : asking ? (
        <div className="actions">
          <button className="primary" disabled={busy} onClick={async () => { setBusy(true); try { await onRevert(c); } finally { setBusy(false); setAsking(false); } }}>{t.confirmRevert}</button>
          <button onClick={() => setAsking(false)}>{t.cancel}</button>
        </div>
      ) : (
        <button onClick={() => setAsking(true)}>{t.revert}</button>
      )}
    </article>
  );
}

export function RuleChangesPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [params] = useSearchParams();
  const [agent, setAgent] = useState(params.get("agent") ?? ""); // Agent 页「它做过什么」带 ?agent= 进来
  const [agents, setAgents] = useState<AgentView["agents"]>([]);
  const [list, setList] = useState<Change[] | null>(null);
  const [more, setMore] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [asking, setAsking] = useState(false);
  const [done, setDone] = useState<string | null>(null);

  useEffect(() => {
    api.get<AgentView>("/agents").then((v) => setAgents(v.agents), () => setAgents([]));
  }, [api]);

  const query = (before = 0) => "/audit/rule-changes?" + new URLSearchParams({ limit: String(PAGE), ...(agent ? { agent } : {}), ...(before ? { before: String(before) } : {}) });
  const load = useCallback(async () => {
    setError(null);
    try {
      const r = await api.get<{ changes: Change[] }>(query());
      setList(r.changes);
      setMore(r.changes.length === PAGE);
    } catch (e) {
      setError(e);
    }
  }, [api, agent]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => void load(), [load]);

  const older = async () => {
    const last = list?.[list.length - 1];
    if (!last) return;
    try {
      const r = await api.get<{ changes: Change[] }>(query(last.seq));
      setList([...(list ?? []), ...r.changes]);
      setMore(r.changes.length === PAGE);
    } catch (e) {
      setError(e);
    }
  };
  const revert = async (c: Change) => {
    setError(null);
    try {
      await api.post("/audit/rule-changes/" + c.seq + "/revert");
      await load();
    } catch (e) {
      setError(e); // 409 带撤不回的原因：以服务端为准
    }
  };
  const revertAll = async () => {
    setError(null);
    setAsking(false);
    try {
      const r = await api.post<{ results: { seq: number; ok: boolean }[] }>("/audit/rule-changes/revert", { agent });
      const ok = r.results.filter((x) => x.ok).length;
      setDone(t.allDone(ok, r.results.length - ok));
      await load();
    } catch (e) {
      setError(e);
    }
  };
  const nameOf = (id?: string) => (id ? agents.find((a) => a.id === id)?.name || id : t.you);
  const who = (c: Change) => (c.actor.type === "human" ? t.you : nameOf(c.agent || c.actor.principal));
  const picked = agents.find((a) => a.id === agent);

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <div className="actions">
        <label>{t.byAgent}{" "}
          <select value={agent} onChange={(e) => { setAgent(e.target.value); setDone(null); }}>
            <option value="">{t.allAgents}</option>
            {agents.map((a) => <option key={a.id} value={a.id}>{a.name || a.id}</option>)}
          </select>
        </label>
        {agent && !asking && <button onClick={() => setAsking(true)}>{t.revertAll(picked?.name || agent)}</button>}
        {agent && asking && (
          <>
            <button className="danger" onClick={() => void revertAll()}>{t.confirmAll}</button>
            <button onClick={() => setAsking(false)}>{t.cancel}</button>
          </>
        )}
      </div>
      {done && <p role="status">{done}</p>}
      {error != null && (
        <div>
          <ErrorNote error={error} />
          <button onClick={() => void load()}>{t.retry}</button>
        </div>
      )}
      {list === null ? (
        error == null && <Skeleton label={t.loading} />
      ) : list.length === 0 ? (
        <EmptyState title={t.empty} />
      ) : (
        <>
          {list.map((c) => <Row key={c.seq} c={c} who={who(c)} onRevert={revert} />)}
          {more && <button onClick={() => void older()}>{t.more}</button>}
        </>
      )}
    </main>
  );
}
