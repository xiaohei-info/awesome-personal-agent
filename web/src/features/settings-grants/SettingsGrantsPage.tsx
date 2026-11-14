import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { ActorBadge, type Step } from "../items/ItemsPage";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { Val, useLabel } from "../../app/values";
import { GRANT_ACTION, ITEM_KIND } from "../../app/ids";
import { useNamed, useNames } from "../../app/names";
import { useRaw } from "../../app/raw";

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

/** 附录 B.1「第二档（可用授权单）」一列（与 kernel/policy.GrantableActions 对照，见 ids.check）；没有通配，项目/来源/能力包管理只接受本人原话，不在其中。 */
export const GRANTABLE_ACTIONS: readonly string[] = GRANT_ACTION;
// 按用户要做的事分组显示；组里的动作合起来正好是全集。
const ACTION_GROUPS: [string, string[]][] = [
  ["decide", ["decision.answer"]],
  ["proposals", ["proposal.accept", "proposal.dismiss", "proposal.attach"]],
  ["items", ["item.create", "item.update", "item.transition", "item.relate"]],
  ["docs", ["revision.confirm", "revision.reject"]],
  ["reports", ["delegate.report", "followmsg.handback"]],
];
const QUICK_DAYS = [1, 7, 30];
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
  const raw = useRaw();
  const label = useLabel();
  const cred = useNames("credential");
  const project = useNames("project");
  const scope = [
    ...g.scope.projects.map((p) => project(p)),
    ...g.scope.kinds.map((k) => label("itemKind", k)),
    ...g.scope.items,
  ].join(t.listSep);

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
    <li data-grant={g.id}>
      <strong>{t.can(cred(g.grantee.credential), g.actions.map((a) => label("grantAction", a)).join(t.listSep))}</strong> <span className={"badge" + (st === "active" ? " ok" : st === "expired" ? " warn" : "")}>{t.status[st]}</span>
      {g.revoked_at && g.revoked_reason ? <span className="sub">{t.reasonParen(g.revoked_reason)}</span> : ""}
      <div className="sub full">
        {scope && t.within(scope)}
        {g.grantee.harness ? t.harnessOnly(g.grantee.harness) : ""}
        {t.until(new Date(g.expires_at).toLocaleString(locale()))}
        {g.reason ? ` · ${g.reason}` : ""}
        {raw && <code className="raw-id">{g.id}</code>}
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
              #{e.seq} · <Val k="event" id={e.type} /> · {e.object} · <ActorBadge actor={e.actor} /> · {new Date(e.at).toLocaleString(locale())}
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
  const [projects, setProjects] = useState<string[]>([]);
  const [kinds, setKinds] = useState<string[]>([]);
  const allProjects = useNamed("project");
  const hosts = useHarnesses();
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
      scope: { projects, kinds, items: split(items) },
      actions,
      expires_at: new Date(Date.now() + Number(days) * DAY_MS).toISOString(),
    };
    if (reason.trim()) body.reason = reason.trim();
    try {
      await api.post("/settings/grants", body);
      setProjects([]);
      setKinds([]);
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
  const flip = (list: string[], set: (v: string[]) => void, x: string) => set(list.includes(x) ? list.filter((y) => y !== x) : [...list, x]);

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
              <option key={c.id} value={c.id}>{c.label || c.id}</option>
            ))}
          </select>
        </label>
        {delegates.length === 0 && <p className="sub">{t.noDelegate}</p>}
        <fieldset className="checks">
          <legend>{t.actionsLegend}</legend>
          {ACTION_GROUPS.map(([g, acts]) => (
            <div key={g}>
              <b className="sub">{t.groups[g]}</b>
              {acts.map((a) => (
                <label key={a}>
                  <input type="checkbox" value={a} checked={actions.includes(a)} onChange={() => toggle(a)} />
                  <Val k="grantAction" id={a} />
                </label>
              ))}
            </div>
          ))}
          <p className="sub full">{t.notGrantable}</p>
        </fieldset>
        <fieldset className="checks">
          <legend>{t.scopeLegend}</legend>
          <div role="group" aria-label={t.projects}>
            <b className="sub">{t.projects}</b>
            {allProjects.map((p) => (
              <label key={p.id}>
                <input type="checkbox" checked={projects.includes(p.id)} onChange={() => flip(projects, setProjects, p.id)} />
                {p.name}
              </label>
            ))}
          </div>
          <div role="group" aria-label={t.kinds}>
            <b className="sub">{t.kinds}</b>
            {ITEM_KIND.map((k) => (
              <label key={k}>
                <input type="checkbox" checked={kinds.includes(k)} onChange={() => flip(kinds, setKinds, k)} />
                <Val k="itemKind" id={k} />
              </label>
            ))}
          </div>
          <label className="full">
            {t.items}
            <input value={items} onChange={(e) => setItems(e.target.value)} />
          </label>
        </fieldset>
        <div>
          <span className="sub">{t.days}</span>
          <span className="sentence">
            {QUICK_DAYS.map((n) => (
              <button key={n} type="button" aria-pressed={days === String(n)} onClick={() => setDays(String(n))}>{t.dayN(n)}</button>
            ))}
            <input type="number" min={1} max={MAX_DAYS} aria-label={t.days} value={days} onChange={(e) => setDays(e.target.value)} />
          </span>
        </div>
        <label>
          {t.reason}
          <input value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
        <details>
          <summary>{t.advanced}</summary>
          <label>
            {t.harness}
            <select value={harness} onChange={(e) => setHarness(e.target.value)}>
              <option value="">{t.anyHarness}</option>
              {[...hosts, ...(harness && !hosts.includes(harness) ? [harness] : [])].map((h) => <option key={h} value={h}>{h}</option>)}
            </select>
          </label>
        </details>
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

/** 已确认声明的宿主名（授权单的「只限某个宿主」只用于收窄和审计，§14.5）。 */
function useHarnesses(): string[] {
  const { api } = useSession();
  const [list, setList] = useState<string[]>([]);
  useEffect(() => {
    api.get<{ declarations: { harness: string }[] }>("/settings/commands").then((r) => setList([...new Set(r.declarations.map((d) => d.harness))]), () => setList([]));
  }, [api]);
  return list;
}
