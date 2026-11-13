import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 GET /v1/judgments/stats、/judgments/spot-check、/settings/judgment（附录 B.2、B.4；主文档 §8.10）。
// 指标、分布、抽检与不确定带的校验都在服务端；本页只调用并如实展示服务端的回答。
interface Row { use: string; template: string; source?: string; backend: string; calls: number; decided: number; uncertain: number; errors: number; reused: number }
interface Rate { use: string; template: string; source?: string; escalation: number; undecided: number; acceptance: number; false_drop: number; reviewed: number }
interface Bucket { use: string; template: string; source?: string; low: number; high: number; count: number }
interface Stats { rows: Row[]; rates: Rate[]; histogram: Bucket[] }
interface Band { template: string; source?: string; low: number; high: number }
interface Settings { bands: Band[]; shadow: string[]; spot_check: { per_brief: number } }
interface Entry { judgment: string; event: string; source: string; subject?: string; template: string; judged_at: string; results: string[] }
interface Source { id: string }

const WINDOWS = ["7d", "30d", "90d"];
const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const pct = (v: number) => `${(v * 100).toFixed(1)}%`;
const key = (g: { use: string; template: string; source?: string }) => `${g.use}|${g.template}|${g.source ?? ""}`;
const RESULT_LABEL: Record<string, string> = { should_see: "该看", correct: "确实不用" };

interface BandDraft { template: string; source: string; low: string; high: string }
const toDraft = (b: Band): BandDraft => ({ template: b.template, source: b.source ?? "", low: String(b.low), high: String(b.high) });

