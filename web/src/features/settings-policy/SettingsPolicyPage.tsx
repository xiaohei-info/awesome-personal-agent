import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, lang, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 /v1/settings/{policy,allowlists,access-limits}（附录 B.2）。模型端点在"设置 › 模型"（features/settings-models）。
// 校验、规整、人格上限（约 400 token）全部由服务端判定，本页只搬运编辑结果并原样显示拒绝原因；被拒绝时什么都没保存，草稿保留。
const ROUTES = ["policy", "allowlists", "access-limits"] as const;
const LEVELS = ["read", "edit", "full"];
const WRITES = ["direct", "proposed"];

interface Rule { kind?: string; tag?: string; project?: string; category: string }
type Doc = Record<string, any> & { version: number };
type Row = [string, string];

const rowsOf = (m: Record<string, string>): Row[] => Object.entries(m ?? {});
const listRowsOf = (m: Record<string, string[]>): Row[] => Object.entries(m ?? {}).map(([k, v]) => [k, v.join(", ")]);

function toMap(rows: Row[], what: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of rows) {
    if (!k.trim() && !v.trim()) continue; // 完全空白的行视为没填
    if (k in out) throw new Error(strings[lang()].dup(what, k));
    out[k] = v;
  }
  return out;
}
const toListMap = (rows: Row[], what: string) =>
  Object.fromEntries(Object.entries(toMap(rows, what)).map(([k, v]) => [k, v.split(/[,，\n]/).map((s) => s.trim()).filter(Boolean)]));

function Card({ title, onSave, children }: { title: string; onSave: () => Promise<number>; children: ReactNode }) {
  const t = useT(strings);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  async function save() {
    setBusy(true);
    setStatus(null);
    try {
      setStatus({ ok: true, text: t.saved(await onSave()) });
    } catch (e) {
      setStatus({ ok: false, text: msg(e) });
    } finally {
      setBusy(false);
    }
  }
  return (
    <section aria-label={title} className="panel stack">
      <h2>{title}</h2>
      {children}
      <div>
        <button type="button" className="primary" disabled={busy} onClick={() => void save()} aria-label={t.saveCard(title)}>{t.save}</button>
      </div>
      {status && <p role={status.ok ? "status" : "alert"}>{status.text}</p>}
    </section>
  );
}

function Rows({ label, rows, onChange, options, keyLabel, valueLabel }: {
  label: string; rows: Row[]; onChange: (r: Row[]) => void; options?: string[]; keyLabel?: string; valueLabel?: string;
}) {
  const t = useT(strings);
  const kl = keyLabel ?? t.name, vl = valueLabel ?? t.value;
  const set = (i: number, j: 0 | 1, v: string) => onChange(rows.map((r, n) => (n === i ? (j ? [r[0], v] : [v, r[1]]) : r) as Row));
  return (
    <div role="group" aria-label={label} className="kv">
      {rows.map(([k, v], i) => (
        <div key={i}>
          <input aria-label={t.rowField(label, kl, i + 1)} value={k} onChange={(e) => set(i, 0, e.target.value)} />
          {options ? (
            <select aria-label={t.rowField(label, vl, i + 1)} value={v} onChange={(e) => set(i, 1, e.target.value)}>
              {options.map((o) => <option key={o}>{o}</option>)}
            </select>
          ) : (
            <input aria-label={t.rowField(label, vl, i + 1)} value={v} onChange={(e) => set(i, 1, e.target.value)} />
          )}
          <button type="button" aria-label={t.delRow(label, i + 1)} onClick={() => onChange(rows.filter((_, n) => n !== i))}>{t.del}</button>
        </div>
      ))}
      <button type="button" onClick={() => onChange([...rows, ["", options?.[0] ?? ""]])}>{t.addRow(label)}</button>
    </div>
  );
}

// 仅当本段的服务端内容真正变化（保存后返回规整结果、或重新加载）时，草稿才重置；同路由其它段保存不影响本段草稿。
function useDraft<T>(doc: unknown, init: () => T) {
  const [d, setD] = useState<T>(init);
  const sig = JSON.stringify(doc);
  useEffect(() => setD(init()), [sig]); // eslint-disable-line react-hooks/exhaustive-deps
  return [d, setD] as const;
}

type Save = (route: (typeof ROUTES)[number], body: unknown) => Promise<number>;

function WebOnlyCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [cats, setCats] = useDraft(doc.web_only, () => (doc.web_only.categories as string[]).join("\n"));
  const [rules, setRules] = useDraft(doc.web_only, () => (doc.web_only.rules as Rule[]).map((r) => [r.kind ?? "", r.tag ?? "", r.project ?? "", r.category]));
  const setRule = (i: number, j: number, v: string) => setRules(rules.map((r, n) => (n === i ? r.map((x, m) => (m === j ? v : x)) : r)));
  return (
    <Card title={t.webOnly} onSave={() => save("policy", {
      web_only: {
        categories: cats.split("\n").map((s) => s.trim()).filter(Boolean),
        rules: rules.map(([kind, tag, project, category]) => ({ kind, tag, project, category })),
      },
    })}>
      <label>{t.categories}<textarea value={cats} onChange={(e) => setCats(e.target.value)} /></label>
      {rules.map((r, i) => (
        <div key={i} role="group" aria-label={t.rule(i + 1)} className="kv-row">
          {t.ruleCols.map((n, j) => (
            <input key={n} aria-label={t.ruleField(i + 1, n)} placeholder={n} value={r[j]} onChange={(e) => setRule(i, j, e.target.value)} />
          ))}
          <button type="button" aria-label={t.delRule(i + 1)} onClick={() => setRules(rules.filter((_, n) => n !== i))}>{t.del}</button>
        </div>
      ))}
      <button type="button" onClick={() => setRules([...rules, ["", "", "", ""]])}>{t.addRule}</button>
    </Card>
  );
}

function SelectField({ label, value, onChange, options }: { label: string; value: string; onChange: (v: string) => void; options: string[] }) {
  return (
    <label>{label}
      <select value={value} onChange={(e) => onChange(e.target.value)}>{options.map((o) => <option key={o}>{o}</option>)}</select>
    </label>
  );
}

export function SettingsPolicyPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [docs, setDocs] = useState<Record<string, Doc> | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      const all = await Promise.all(ROUTES.map((r) => api.get<Doc>("/settings/" + r)));
      setDocs(Object.fromEntries(ROUTES.map((r, i) => [r, all[i]])));
    } catch (e) {
      setDocs(null);
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  const save: Save = async (route, body) => {
    const next = await api.put<Doc>("/settings/" + route, body);
    setDocs((d) => ({ ...d!, [route]: next }));
    return next.version;
  };

  if (error)
    return (
      <main>
        <PageHead>{t.title}</PageHead>
        <p role="alert">{error}</p>
        <button onClick={() => void load()}>{t.retry}</button>
      </main>
    );
  if (!docs) return <main><p role="status">{t.loading}</p></main>;
  const p = docs["policy"], al = docs["allowlists"], lim = docs["access-limits"];
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <WebOnlyCard doc={p} save={save} />
      <DisturbCard doc={p} save={save} />
      <MaterialCard doc={p} save={save} />
      <LimitCard doc={lim} save={save} />
      <p className="sub">
        {t.movedPre}
        <Link to="/settings/models">{t.movedLink}</Link>
        {t.movedPost}
      </p>
      <AllowlistCard doc={al} save={save} />
      <PersonaCard doc={p} save={save} />
    </main>
  );
}

interface Mute { scope: string; id: string; until?: string }
const MUTE_SCOPES = ["source", "project", "item"];
const pad = (n: number) => String(n).padStart(2, "0");
const toLocalInput = (iso?: string) => {
  if (!iso) return "";
  const d = new Date(iso);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
};

