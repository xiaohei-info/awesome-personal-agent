import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg, useLang, useT, type Lang } from "../../app/i18n";
import { useRaw } from "../../app/raw";
import { strings } from "./strings";

// 设置 › 提醒渠道的内置渠道（界面设计 §9.9）：卡片只显示现状；添加走四步：选类型 → 填表单 → 测试 → 启用。
// 表单由接入声明（GET /connections/types）生成；凭据只能填写和替换，读接口只回是否已设置。
// 「用它来送」写打扰策略的路由（disturb.routes 的类别键 → channel:<连接 id>）。规则与校验都在服务端。

type Text = { zh: string; en: string };
interface Field { key: string; kind: string; label: Text; hint?: Text; default?: string; required?: boolean; group?: string; choices?: { value: string; label: Text }[] }
export interface Decl { type: string; title: Text; tagline: Text; default: Text; via: Text; capabilities: { buttons: boolean; reply: boolean; identify: boolean }; fields: Field[]; guide?: Text[] }
export interface Conn {
  id: string; type: string; name: string; config: Record<string, string>; secrets: Record<string, boolean>; enabled: boolean;
  state: "ok" | "problem" | "disabled"; problem?: string; detail?: string; last_ok_at?: string;
}
interface TestResult { ok: boolean; problem?: string; detail?: string }

export const CLASSES = ["decision", "reminder", "brief", "health"] as const;
const DEFAULT_ROUTE: Record<string, string> = { decision: "inject", reminder: "inject", brief: "inject", health: "web" };
const routeOf = (id: string) => "channel:" + id;

/** 新路由表：checked 里的类别指向这个连接；原来指向它而这次没勾的类别回到默认。 */
export function withRoutes(routes: Record<string, string>, id: string, checked: string[]): Record<string, string> {
  const out = { ...routes };
  for (const c of CLASSES) {
    if (checked.includes(c)) out[c] = routeOf(id);
    else if (out[c] === routeOf(id)) out[c] = DEFAULT_ROUTE[c];
  }
  return out;
}