export function JudgmentPage() {
  const { api } = useSession();
  const [since, setSince] = useState("30d");
  const [template, setTemplate] = useState("");
  const [source, setSource] = useState("");
  const [sources, setSources] = useState<Source[]>([]);
  const [stats, setStats] = useState<Stats | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [drafts, setDrafts] = useState<BandDraft[]>([]);
  const [newShadow, setNewShadow] = useState("");
  const [pool, setPool] = useState<Entry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const loadStats = useCallback(async () => {
    const q = new URLSearchParams({ since });
    if (template) q.set("template", template);
    if (source) q.set("source", source);
    setStats(await api.get<Stats>(`/judgments/stats?${q}`));
  }, [api, since, template, source]);
  const loadSettings = useCallback(async () => {
    const s = await api.get<Settings>("/settings/judgment");
    setSettings(s);
    setDrafts(s.bands.map(toDraft));
  }, [api]);
  const loadPool = useCallback(async () => setPool((await api.get<{ spot_check: Entry[] }>("/judgments/spot-check")).spot_check), [api]);
  const guard = useCallback(async (f: () => Promise<void>) => {
    try {
      await f();
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, []);

  useEffect(() => { void guard(loadStats); }, [guard, loadStats]);
  useEffect(() => {
    void guard(async () => {
      await Promise.all([loadSettings(), loadPool(), api.get<{ sources: Source[] }>("/sources").then((r) => setSources(r.sources))]);
    });
  }, [api, guard, loadSettings, loadPool]);

  const saveBands = () =>
    guard(async () => {
      setNotice(null);
      const bands = drafts.map((d) => ({ template: d.template.trim(), ...(d.source ? { source: d.source } : {}), low: Number(d.low), high: Number(d.high) }));
      await api.put("/settings/judgment", { bands });
      await loadSettings();
      setNotice("不确定带已保存");
    });
  const saveShadow = (shadow: string[]) =>
    guard(async () => {
      setNotice(null);
      await api.put("/settings/judgment", { shadow });
      await loadSettings();
    });
  const review = (e: Entry, result: string) =>
    guard(async () => {
      setNotice(null);
      await api.post(`/judgments/${encodeURIComponent(e.judgment)}/review`, { result });
      await Promise.all([loadPool(), loadStats()]);
      setNotice(`已记录抽检结论：${RESULT_LABEL[result] ?? result}`);
    });
  const setDraft = (i: number, patch: Partial<BandDraft>) => setDrafts((ds) => ds.map((d, j) => (j === i ? { ...d, ...patch } : d)));

  const templates = [...new Set([...(stats?.rates ?? []).map((r) => r.template), ...(settings?.bands ?? []).map((b) => b.template), ...(settings?.shadow ?? [])])].sort();
  const bandFor = (g: { template: string; source?: string }) =>
    settings?.bands.find((b) => b.template === g.template && (b.source ?? "") === (g.source ?? "")) ?? settings?.bands.find((b) => b.template === g.template && !b.source);

  return (
    <main>
      <PageHead lede="各级判断的指标与分数分布；不确定带、影子模式和抽检结论只能在这里由你调整。">判断与校准</PageHead>
      {error && <p role="alert" className="err">{error}</p>}
      {notice && <p role="status">{notice}</p>}

      <h2>指标</h2>
      <p>
        <label>窗口 <select aria-label="窗口" value={since} onChange={(e) => setSince(e.target.value)}>{WINDOWS.map((w) => <option key={w} value={w}>{w.replace("d", " 天")}</option>)}</select></label>{" "}
        <label>模板 <select aria-label="模板" value={template} onChange={(e) => setTemplate(e.target.value)}><option value="">全部</option>{templates.map((t) => <option key={t}>{t}</option>)}</select></label>{" "}
        <label>来源 <select aria-label="来源" value={source} onChange={(e) => setSource(e.target.value)}><option value="">全部</option>{sources.map((s) => <option key={s.id}>{s.id}</option>)}</select></label>
      </p>
      {!stats ? <p>加载中…</p> : stats.rates.length === 0 ? <p>这个窗口里还没有判断记录。</p> : (
        <table aria-label="指标">
          <thead><tr><th>用途</th><th>模板</th><th>来源</th><th>升级率</th><th>未定率</th><th>接受率</th><th>误丢率</th><th>已抽检</th></tr></thead>
          <tbody>
            {stats.rates.map((r) => (
              <tr key={key(r)}>
                <td>{r.use}</td><td>{r.template}</td><td>{r.source ?? "全部"}</td>
                <td>{pct(r.escalation)}</td><td>{pct(r.undecided)}</td><td>{pct(r.acceptance)}</td><td>{pct(r.false_drop)}</td><td>{r.reviewed}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {stats && stats.rows.length > 0 && (
        <table aria-label="各级调用">
          <thead><tr><th>用途</th><th>模板</th><th>来源</th><th>后端</th><th>调用</th><th>定案</th><th>未定</th><th>报错</th><th>复用</th></tr></thead>
          <tbody>
            {stats.rows.map((r) => (
              <tr key={key(r) + "|" + r.backend}>
                <td>{r.use}</td><td>{r.template}</td><td>{r.source ?? "全部"}</td><td>{r.backend}</td><td>{r.calls}</td><td>{r.decided}</td><td>{r.uncertain}</td><td>{r.errors}</td><td>{r.reused}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>分数分布</h2>
      {(stats?.rates ?? []).map((g) => {
        const buckets = (stats?.histogram ?? []).filter((b) => key(b) === key(g));
        if (buckets.length === 0) return null;
        const max = Math.max(...buckets.map((b) => b.count));
        const band = bandFor(g);
        return (
          <figure key={key(g)} aria-label={`分布 ${g.use} ${g.template} ${g.source ?? "全部"}`}>
            <figcaption>
              {g.use} · {g.template} · {g.source ?? "全部来源"}；当前不确定带：{band ? `${band.low}–${band.high}` : "沿用能力包声明"}
              ；低端误丢率 {pct(g.false_drop)}（已抽检 {g.reviewed}）；高端接受率 {pct(g.acceptance)}
            </figcaption>
            <table>
              <tbody>
                {buckets.map((b) => (
                  <tr key={b.low}>
                    <td>{b.low.toFixed(1)}–{b.high.toFixed(1)}</td>
                    <td><progress aria-label={`${b.low.toFixed(1)}–${b.high.toFixed(1)}`} max={max} value={b.count} /></td>
                    <td>{b.count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </figure>
        );
      })}

      <h2>不确定带</h2>
      <p>按模板设置；带来源的条目优先于不带来源的。保存是一次策略变更，会记入审计。</p>
      <table aria-label="不确定带">
        <thead><tr><th>模板</th><th>来源</th><th>下限</th><th>上限</th><th /></tr></thead>
        <tbody>
          {drafts.map((d, i) => (
            <tr key={i}>
              <td><input aria-label="带模板" value={d.template} onChange={(e) => setDraft(i, { template: e.target.value })} /></td>
              <td>
                <select aria-label="带来源" value={d.source} onChange={(e) => setDraft(i, { source: e.target.value })}>
                  <option value="">全部来源</option>{sources.map((s) => <option key={s.id}>{s.id}</option>)}
                </select>
              </td>
              <td><input aria-label="下限" type="number" step="0.01" value={d.low} onChange={(e) => setDraft(i, { low: e.target.value })} /></td>
              <td><input aria-label="上限" type="number" step="0.01" value={d.high} onChange={(e) => setDraft(i, { high: e.target.value })} /></td>
              <td><button className="quiet" onClick={() => setDrafts((ds) => ds.filter((_, j) => j !== i))}>删除</button></td>
            </tr>
          ))}
        </tbody>
      </table>
      <p>
        <button onClick={() => setDrafts((ds) => [...ds, { template: template || "", source: "", low: "", high: "" }])}>添加一条</button>{" "}
        <button onClick={saveBands}>保存不确定带</button>
      </p>

      <h2>影子模式</h2>
      <p>开启后该模板的分类端点只记分、不定案，处置仍按没有它的路径走。</p>
      <ul>
        {[...new Set([...templates, ...(settings?.shadow ?? [])])].map((t) => {
          const on = settings?.shadow.includes(t) ?? false;
          return (
            <li key={t}>
              <label>
                <input type="checkbox" checked={on} onChange={() => saveShadow(on ? settings!.shadow.filter((x) => x !== t) : [...(settings?.shadow ?? []), t])} /> {t}
              </label>
            </li>
          );
        })}
      </ul>
      <p>
        <input aria-label="影子模板" placeholder="模板名" value={newShadow} onChange={(e) => setNewShadow(e.target.value)} />{" "}
        <button disabled={!newShadow.trim()} onClick={() => { const t = newShadow.trim(); setNewShadow(""); void saveShadow([...new Set([...(settings?.shadow ?? []), t])]); }}>开启影子</button>
      </p>

      <h2>抽检</h2>
      <p>下面是被初筛忽略的事件。点“该看”会把事件转成建议并计入误丢；“确实不用”只记结论。</p>
      {pool && pool.length === 0 && <p>现在没有待抽检的条目。</p>}
      <ul aria-label="待抽检">
        {(pool ?? []).map((e) => (
          <li key={e.judgment}>
            <strong>{e.subject || e.event}</strong>（{e.source} · {e.template} · {e.judged_at}）{" "}
            {e.results.map((r) => <button key={r} onClick={() => review(e, r)}>{RESULT_LABEL[r] ?? r}</button>)}
          </li>
        ))}
      </ul>
    </main>
  );
}