// 保存时带上整段 disturb：PUT 替换整段，只发 routes 会抹掉静默、合并窗口、豁免和推迟判定。
function DisturbCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [rows, setRows] = useDraft(doc.disturb, () => rowsOf(doc.disturb.routes));
  const [cap, setCap] = useDraft(doc.disturb, () => String(doc.disturb.daily_cap ?? 20));
  const [mutes, setMutes] = useDraft(doc.disturb, () => ((doc.disturb.mutes ?? []) as Mute[]).map((m) => [m.scope, m.id, toLocalInput(m.until)]));
  const setMute = (i: number, j: number, v: string) => setMutes(mutes.map((r, n) => (n === i ? r.map((x, m) => (m === j ? v : x)) : r)));
  return (
    <Card title={t.disturb} onSave={() => {
      const n = Number(cap);
      if (cap.trim() === "" || !Number.isInteger(n) || n < 0) return Promise.reject(new Error(t.capInvalid));
      const list: Mute[] = mutes
        .filter(([, id]) => id.trim())
        .map(([scope, id, until]) => ({ scope, id: id.trim(), ...(until ? { until: new Date(until).toISOString() } : {}) }));
      return save("policy", { disturb: { ...doc.disturb, routes: toMap(rows, t.disturb), daily_cap: n, mutes: list } });
    }}>
      <p className="sub">{t.disturbHint}</p>
      <Rows label={t.routes} keyLabel={t.category} valueLabel={t.route} rows={rows} onChange={setRows} />
      <label>{t.capLabel}
        <input type="number" min={0} aria-label={t.cap} value={cap} onChange={(e) => setCap(e.target.value)} />
      </label>
      <p className="sub">{t.muteHint}</p>
      {mutes.map((m, i) => (
        <div key={i} role="group" aria-label={t.mute(i + 1)} className="kv-row">
          <select aria-label={t.muteScope(i + 1)} value={m[0]} onChange={(e) => setMute(i, 0, e.target.value)}>
            {MUTE_SCOPES.map((o) => <option key={o}>{o}</option>)}
          </select>
          <input aria-label={t.muteId(i + 1)} placeholder="id" value={m[1]} onChange={(e) => setMute(i, 1, e.target.value)} />
          <input type="datetime-local" aria-label={t.muteUntil(i + 1)} value={m[2]} onChange={(e) => setMute(i, 2, e.target.value)} />
          <button type="button" aria-label={t.delMute(i + 1)} onClick={() => setMutes(mutes.filter((_, n) => n !== i))}>{t.del}</button>
        </div>
      ))}
      <button type="button" onClick={() => setMutes([...mutes, [MUTE_SCOPES[0], "", ""]])}>{t.addMute}</button>
    </Card>
  );
}

function MaterialCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [def, setDef] = useDraft(doc.material_write, () => doc.material_write.default as string);
  const [rows, setRows] = useDraft(doc.material_write, () => rowsOf(doc.material_write.kinds));
  return (
    <Card title={t.materialWrite} onSave={() => save("policy", { material_write: { default: def, kinds: toMap(rows, t.materialWrite) } })}>
      <SelectField label={t.defaultWrite} value={def} onChange={setDef} options={WRITES} />
      <Rows label={t.materialKind} keyLabel="kind" valueLabel={t.strategy} rows={rows} onChange={setRows} options={WRITES} />
    </Card>
  );
}

function LimitCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [def, setDef] = useDraft(doc.permission_cap, () => doc.permission_cap.default as string);
  const [rows, setRows] = useDraft(doc.permission_cap, () => rowsOf(doc.permission_cap.by_harness));
  return (
    <Card title={t.limits} onSave={() => save("access-limits", { permission_cap: { default: def, by_harness: toMap(rows, t.limits) } })}>
      <SelectField label={t.defaultCap} value={def} onChange={setDef} options={LEVELS} />
      <Rows label={t.harnessCap} keyLabel="harness" valueLabel={t.capValue} rows={rows} onChange={setRows} options={LEVELS} />
    </Card>
  );
}

function AllowlistCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [fwd, setFwd] = useDraft(doc.forward_allowlist, () => listRowsOf(doc.forward_allowlist));
  const [ans, setAns] = useDraft(doc.channel_answerers, () => listRowsOf(doc.channel_answerers));
  return (
    <Card title={t.allowlists} onSave={() => save("allowlists", {
      forward_allowlist: toListMap(fwd, t.forward),
      channel_answerers: toListMap(ans, t.answerersList),
    })}>
      <p className="sub">{t.allowHint}</p>
      <Rows label={t.forward} keyLabel={t.source} valueLabel={t.sender} rows={fwd} onChange={setFwd} />
      <Rows label={t.answerers} keyLabel={t.channel} valueLabel={t.answerer} rows={ans} onChange={setAns} />
    </Card>
  );
}

function PersonaCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const [text, setText] = useDraft(doc.persona, () => doc.persona as string);
  return (
    <Card title={t.persona} onSave={() => save("policy", { persona: text })}>
      <label>{t.personaLabel}<textarea value={text} onChange={(e) => setText(e.target.value)} /></label>
    </Card>
  );
}
