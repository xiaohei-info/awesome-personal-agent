import { useCallback, useEffect, useState, type ReactNode } from "react";
import { ApiError } from "../../app/api";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 /v1/settings/{policy,allowlists,access-limits}（附录 B.2）。模型端点在"设置 › 模型"（features/settings-models）。
// 校验、规整、人格上限（约 400 token）全部由服务端判定，本页只搬运编辑结果并原样显示拒绝原因；被拒绝时什么都没保存，草稿保留。
const ROUTES = ["policy", "allowlists", "access-limits"] as const;
const LEVELS = ["read", "edit", "full"];
const WRITES = ["direct", "proposed"];

interface Rule { kind?: string; tag?: string; project?: string; category: string }
type Doc = Record<string, any> & { version: number };
type Row = [string, string];

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const rowsOf = (m: Record<string, string>): Row[] => Object.entries(m ?? {});
const listRowsOf = (m: Record<string, string[]>): Row[] => Object.entries(m ?? {}).map(([k, v]) => [k, v.join(", ")]);

function toMap(rows: Row[], what: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of rows) {
    if (!k.trim() && !v.trim()) continue; // 完全空白的行视为没填
    if (k in out) throw new Error(`${what}：「${k}」重复`);
    out[k] = v;
  }
  return out;
}
const toListMap = (rows: Row[], what: string) =>
  Object.fromEntries(Object.entries(toMap(rows, what)).map(([k, v]) => [k, v.split(/[,，\n]/).map((s) => s.trim()).filter(Boolean)]));

