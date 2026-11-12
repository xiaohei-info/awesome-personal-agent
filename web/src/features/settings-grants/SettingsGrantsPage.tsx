import { useCallback, useEffect, useState, type FormEvent } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { ActorBadge, type Step } from "../items/ItemsPage";

// 形状对应 /v1/settings/grants（policy.Grant）与 /v1/audit/events?grant=。有效期上限、动作白名单、凭据与范围校验都在服务端，
// 本页只搬运输入并如实展示服务端的回答；授权单内容不可改，所以没有编辑或延长，只有撤销后重签。
interface Grant {
  id: string;
  grantee: { credential: string; harness?: string };
  scope: { projects: string[]; kinds: string[]; items: string[] };
  actions: string[];
  expires_at: string;
  reason?: string;
  created_at: string;
  revoked_at?: string;
  revoked_reason?: string;
}
interface Credential { id: string; kind: string; label?: string; revoked_at?: string }
interface UsageEvent extends Step { object: string }

/** 附录 B.1「第二档（可用授权单）」一列；没有通配，项目/来源/能力包管理只接受本人原话，不在其中。 */
export const GRANTABLE_ACTIONS = [
  "item.create", "item.update", "item.transition", "item.relate",
  "decision.answer", "proposal.accept", "proposal.dismiss",
  "revision.confirm", "revision.reject",
  "delegate.report", "followmsg.handback",
];
const DEFAULT_DAYS = 7;
const MAX_DAYS = 90;
const DAY_MS = 86_400_000;

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const split = (s: string) => s.split(/[\s,，]+/).filter(Boolean);

function status(g: Grant, now = Date.now()) {
  if (g.revoked_at) return "已撤销";
  return Date.parse(g.expires_at) <= now ? "已过期" : "有效";
}

function GrantRow({ g, onError, onChanged }: { g: Grant; onError: (m: string | null) => void; onChanged: () => Promise<void> }) {
  const { api } = useSession();
  const [reason, setReason] = useState("");
  const [usage, setUsage] = useState<UsageEvent[] | null>(null);
  const st = status(g);

  const revoke = async () => {
    onError(null);
    try {
      await api.post("/settings/grants/" + encodeURIComponent(g.id) + "/revoke", { reason: reason.trim() });
    } catch (e) {
      onError(msg(e));
    }
    await onChanged(); // 以服务端状态为准
  };
  const showUsage = async () => {
    onError(null);
    try {
      setUsage((await api.get<{ events: UsageEvent[] }>("/audit/events?" + new URLSearchParams({ grant: g.id, limit: "100" }))).events);
    } catch (e) {
      onError(msg(e));
    }
  };

  return (
    <li>
      <strong>{g.id}</strong> <span className={"badge" + (st === "有效" ? " ok" : st === "已过期" ? " warn" : "")}>{st}</span>
      {g.revoked_at && g.revoked_reason ? <span className="sub">（{g.revoked_reason}）</span> : ""}
      <div className="sub full">
        凭据 {g.grantee.credential}
        {g.grantee.harness ? ` · harness ${g.grantee.harness}` : ""} · 动作 {g.actions.join("、")} · 范围
        {g.scope.projects.length ? ` 项目 ${g.scope.projects.join("、")}` : ""}
        {g.scope.kinds.length ? ` 类型 ${g.scope.kinds.join("、")}` : ""}
        {g.scope.items.length ? ` 事项 ${g.scope.items.join("、")}` : ""} · 到期 {new Date(g.expires_at).toLocaleString()}
        {g.reason ? ` · ${g.reason}` : ""}
      </div>
      <div className="actions full">
        {st === "有效" && (
          <>
            <label>
              撤销原因
              <input value={reason} onChange={(e) => setReason(e.target.value)} />
            </label>
            <button className="danger" disabled={!reason.trim()} onClick={revoke}>撤销</button>
          </>
        )}
        <button onClick={showUsage}>使用记录</button>
      </div>
      {usage && (usage.length === 0 ? <p className="sub full">还没有使用记录</p> : (
        <ul aria-label="使用记录" className="plain">
          {usage.map((e) => (
            <li key={e.seq}>
              #{e.seq} · {e.type} · {e.object} · <ActorBadge actor={e.actor} /> · {new Date(e.at).toLocaleString()}
              {e.reason ? ` · ${e.reason}` : ""}
            </li>
          ))}
        </ul>
      ))}
    </li>
  );
}

