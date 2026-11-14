import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { ActorBadge, type Step } from "../items/ItemsPage";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

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

const split = (s: string) => s.split(/[\s,，]+/).filter(Boolean);

function status(g: Grant, now = Date.now()): "revoked" | "expired" | "active" {
  if (g.revoked_at) return "revoked";
  return Date.parse(g.expires_at) <= now ? "expired" : "active";
}

function GrantRow({ g, onError, onChanged }: { g: Grant; onError: (m: string | null) => void; onChanged: () => Promise<void> }) {
  const { api } = useSession();
  const t = useT(strings);
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
      <strong>{g.id}</strong> <span className={"badge" + (st === "active" ? " ok" : st === "expired" ? " warn" : "")}>{t.status[st]}</span>
      {g.revoked_at && g.revoked_reason ? <span className="sub">{t.reasonParen(g.revoked_reason)}</span> : ""}
      <div className="sub full">
        {t.credential(g.grantee.credential)}
        {g.grantee.harness ? ` · harness ${g.grantee.harness}` : ""}
        {t.actions(g.actions.join(t.listSep))}
        {g.scope.projects.length ? t.scopeProjects(g.scope.projects.join(t.listSep)) : ""}
        {g.scope.kinds.length ? t.scopeKinds(g.scope.kinds.join(t.listSep)) : ""}
        {g.scope.items.length ? t.scopeItems(g.scope.items.join(t.listSep)) : ""}
        {t.expires(new Date(g.expires_at).toLocaleString(locale()))}
        {g.reason ? ` · ${g.reason}` : ""}
      </div>
      <div className="actions full">
        {st === "active" && (
          <>
            <label>
              {t.revokeReason}
              <input value={reason} onChange={(e) => setReason(e.target.value)} />
            </label>
            <button className="danger" disabled={!reason.trim()} onClick={revoke}>{t.revoke}</button>
          </>
        )}
        <button onClick={showUsage}>{t.usage}</button>
      </div>
      {usage && (usage.length === 0 ? <p className="sub full">{t.noUsage}</p> : (
        <ul aria-label={t.usage} className="plain">
          {usage.map((e) => (
            <li key={e.seq}>
              #{e.seq} · {e.type} · {e.object} · <ActorBadge actor={e.actor} /> · {new Date(e.at).toLocaleString(locale())}
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
  const t = useT(strings);
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
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error && <div role="alert">{error}</div>}
      {/* noValidate：有效期等校验以服务端为准，max 只是提示 */}
      <h2>{t.issueTitle}</h2>
      <form className="panel" aria-label={t.issueTitle} noValidate onSubmit={issue}>
        <label>
          {t.grantee}
          <select value={chosen} onChange={(e) => setCredential(e.target.value)}>
            {delegates.map((c) => (
              <option key={c.id} value={c.id}>{t.option(c.label || c.id, c.id)}</option>
            ))}
          </select>
        </label>
        <label>
          {t.harness}
          <input value={harness} onChange={(e) => setHarness(e.target.value)} />
        </label>
        <label>
          {t.projects}
          <input value={projects} onChange={(e) => setProjects(e.target.value)} />
        </label>
        <label>
          {t.kinds}
          <input value={kinds} onChange={(e) => setKinds(e.target.value)} />
        </label>
        <label>
          {t.items}
          <input value={items} onChange={(e) => setItems(e.target.value)} />
        </label>
        <fieldset className="checks">
          <legend>{t.actionsLegend}</legend>
          {GRANTABLE_ACTIONS.map((a) => (
            <label key={a}>
              <input type="checkbox" value={a} checked={actions.includes(a)} onChange={() => toggle(a)} />
              {a}
            </label>
          ))}
        </fieldset>
        <label>
          {t.days}
          <input type="number" min={1} max={MAX_DAYS} value={days} onChange={(e) => setDays(e.target.value)} />
        </label>
        <label>
          {t.reason}
          <input value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
        <div>
          <button type="submit" disabled={busy || !chosen || actions.length === 0}>{t.issue}</button>
        </div>
      </form>
      {grants === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : grants.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        <ul aria-label={t.list} className="rows">
          {grants.map((g) => (
            <GrantRow key={g.id} g={g} onError={setError} onChanged={load} />
          ))}
        </ul>
      )}
    </main>
  );
}