function Card({ title, onSave, children }: { title: string; onSave: () => Promise<number>; children: ReactNode }) {
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  async function save() {
    setBusy(true);
    setStatus(null);
    try {
      setStatus({ ok: true, text: `已保存（版本 ${await onSave()}）` });
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
        <button type="button" className="primary" disabled={busy} onClick={() => void save()} aria-label={`保存${title}`}>保存</button>
      </div>
      {status && <p role={status.ok ? "status" : "alert"}>{status.text}</p>}
    </section>
  );
}

function Rows({ label, rows, onChange, options, keyLabel = "名称", valueLabel = "值" }: {
  label: string; rows: Row[]; onChange: (r: Row[]) => void; options?: string[]; keyLabel?: string; valueLabel?: string;
}) {
  const set = (i: number, j: 0 | 1, v: string) => onChange(rows.map((r, n) => (n === i ? (j ? [r[0], v] : [v, r[1]]) : r) as Row));
  return (
    <div role="group" aria-label={label} className="kv">
      {rows.map(([k, v], i) => (
        <div key={i}>
          <input aria-label={`${label}${keyLabel} ${i + 1}`} value={k} onChange={(e) => set(i, 0, e.target.value)} />
          {options ? (
            <select aria-label={`${label}${valueLabel} ${i + 1}`} value={v} onChange={(e) => set(i, 1, e.target.value)}>
              {options.map((o) => <option key={o}>{o}</option>)}
            </select>
          ) : (
            <input aria-label={`${label}${valueLabel} ${i + 1}`} value={v} onChange={(e) => set(i, 1, e.target.value)} />
          )}
          <button type="button" aria-label={`删除${label} ${i + 1}`} onClick={() => onChange(rows.filter((_, n) => n !== i))}>删除</button>
        </div>
      ))}
      <button type="button" onClick={() => onChange([...rows, ["", options?.[0] ?? ""]])}>添加{label}</button>
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
  const [cats, setCats] = useDraft(doc.web_only, () => (doc.web_only.categories as string[]).join("\n"));
  const [rules, setRules] = useDraft(doc.web_only, () => (doc.web_only.rules as Rule[]).map((r) => [r.kind ?? "", r.tag ?? "", r.project ?? "", r.category]));
  const setRule = (i: number, j: number, v: string) => setRules(rules.map((r, n) => (n === i ? r.map((x, m) => (m === j ? v : x)) : r)));
  return (
    <Card title="web_only 分类" onSave={() => save("policy", {
      web_only: {
        categories: cats.split("\n").map((s) => s.trim()).filter(Boolean),
        rules: rules.map(([kind, tag, project, category]) => ({ kind, tag, project, category })),
      },
    })}>
      <label>类别（每行一个）<textarea value={cats} onChange={(e) => setCats(e.target.value)} /></label>
      {rules.map((r, i) => (
        <div key={i} role="group" aria-label={`匹配规则 ${i + 1}`} className="kv-row">
          {["kind", "tag", "项目", "类别"].map((n, j) => (
            <input key={n} aria-label={`规则${i + 1} ${n}`} placeholder={n} value={r[j]} onChange={(e) => setRule(i, j, e.target.value)} />
          ))}
          <button type="button" aria-label={`删除规则 ${i + 1}`} onClick={() => setRules(rules.filter((_, n) => n !== i))}>删除</button>
        </div>
      ))}
      <button type="button" onClick={() => setRules([...rules, ["", "", "", ""]])}>添加匹配规则</button>
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
        <PageHead>策略与人格</PageHead>
        <p role="alert">{error}</p>
        <button onClick={() => void load()}>重试</button>
      </main>
    );
  if (!docs) return <main><p role="status">加载中…</p></main>;
  const p = docs["policy"], al = docs["allowlists"], lim = docs["access-limits"];
  return (
    <main>
      <PageHead lede="保存的设置在下一次判定时生效；服务端拒绝时不会保存任何内容。">策略与人格</PageHead>
      <WebOnlyCard doc={p} save={save} />
      <DisturbCard doc={p} save={save} />
      <MaterialCard doc={p} save={save} />
      <LimitCard doc={lim} save={save} />
      <p className="sub">模型端点已移到 <Link to="/settings/models">设置 › 模型</Link>。</p>
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
  const [rows, setRows] = useDraft(doc.disturb, () => rowsOf(doc.disturb.routes));
  const [cap, setCap] = useDraft(doc.disturb, () => String(doc.disturb.daily_cap ?? 20));
  const [mutes, setMutes] = useDraft(doc.disturb, () => ((doc.disturb.mutes ?? []) as Mute[]).map((m) => [m.scope, m.id, toLocalInput(m.until)]));
  const setMute = (i: number, j: number, v: string) => setMutes(mutes.map((r, n) => (n === i ? r.map((x, m) => (m === j ? v : x)) : r)));
  return (
    <Card title="打扰策略" onSave={() => {
      const n = Number(cap);
      if (cap.trim() === "" || !Number.isInteger(n) || n < 0) return Promise.reject(new Error("日上限必须是不小于 0 的整数（0 表示不限）"));
      const list: Mute[] = mutes
        .filter(([, id]) => id.trim())
        .map(([scope, id, until]) => ({ scope, id: id.trim(), ...(until ? { until: new Date(until).toISOString() } : {}) }));
      return save("policy", { disturb: { ...doc.disturb, routes: toMap(rows, "打扰策略"), daily_cap: n, mutes: list } });
    }}>
      <p className="sub">键为投递类别，或「类别:优先级」（后者优先）；值为路由。</p>
      <Rows label="打扰路由" keyLabel="类别" valueLabel="路由" rows={rows} onChange={setRows} />
      <label>日上限（每个自然日经渠道推送的提醒条数，0 表示不限）
        <input type="number" min={0} aria-label="日上限" value={cap} onChange={(e) => setCap(e.target.value)} />
      </label>
      <p className="sub">静音：被静音的来源、项目或事项的提醒只留 Web 未读，决策仍进待办；到期时间为空表示一直静音。</p>
      {mutes.map((m, i) => (
        <div key={i} role="group" aria-label={`静音 ${i + 1}`} className="kv-row">
          <select aria-label={`静音${i + 1} 范围`} value={m[0]} onChange={(e) => setMute(i, 0, e.target.value)}>
            {MUTE_SCOPES.map((o) => <option key={o}>{o}</option>)}
          </select>
          <input aria-label={`静音${i + 1} id`} placeholder="id" value={m[1]} onChange={(e) => setMute(i, 1, e.target.value)} />
          <input type="datetime-local" aria-label={`静音${i + 1} 到期`} value={m[2]} onChange={(e) => setMute(i, 2, e.target.value)} />
          <button type="button" aria-label={`删除静音 ${i + 1}`} onClick={() => setMutes(mutes.filter((_, n) => n !== i))}>删除</button>
        </div>
      ))}
      <button type="button" onClick={() => setMutes([...mutes, [MUTE_SCOPES[0], "", ""]])}>添加静音</button>
    </Card>
  );
}

function MaterialCard({ doc, save }: { doc: Doc; save: Save }) {
  const [def, setDef] = useDraft(doc.material_write, () => doc.material_write.default as string);
  const [rows, setRows] = useDraft(doc.material_write, () => rowsOf(doc.material_write.kinds));
  return (
    <Card title="材料写入策略" onSave={() => save("policy", { material_write: { default: def, kinds: toMap(rows, "材料写入策略") } })}>
      <SelectField label="未登记 kind 的默认策略" value={def} onChange={setDef} options={WRITES} />
      <Rows label="材料 kind" keyLabel="kind" valueLabel="策略" rows={rows} onChange={setRows} options={WRITES} />
    </Card>
  );
}

function LimitCard({ doc, save }: { doc: Doc; save: Save }) {
  const [def, setDef] = useDraft(doc.permission_cap, () => doc.permission_cap.default as string);
  const [rows, setRows] = useDraft(doc.permission_cap, () => rowsOf(doc.permission_cap.by_harness));
  return (
    <Card title="权限上限" onSave={() => save("access-limits", { permission_cap: { default: def, by_harness: toMap(rows, "权限上限") } })}>
      <SelectField label="默认上限" value={def} onChange={setDef} options={LEVELS} />
      <Rows label="harness 上限" keyLabel="harness" valueLabel="上限" rows={rows} onChange={setRows} options={LEVELS} />
    </Card>
  );
}

function AllowlistCard({ doc, save }: { doc: Doc; save: Save }) {
  const [fwd, setFwd] = useDraft(doc.forward_allowlist, () => listRowsOf(doc.forward_allowlist));
  const [ans, setAns] = useDraft(doc.channel_answerers, () => listRowsOf(doc.channel_answerers));
  return (
    <Card title="白名单" onSave={() => save("allowlists", {
      forward_allowlist: toListMap(fwd, "转发白名单"),
      channel_answerers: toListMap(ans, "渠道回答人白名单"),
    })}>
      <p className="sub">两份白名单相互独立；多个值用逗号分隔。</p>
      <Rows label="转发白名单" keyLabel="来源" valueLabel="发送者" rows={fwd} onChange={setFwd} />
      <Rows label="渠道回答人" keyLabel="渠道" valueLabel="回答人" rows={ans} onChange={setAns} />
    </Card>
  );
}

function PersonaCard({ doc, save }: { doc: Doc; save: Save }) {
  const [text, setText] = useDraft(doc.persona, () => doc.persona as string);
  return (
    <Card title="人格" onSave={() => save("policy", { persona: text })}>
      <label>人格（约 400 token 上限，只影响表达方式，不放宽任何权限）<textarea value={text} onChange={(e) => setText(e.target.value)} /></label>
    </Card>
  );
}