export function SettingsGrantsPage() {
  const { api } = useSession();
  const [grants, setGrants] = useState<Grant[] | null>(null);
  const [delegates, setDelegates] = useState<Credential[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [credential, setCredential] = useState("");
  const [harness, setHarness] = useState("");
  const [projects, setProjects] = useState("");
  const [kinds, setKinds] = useState("");
  const [items, setItems] = useState("");
  const [actions, setActions] = useState<string[]>([]);
  const [days, setDays] = useState(String(DEFAULT_DAYS));
  const [reason, setReason] = useState("");

  const load = useCallback(
    () =>
      api.get<{ grants: Grant[] }>("/settings/grants").then(
        (r) => setGrants(r.grants),
        (e) => setError(msg(e)),
      ),
    [api],
  );

  useEffect(() => {
    void load();
    api.get<{ credentials: Credential[] }>("/settings/tokens").then(
      (r) => setDelegates(r.credentials.filter((c) => c.kind === "delegate" && !c.revoked_at)),
      (e) => setError(msg(e)),
    );
  }, [api, load]);

  const chosen = credential || delegates[0]?.id || "";

  const issue = async (ev: FormEvent) => {
    ev.preventDefault();
    setBusy(true);
    setError(null);
    const grantee: Record<string, string> = { credential: chosen };
    if (harness.trim()) grantee.harness = harness.trim();
    const body: Record<string, unknown> = {
      grantee,
      scope: { projects: split(projects), kinds: split(kinds), items: split(items) },
      actions,
      expires_at: new Date(Date.now() + Number(days) * DAY_MS).toISOString(),
    };
    if (reason.trim()) body.reason = reason.trim();
    try {
      await api.post("/settings/grants", body);
      setProjects("");
      setKinds("");
      setItems("");
      setActions([]);
      setReason("");
      setDays(String(DEFAULT_DAYS));
    } catch (e) {
      setError(msg(e));
    } finally {
      setBusy(false);
    }
    await load();
  };

  const toggle = (a: string) => setActions((cur) => (cur.includes(a) ? cur.filter((x) => x !== a) : [...cur, a]));

  return (
    <main>
      <PageHead lede="让某个 agent 凭据在限定范围、限定期限内替你做决定。只在这里签发和撤销。">授权单</PageHead>
      {error && <div role="alert">{error}</div>}
      {/* noValidate：有效期等校验以服务端为准，max 只是提示 */}
      <h2>签发授权单</h2>
      <form className="panel" aria-label="签发授权单" noValidate onSubmit={issue}>
        <label>
          被授权凭据
          <select value={chosen} onChange={(e) => setCredential(e.target.value)}>
            {delegates.map((c) => (
              <option key={c.id} value={c.id}>{`${c.label || c.id}（${c.id}）`}</option>
            ))}
          </select>
        </label>
        <label>
          harness（可选）
          <input value={harness} onChange={(e) => setHarness(e.target.value)} />
        </label>
        <label>
          项目范围
          <input value={projects} onChange={(e) => setProjects(e.target.value)} />
        </label>
        <label>
          类型范围
          <input value={kinds} onChange={(e) => setKinds(e.target.value)} />
        </label>
        <label>
          事项范围
          <input value={items} onChange={(e) => setItems(e.target.value)} />
        </label>
        <fieldset className="checks">
          <legend>动作</legend>
          {GRANTABLE_ACTIONS.map((a) => (
            <label key={a}>
              <input type="checkbox" value={a} checked={actions.includes(a)} onChange={() => toggle(a)} />
              {a}
            </label>
          ))}
        </fieldset>
        <label>
          有效天数
          <input type="number" min={1} max={MAX_DAYS} value={days} onChange={(e) => setDays(e.target.value)} />
        </label>
        <label>
          原因
          <input value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
        <div>
          <button type="submit" disabled={busy || !chosen || actions.length === 0}>签发</button>
        </div>
      </form>
      {grants === null ? (
        !error && <p role="status">加载中…</p>
      ) : grants.length === 0 ? (
        <p className="empty">还没有授权单</p>
      ) : (
        <ul aria-label="授权单列表" className="rows">
          {grants.map((g) => (
            <GrantRow key={g.id} g={g} onError={setError} onChanged={load} />
          ))}
        </ul>
      )}
    </main>
  );
}
