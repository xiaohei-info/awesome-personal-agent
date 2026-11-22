import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 设置 › 对外接口：外部程序来向 APA 要数据用的钥匙与接口说明（§4.1、§10.3、§14.2；决定 77、79）。与「提醒渠道」的渠道钥匙不是一回事。
// 钥匙的 scope 决定能读什么（api/events、kernel/auth.IsExportExt）：export 读事项、材料并订阅事件；forward 只订阅事件。
// 页面不再让人选「用途」：默认签 export，勾「只允许订阅事件」才签 forward。接口说明只写已实现的路由（附录 B.2「拉取事件」、B.3）。
const mine = (scope?: string) => !!scope && ["export", "forward"].some((x) => scope === x || scope.startsWith(x + "."));
const eventsOnly = (scope: string) => scope === "forward" || scope.startsWith("forward.");

// 与 t.methods 一一对应。返回示例是形状示意，字段以实际返回为准。
const METHODS = [
  { path: "/items", params: "phase kind project tag q sort(updated|deadline|planned_for|priority) planned_for from to(≤62d) origin_source", example: "/items?phase=open&sort=deadline",
    reply: `{"items": [{"id": "itm_…", "kind": "task", "phase": "open", "title": "…", "project": "prj_…",
  "priority": "normal", "planned_for": "2026-11-23", "deadline": "2026-11-30T23:59:59+08:00",
  "tags": ["…"], "origin": {"source": "src_…", "ref": "…"}, "version": 3, "updated_at": "…"}],
 "last_rejections": {}}` },
  { path: "/items/{id}", params: "", example: "/items/itm_xxx",
    reply: `{"item": {"id": "itm_…", "title": "…", "body": "…", …},
 "progress": [{"id": "…", "text": "…", "actor": "…", "at": "…"}],
 "relations": [{"type": "relates", "from": "itm_…", "to": "itm_…"}],
 "correlation_keys": [], "last_rejection": null}` },
  { path: "/materials", params: "project", example: "/materials?project=prj_xxx",
    reply: `{"materials": [{"id": "mat_…", "project": "prj_…", "kind": "…", "title": "…",
  "form": "owned", "current_rev": "…", "open_url": "…"}]}` },
  { path: "/materials/{id}", params: "rev", example: "/materials/mat_xxx",
    reply: `{"material": {"id": "mat_…", "title": "…", …}, "relations": […],
 "revision": {"id": "…", "seq": 3, "content": "…Markdown…", "state": "…", "at": "…"}}` },
  { path: "/events", params: "cursor(0) limit(1–500, 100) wait(s, ≤60)", example: "/events?cursor=0&limit=100&wait=30",
    reply: `{"events": [{"seq": 42, "type": "item.created", "object": "itm_…", "title": "…",
  "actor": {"type": "user", "principal": "owner", "via": "web"}, "at": "2026-11-18T08:30:00Z"}],
 "next": 43}` },
];

interface Credential { id: string; kind: string; label?: string; scope?: string; revoked_at?: string }

export function SettingsOutwardApiPage() {
  const { api } = useSession();
  const t = useT(strings);
  // 明文只放在组件状态里，离开页面、主动关闭或再次签发即丢弃。
  const [secret, setSecret] = useState<string | null>(null);
  const [creds, setCreds] = useState<Credential[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [onlyEvents, setOnlyEvents] = useState(false);
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);
  const load = useCallback(
    () => api.get<{ credentials: Credential[] }>("/settings/ext-keys").then((r) => setCreds(r.credentials.filter((c) => mine(c.scope))), (e) => setError(msg(e))),
    [api],
  );
  useEffect(() => void load(), [load]);

  const issue = async () => {
    setBusy(true);
    setError(null);
    setSecret(null);
    const body: Record<string, string> = { scope: onlyEvents ? "forward" : "export" };
    if (label.trim()) body.label = label.trim();
    try {
      setSecret((await api.post<{ secret: string }>("/settings/ext-keys", body)).secret);
      setLabel("");
      setOnlyEvents(false);
    } catch (e) {
      setError(msg(e));
    } finally {
      setBusy(false);
    }
    await load();
  };
  const revoke = async (id: string) => {
    setError(null);
    try {
      await api.del("/settings/ext-keys/" + encodeURIComponent(id));
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  return (
    <main className="stack">
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <p className="sub">{t.hint}</p>
      <p className="sub">{t.notChannel}</p>
      {secret && (
        <div role="alert" className="secret">
          <p>{t.secretOnce}</p>
          <code>{secret}</code>
          <div><button onClick={() => setSecret(null)}>{t.savedIt}</button></div>
        </div>
      )}
      {error && <div role="alert">{error}</div>}
      <form className="panel stack" aria-label={t.issueTitle} onSubmit={(e) => { e.preventDefault(); void issue(); }}>
        <h2>{t.issueTitle}</h2>
        <label>{t.label}<input value={label} onChange={(e) => setLabel(e.target.value)} placeholder={t.labelHint} /></label>
        <label><input type="checkbox" checked={onlyEvents} onChange={(e) => setOnlyEvents(e.target.checked)} /> {t.onlyEvents}</label>
        <p className="sub">{onlyEvents ? t.canEvents : t.canAll}</p>
        <div><button type="submit" className="primary" disabled={busy}>{t.issue}</button></div>
      </form>
      {creds === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : creds.length === 0 ? (
        <p className="sub">{t.none}</p>
      ) : (
        <ul className="rows" aria-label={t.keys}>
          {creds.map((c) => (
            <li key={c.id}>
              <strong>{c.label || c.id}</strong> <span className="sub">{eventsOnly(c.scope!) ? t.canEvents : t.canAll}</span>
              {c.revoked_at ? <span className="badge">{t.revoked}</span> : <button className="danger" onClick={() => void revoke(c.id)}>{t.revoke}</button>}
            </li>
          ))}
        </ul>
      )}
      <ApiDoc />
    </main>
  );
}

/** 接口说明：基址取本页的访问地址，外部系统照着就能接。示例里的取值只是形状示意。 */
function ApiDoc() {
  const t = useT(strings);
  const base = window.location.origin + "/v1";
  const curl = (path: string) => `curl -H "Authorization: Bearer $APA_KEY" "${base}${path}"`;
  return (
    <section aria-label={t.docTitle} className="panel stack">
      <h2>{t.docTitle}</h2>
      <dl className="pair">
        <div><dt>{t.docBase}</dt><dd><code data-testid="api-base">{base}</code></dd></div>
        <div><dt>{t.docAuth}</dt><dd><code>Authorization: Bearer &lt;{t.docKey}&gt;</code></dd></div>
      </dl>
      <p className="sub">{t.docReadOnly}</p>
      {METHODS.map((m, i) => (
        <section key={m.path} aria-label={t.methods[i].name} className="stack">
          <h3>{t.methods[i].name} <code>GET {m.path}</code></h3>
          <p className="sub">{t.methods[i].what}</p>
          {m.params && <p className="sub">{t.params}：<code>{m.params}</code></p>}
          {m.path === "/events" && <ul className="plain sub">{t.eventNotes.map((n) => <li key={n}>{n}</li>)}</ul>}
          <pre><code>{curl(m.example)}</code></pre>
          <pre><code>{m.reply}</code></pre>
        </section>
      ))}
      <h3>{t.docErrors}</h3>
      <pre><code>{`{"code": "permission_denied", "why": "…", "next": "…"}`}</code></pre>
      <ul className="plain">{t.errors.map((e) => <li key={e[0]}><code>{e[0]}</code> {e[1]}</li>)}</ul>
    </section>
  );
}