const usedFor = (routes: Record<string, string>, id: string) => CLASSES.filter((c) => routes[c] === routeOf(id));
const viewOnly = (d?: Decl) => !!d && !d.capabilities.buttons && !d.capabilities.reply;
export const time = (s?: string) => (s ? new Date(s).toLocaleString(undefined, { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "");

export function Connections({ routes, saveRoutes }: { routes: Record<string, string>; saveRoutes: (r: Record<string, string>) => Promise<unknown> }) {
  const t = useT(strings);
  const { api } = useSession();
  const [conns, setConns] = useState<Conn[] | null>(null);
  const [types, setTypes] = useState<Decl[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const load = useCallback(async () => {
    try {
      const [c, d] = await Promise.all([
        api.get<{ connections: Conn[] }>("/connections?direction=channel"),
        api.get<{ types: Decl[] }>("/connections/types?direction=channel"),
      ]);
      setConns(c.connections);
      setTypes(d.types);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);
  if (error) return <p role="alert">{error}</p>;
  if (!conns) return null;
  const decl = (type: string) => types.find((d) => d.type === type);
  return (
    <>
      {conns.map((c) => (
        <ConnCard key={c.id} c={c} d={decl(c.type)} routes={routes} saveRoutes={saveRoutes} onChange={() => void load()} />
      ))}
      {adding ? (
        <AddWizard types={types} routes={routes} saveRoutes={saveRoutes} onDone={() => { setAdding(false); void load(); }} />
      ) : (
        <div><button type="button" className="dashed" onClick={() => setAdding(true)}>+ {t.addChannel}</button></div>
      )}
    </>
  );
}

function ConnCard({ c, d, routes, saveRoutes, onChange }: {
  c: Conn; d?: Decl; routes: Record<string, string>; saveRoutes: (r: Record<string, string>) => Promise<unknown>; onChange: () => void;
}) {
  const t = useT(strings);
  const lang = useLang();
  const raw = useRaw();
  const { api } = useSession();
  const [editing, setEditing] = useState(false);
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const run = async (f: () => Promise<unknown>) => {
    setBusy(true);
    setNote(null);
    try {
      await f();
    } catch (e) {
      setNote({ ok: false, text: msg(e) });
    } finally {
      setBusy(false);
    }
  };
  const test = () => run(async () => {
    const r = await api.post<TestResult>(`/connections/${encodeURIComponent(c.id)}/test`);
    setNote(r.ok ? { ok: true, text: t.testSent } : { ok: false, text: t.problem[r.problem ?? "temporary"] ?? t.problem.temporary });
    onChange();
  });
  const toggle = () => run(async () => {
    await api.post(`/connections/${encodeURIComponent(c.id)}/${c.enabled ? "disable" : "enable"}`);
    onChange();
  });
  const remove = () => run(async () => {
    if (!window.confirm(t.confirmRemove(c.name))) return;
    if (c.enabled) await api.post(`/connections/${encodeURIComponent(c.id)}/disable`); // 先停止发送，再删
    await api.del(`/connections/${encodeURIComponent(c.id)}`);
    if (usedFor(routes, c.id).length) await saveRoutes(withRoutes(routes, c.id, []));
    onChange();
  });
  const used = usedFor(routes, c.id).map((k) => t.classes[k]);
  const badge = c.state === "ok" ? ["ok", t.inUse] : c.state === "problem" ? ["warn", t.hasProblem] : ["plain", t.disabled];
  return (
    <section aria-label={c.name} className="panel stack">
      <div className="conn-head">
        <h2>{c.name}</h2> {d && <span className="sub">{d.title[lang]}</span>} <span className={"badge " + badge[0]}>{badge[1]}</span>
        <span className="grow" />
        {c.enabled && <button type="button" disabled={busy} onClick={() => void test()}>{t.sendTest}</button>}
        <button type="button" disabled={busy} onClick={() => setEditing(!editing)}>{t.edit}</button>
        <button type="button" disabled={busy} onClick={() => void toggle()}>{c.enabled ? t.disable : t.enable}</button>
        <button type="button" className="danger" disabled={busy} onClick={() => void remove()}>{t.remove}</button>
      </div>
      {d && <Caps d={d} t={t} />}
      {c.state === "problem" && c.problem && (
        <p role="alert" className="conn-problem">
          {t.problem[c.problem] ?? t.problem.temporary}{" "}
          <button type="button" onClick={() => setEditing(true)}>{t.refill}</button>
        </p>
      )}
      <p className="sub">
        {c.enabled
          ? [c.last_ok_at ? t.lastSent(time(c.last_ok_at)) : t.neverSent, used.length ? t.usedFor(used.join(lang === "zh" ? "、" : ", ")) : t.notUsed].join(" · ")
          : t.stoppedNote}
      </p>
      {d && <p className="sub">{t.via(d.via[lang])}</p>}
      {raw && <p className="sub"><code>{t.rawId(c.id, c.type)}{c.problem ? ` · ${c.problem}${c.detail ? ": " + c.detail : ""}` : ""}</code></p>}
      {note && <p role={note.ok ? "status" : "alert"}>{note.text}</p>}
      {editing && d && (
        <EditForm c={c} d={d} routes={routes} saveRoutes={saveRoutes} onDone={() => { setEditing(false); onChange(); }} />
      )}
    </section>
  );
}

function Caps({ d, t }: { d: Decl; t: (typeof strings)["zh"] }) {
  const caps = viewOnly(d) ? [t.viewOnly, t.formLink] : [d.capabilities.buttons && t.buttons, d.capabilities.reply && t.reply].filter(Boolean);
  return <p className="chips">{caps.map((x) => <span key={String(x)} className="badge plain">{x}</span>)}</p>;
}

/** 表单字段：凭据已设置时显示「已设置 · 替换」，替换后才出现输入框；高级字段折叠。 */
function Fields({ d, lang, config, setConfig, secrets, setSecrets, saved }: {
  d: Decl; lang: Lang; config: Record<string, string>; setConfig: (c: Record<string, string>) => void;
  secrets: Record<string, string>; setSecrets: (s: Record<string, string>) => void; saved?: Record<string, boolean>;
}) {
  const t = useT(strings);
  const [replacing, setReplacing] = useState<Record<string, boolean>>({});
  const one = (f: Field) => {
    const label = f.label[lang];
    if (f.kind === "secret") {
      if (saved?.[f.key] && !replacing[f.key])
        return (
          <p key={f.key} className="line">{label}: <span className="sub">{t.secretSet} ••••••</span>{" "}
            <button type="button" onClick={() => setReplacing({ ...replacing, [f.key]: true })}>{t.replace}</button></p>
        );
      return (
        <label key={f.key}>{label}
          <input type="password" autoComplete="off" value={secrets[f.key] ?? ""} onChange={(e) => setSecrets({ ...secrets, [f.key]: e.target.value })} />
          <span className="sub">{t.secretHint}</span>
        </label>
      );
    }
    if (f.kind === "toggle")
      return (
        <label key={f.key}><input type="checkbox" checked={(config[f.key] ?? f.default) === "true"} onChange={(e) => setConfig({ ...config, [f.key]: String(e.target.checked) })} /> {label}</label>
      );
    if (f.kind === "select")
      return (
        <label key={f.key}>{label}
          <select value={config[f.key] ?? f.default ?? ""} onChange={(e) => setConfig({ ...config, [f.key]: e.target.value })}>
            {f.choices?.map((c) => <option key={c.value} value={c.value}>{c.label[lang]}</option>)}
          </select>
        </label>
      );
    if (f.kind === "identify") return null; // 识别流程写入，PR3 起用到
    return (
      <label key={f.key}>{label}
        <input type={f.kind === "url" ? "url" : f.kind === "number" ? "number" : "text"} value={config[f.key] ?? f.default ?? ""}
          onChange={(e) => setConfig({ ...config, [f.key]: e.target.value })} />
        {f.hint && <span className="sub">{f.hint[lang]}</span>}
      </label>
    );
  };
  const main = d.fields.filter((f) => f.group !== "advanced"), adv = d.fields.filter((f) => f.group === "advanced");
  return (
    <>
      {main.map(one)}
      {adv.length > 0 && <details><summary>{t.more}</summary><div className="stack">{adv.map(one)}</div></details>}
    </>
  );
}

/** 只送表单里改过的值：配置整体送（服务端按声明补默认值），凭据只送填了的。 */
const filled = (s: Record<string, string>) => Object.fromEntries(Object.entries(s).filter(([, v]) => v !== ""));
const missing = (d: Decl, config: Record<string, string>, secrets: Record<string, string>, saved?: Record<string, boolean>) =>
  d.fields.some((f) => f.required && f.kind !== "identify" && !(f.kind === "secret" ? secrets[f.key] || saved?.[f.key] : config[f.key] ?? f.default));

function UseFor({ checked, setChecked }: { checked: string[]; setChecked: (c: string[]) => void }) {
  const t = useT(strings);
  return (
    <fieldset>
      <legend>{t.useFor}</legend>
      <div className="chips">
        {CLASSES.map((c) => (
          <label key={c}><input type="checkbox" checked={checked.includes(c)}
            onChange={(e) => setChecked(e.target.checked ? [...checked, c] : checked.filter((x) => x !== c))} /> {t.classes[c]}</label>
        ))}
      </div>
    </fieldset>
  );
}

function EditForm({ c, d, routes, saveRoutes, onDone }: {
  c: Conn; d: Decl; routes: Record<string, string>; saveRoutes: (r: Record<string, string>) => Promise<unknown>; onDone: () => void;
}) {
  const t = useT(strings);
  const lang = useLang();
  const { api } = useSession();
  const [config, setConfig] = useState(c.config);
  const [secrets, setSecrets] = useState<Record<string, string>>({});
  const [checked, setChecked] = useState<string[]>(usedFor(routes, c.id));
  const [error, setError] = useState<string | null>(null);
  const save = async () => {
    setError(null);
    try {
      await api.put(`/connections/${encodeURIComponent(c.id)}`, { config, secrets: filled(secrets) });
      const next = withRoutes(routes, c.id, checked);
      if (JSON.stringify(next) !== JSON.stringify(routes)) await saveRoutes(next);
      onDone();
    } catch (e) {
      setError(msg(e));
    }
  };
  return (
    <form className="stack" aria-label={t.edit} onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <Fields d={d} lang={lang} config={config} setConfig={setConfig} secrets={secrets} setSecrets={setSecrets} saved={c.secrets} />
      <UseFor checked={checked} setChecked={setChecked} />
      <div className="conn-actions">
        <button type="submit" className="primary" disabled={missing(d, config, secrets, c.secrets)}>{t.save}</button>
        <button type="button" onClick={onDone}>{t.cancel}</button>
      </div>
      {error && <p role="alert">{error}</p>}
    </form>
  );
}

function AddWizard({ types, routes, saveRoutes, onDone }: {
  types: Decl[]; routes: Record<string, string>; saveRoutes: (r: Record<string, string>) => Promise<unknown>; onDone: () => void;
}) {
  const t = useT(strings);
  const lang = useLang();
  const { api } = useSession();
  const [d, setD] = useState<Decl | null>(null);
  const [step, setStep] = useState(1);
  const [name, setName] = useState("");
  const [config, setConfig] = useState<Record<string, string>>({});
  const [secrets, setSecrets] = useState<Record<string, string>>({});
  const [result, setResult] = useState<TestResult | null>(null);
  const [testing, setTesting] = useState(false);
  const [checked, setChecked] = useState<string[]>(["decision", "reminder"]);
  const [error, setError] = useState<string | null>(null);
  const pick = (x: Decl) => { setD(x); setName(x.default[lang]); setConfig({}); setSecrets({}); setResult(null); setStep(2); };
  const test = async () => {
    setTesting(true);
    setError(null);
    try {
      setResult(await api.post<TestResult>("/connections/test", { type: d!.type, config, secrets: filled(secrets), send: true }));
    } catch (e) {
      setError(msg(e));
    } finally {
      setTesting(false);
    }
  };
  const finish = async (enabled: boolean) => {
    setError(null);
    try {
      const c = await api.post<Conn>("/connections", { type: d!.type, name: name.trim(), config, secrets: filled(secrets), enabled });
      if (enabled && checked.length) await saveRoutes(withRoutes(routes, c.id, checked));
      onDone();
    } catch (e) {
      setError(msg(e));
    }
  };
  const head = (n: number, s: string) => <h2>{t.step(n, s)}</h2>;
  return (
    <section aria-label={t.addChannel} className="panel stack">
      {step === 1 && (
        <>
          {head(1, t.stepType)}
          {types.map((x) => (
            <button key={x.type} type="button" className="choice" onClick={() => pick(x)}>
              <strong>{x.title[lang]}</strong> <span className="sub">{x.tagline[lang]}</span>
            </button>
          ))}
          <div><button type="button" onClick={onDone}>{t.cancel}</button></div>
        </>
      )}
      {step === 2 && d && (
        <form className="stack" onSubmit={(e) => { e.preventDefault(); setStep(3); }}>
          {head(2, t.stepForm)}
          <p className="sub">{d.title[lang]} · {t.via(d.via[lang])}</p>
          <label>{t.name}<input value={name} onChange={(e) => setName(e.target.value)} /></label>
          <Fields d={d} lang={lang} config={config} setConfig={setConfig} secrets={secrets} setSecrets={setSecrets} />
          {d.guide && d.guide.length > 0 && (
            <details><summary>{t.howTo}</summary><ol>{d.guide.map((g, i) => <li key={i}>{g[lang]}</li>)}</ol></details>
          )}
          <div className="conn-actions">
            <button type="submit" className="primary" disabled={!name.trim() || missing(d, config, secrets)}>{t.next}</button>
            <button type="button" onClick={() => setStep(1)}>{t.back}</button>
          </div>
        </form>
      )}
      {step === 3 && d && (
        <>
          {head(3, t.stepTest)}
          <div className="conn-actions">
            <button type="button" disabled={testing} onClick={() => void test()}>{testing ? t.testing : t.sendOne}</button>
          </div>
          {result && <p role={result.ok ? "status" : "alert"}>{result.ok ? t.testSent : t.problem[result.problem ?? "temporary"] ?? t.problem.temporary}</p>}
          <div className="conn-actions">
            <button type="button" className="primary" onClick={() => setStep(4)}>{t.next}</button>
            <button type="button" onClick={() => setStep(2)}>{t.back}</button>
          </div>
        </>
      )}
      {step === 4 && d && (
        <>
          {head(4, t.stepEnable)}
          <UseFor checked={checked} setChecked={setChecked} />
          <div className="conn-actions">
            <button type="button" className="primary" onClick={() => void finish(true)}>{t.enableNow}</button>
            <button type="button" onClick={() => void finish(false)}>{t.keepOff}</button>
          </div>
        </>
      )}
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
