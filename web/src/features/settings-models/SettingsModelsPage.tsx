import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 /v1/settings/model-endpoints 与它的两条探测路由（附录 B.2；设计 §4.1、§14.14、§15.5）。
// 校验、规整、默认值都在服务端；本页只搬运编辑结果，原样显示拒绝原因，被拒绝时草稿保留。
// 密钥只写不读：读取只有 api_key_set 与末 4 位；回传占位 *** 表示沿用原密钥。
const MASK = "***";
const JUDGE_PROTOCOLS = ["probability-judgment/1"]; // chat-logprobs 待 SPIKE-LOGPROB（§21.3）结论后再开放，此前不提供。
const MANUAL = "\u0000manual";

type Kind = "generate" | "judge";
type Locality = "local" | "external" | "";
interface Endpoint {
  url: string;
  api_key_set: boolean;
  api_key_hint?: string;
  model?: string;
  locality: Locality;
  protocol?: string;
  timeout_ms?: number;
  max_batch?: number;
  max_material_chars?: number;
}
interface Doc {
  version: number;
  judge_endpoint: Endpoint | null;
  generate_endpoint: Endpoint | null;
  screening_timeout_ms: number;
}
interface ProbeResult {
  status: "ok" | "warning" | "error";
  code?: string;
  why: string;
  next?: string;
  data?: { elapsed_ms?: number; model?: string; probability_source?: string; status?: number };
}
interface ModelList {
  ok: boolean;
  models?: string[];
  truncated?: boolean;
  reason?: string;
  status?: number;
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

// 探测结论的说明（按服务端的 code）；查不到时只显示服务端原文。
const CODE_TEXT: Record<string, string> = {
  ok: "连通，鉴权通过，示例请求得到了回答",
  unauthorized: "鉴权失败：端点拒绝了这个密钥",
  protocol_error: "响应不符合所选协议",
  self_reported: "端点给出的概率是自报的，不能作为判断端点",
  invalid_request: "端点认为示例请求无效：检查协议与模型名",
  no_answer: "端点没有回答示例题",
  timeout: "端点没有在时限内回答",
  http_status: "端点返回了错误状态",
  schema_mismatch: "能用，但输出不符合示例的结构化格式，卡片文案可能退化",
  bad_response: "响应不是对话补全格式",
  unreachable: "连不上端点：检查地址与网络",
};
const REASON_TEXT: Record<string, string> = {
  unauthorized: "鉴权失败：端点拒绝了这个密钥",
  not_found: "这个端点不提供模型列表",
  http_status: "端点返回了错误状态",
  timeout: "端点没有在时限内回答",
  bad_response: "端点返回的不是模型列表",
  unreachable: "连不上端点：检查地址与网络",
};

function origin(u: string) {
  try {
    const x = new URL(u);
    return x.protocol + "//" + x.host;
  } catch {
    return "";
  }
}

function posInt(v: string, what: string) {
  const n = Number(v);
  if (v.trim() === "" || !Number.isInteger(n) || n <= 0) throw new Error(`${what}必须是正整数`);
  return n;
}

type Draft = Record<string, string>;
const JUDGE_NEW: Draft = { url: "", protocol: JUDGE_PROTOCOLS[0], model: "", locality: "", timeout_ms: "10000", max_batch: "32", max_material_chars: "8000" };

function draftOf(kind: Kind, e: Endpoint | null): Draft {
  if (!e) return kind === "judge" ? { ...JUDGE_NEW } : { url: "", model: "", locality: "" };
  const d: Draft = { url: e.url ?? "", model: e.model ?? "", locality: e.locality ?? "" };
  if (kind === "judge")
    Object.assign(d, { protocol: e.protocol || JUDGE_PROTOCOLS[0], timeout_ms: String(e.timeout_ms ?? 10000), max_batch: String(e.max_batch ?? 32), max_material_chars: String(e.max_material_chars ?? 8000) });
  return d;
}

export function SettingsModelsPage() {
  const { api } = useSession();
  const [doc, setDoc] = useState<Doc | null>(null);
  const [unsetExternal, setUnsetExternal] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      setDoc(await api.get<Doc>("/settings/model-endpoints"));
    } catch (e) {
      setDoc(null);
      setError(msg(e));
    }
    // 外发范围只用来提示，取不到就不提示。
    api.get<{ sources: { outbound?: { external: string | null } }[] }>("/sources").then(
      (r) => setUnsetExternal(r.sources.filter((s) => !s.outbound?.external).length),
      () => setUnsetExternal(null),
    );
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  const save = async (body: Partial<Doc>) => {
    const next = await api.put<Doc>("/settings/model-endpoints", body);
    setDoc(next);
    return next.version;
  };

  const lede = "生成端点起草建议、写决策卡片和摘要；判断端点给是非题打分，比生成端点更快、更省。两个都可以不配，工作台和规则照常可用。";
  if (error)
    return (
      <main>
        <PageHead lede={lede}>模型</PageHead>
        <p role="alert">{error}</p>
        <button onClick={() => void load()}>重试</button>
      </main>
    );
  if (!doc) return <main><p role="status">加载中…</p></main>;
  return (
    <main>
      <PageHead lede={lede}>模型</PageHead>
      {!doc.generate_endpoint && !doc.judge_endpoint && (
        <section className="panel" aria-label="还没有配置模型端点">
          <p>还没有配置模型端点。不配也能用：工作台、规则和提醒照常工作，规则判不了的事件留在来源页的积压里。</p>
          <p className="sub">配置生成端点后，APA 开始初筛来源事件、起草建议并撰写决策卡片；判断端点是可选的加速。</p>
        </section>
      )}
      <EndpointCard kind="generate" doc={doc.generate_endpoint} unsetExternal={unsetExternal} save={(e) => save({ generate_endpoint: e as Endpoint | null })} />
      <EndpointCard kind="judge" doc={doc.judge_endpoint} unsetExternal={unsetExternal} save={(e) => save({ judge_endpoint: e as Endpoint | null })} />
      <ScreeningTimeoutCard value={doc.screening_timeout_ms} save={(n) => save({ screening_timeout_ms: n })} />
    </main>
  );
}

function SaveLine({ busy, status, onSave, label }: { busy: boolean; status: { ok: boolean; text: string } | null; onSave: () => void; label: string }) {
  return (
    <>
      <div className="row">
        <button type="button" className="primary" disabled={busy} onClick={onSave} aria-label={label}>保存</button>
      </div>
      {status && <p role={status.ok ? "status" : "alert"}>{status.text}</p>}
    </>
  );
}

function useSaver(run: () => Promise<number>) {
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const go = async () => {
    setBusy(true);
    setStatus(null);
    try {
      setStatus({ ok: true, text: `已保存（版本 ${await run()}）` });
    } catch (e) {
      setStatus({ ok: false, text: msg(e) });
    } finally {
      setBusy(false);
    }
  };
  return { busy, status, save: () => void go() };
}

function EndpointCard({ kind, doc, unsetExternal, save }: { kind: Kind; doc: Endpoint | null; unsetExternal: number | null; save: (e: object | null) => Promise<number> }) {
  const { api } = useSession();
  const title = kind === "generate" ? "生成端点" : "判断端点";
  const sig = JSON.stringify(doc);
  const [on, setOn] = useState(doc != null);
  const [d, setD] = useState<Draft>(() => draftOf(kind, doc));
  // 密钥：keep 沿用已存密钥（发 ***）；edit 发输入框的值（空表示不带密钥）。
  const [keyMode, setKeyMode] = useState<"keep" | "edit">(doc?.api_key_set ? "keep" : "edit");
  const [key, setKey] = useState("");
  const [models, setModels] = useState<string[] | null>(null);
  const [manual, setManual] = useState(true);
  const [listNote, setListNote] = useState<{ ok: boolean; text: string } | null>(null);
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const [probing, setProbing] = useState<"" | "models" | "test">("");
  const [probeErr, setProbeErr] = useState<string | null>(null);
  const [elapsed, setElapsed] = useState(0);

  // 服务端内容变化（保存后、重新加载）时重置草稿。
  useEffect(() => {
    setOn(doc != null);
    setD(draftOf(kind, doc));
    setKeyMode(doc?.api_key_set ? "keep" : "edit");
    setKey("");
  }, [sig]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!probing) return;
    setElapsed(0);
    const t = setInterval(() => setElapsed((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, [probing]);

  const set = (k: string, v: string) => setD((x) => ({ ...x, [k]: v }));
  const setURL = (v: string) => {
    set("url", v);
    // 已存密钥只发往原来的主机：地址换了源就要求重新输入（服务端同样拒绝）。
    if (keyMode === "keep" && doc && origin(v) !== origin(doc.url)) {
      setKeyMode("edit");
      setKey("");
    }
  };
  const apiKey = keyMode === "keep" ? MASK : key;
  const config = () => {
    const base = { url: d.url.trim(), api_key: apiKey, model: d.model.trim(), locality: d.locality };
    if (kind === "generate") return base;
    return { ...base, protocol: d.protocol, timeout_ms: posInt(d.timeout_ms, "超时（毫秒）"), max_batch: posInt(d.max_batch, "批量上限"), max_material_chars: posInt(d.max_material_chars, "资料字数上限") };
  };
  const saver = useSaver(async () => {
    if (!on) return save(null);
    if (!d.locality) throw new Error("请选择端点在哪里：配置这个端点时必选");
    return save(config());
  });
  const canList = kind === "generate" || d.protocol === "chat-logprobs";

  const fetchModels = async () => {
    setProbing("models");
    setListNote(null);
    setProbeErr(null);
    try {
      const r = await api.post<ModelList>("/settings/model-endpoints/models", { endpoint: kind, url: d.url.trim(), api_key: apiKey, protocol: d.protocol });
      if (r.ok && r.models) {
        setModels(r.models);
        setManual(r.models.length === 0);
        setListNote({ ok: true, text: r.models.length ? `已从端点取到 ${r.models.length} 个模型${r.truncated ? "（只列出前 500 个）" : ""}。` : "端点没有列出任何模型，请手动填写。" });
      } else {
        setModels(null);
        setManual(true);
        const why = REASON_TEXT[r.reason ?? ""] ?? r.reason ?? "";
        setListNote({ ok: false, text: `${why}${r.status ? `（HTTP ${r.status}）` : ""}。可以手动填写模型名。` });
      }
    } catch (e) {
      setProbeErr(msg(e));
    } finally {
      setProbing("");
    }
  };

  const test = async () => {
    setProbing("test");
    setProbe(null);
    setProbeErr(null);
    try {
      setProbe(await api.post<ProbeResult>("/settings/model-endpoints/test", { endpoint: kind, config: config() }));
    } catch (e) {
      setProbeErr(msg(e));
    } finally {
      setProbing("");
    }
  };

  const modelField = (label: string) => {
    const options = models ? (d.model && !models.includes(d.model) ? [d.model, ...models] : models) : [];
    return models && !manual ? (
      <label>
        {label}
        <select value={d.model} onChange={(e) => (e.target.value === MANUAL ? setManual(true) : set("model", e.target.value))}>
          {!d.model && <option value="">（请选择）</option>}
          {options.map((m) => <option key={m} value={m}>{m}</option>)}
          <option value={MANUAL}>手动填写…</option>
        </select>
      </label>
    ) : (
      <label>
        {label}
        <input value={d.model} onChange={(e) => set("model", e.target.value)} placeholder={kind === "judge" && !canList ? "此协议不提供模型列表" : undefined} />
      </label>
    );
  };

  return (
    <section aria-label={title} className="panel stack">
      <h2>{title}</h2>
      <p className="sub">
        {kind === "generate"
          ? "对话补全兼容协议。用于起草建议、决策卡片文案和摘要；没有判断端点时也由它初筛来源事件。"
          : "概率判断协议，给“值不值得看”这类是非题打分。不配时，是非题交给生成端点。"}
      </p>
      <label>
        <input type="checkbox" checked={on} aria-label={`配置${title}`} onChange={(e) => setOn(e.target.checked)} />
        配置{title}
      </label>
      {on && (
        <>
          <fieldset className="choice">
            <legend>端点在哪里（配置这个端点时必选）</legend>
            <label>
              <input type="radio" name={kind + "-locality"} value="local" checked={d.locality === "local"} onChange={() => set("locality", "local")} />
              <span><b>本机或我自己控制的主机</b><small>local：来源内容按各来源的本机外发范围送出，缺省为全文</small></span>
            </label>
            <label>
              <input type="radio" name={kind + "-locality"} value="external" checked={d.locality === "external"} onChange={() => set("locality", "external")} />
              <span><b>外部服务</b><small>external：来源内容按各来源的外部外发范围裁剪后才送出</small></span>
            </label>
          </fieldset>
          {d.locality === "external" && unsetExternal != null && unsetExternal > 0 && (
            <p className="note">
              有 {unsetExternal} 个来源还没设置外部外发范围，它们的内容不会送到这个端点。<Link to="/sources">去来源页设置</Link>
            </p>
          )}
          {kind === "judge" && (
            <label>
              协议
              <select value={d.protocol} onChange={(e) => set("protocol", e.target.value)}>
                {JUDGE_PROTOCOLS.map((p) => <option key={p}>{p}</option>)}
              </select>
            </label>
          )}
          <label>
            {kind === "judge" ? "端点 URL（完整路径，不追加任何后缀）" : "端点 URL"}
            <input value={d.url} onChange={(e) => setURL(e.target.value)} placeholder={kind === "judge" ? "http://127.0.0.1:8090/judge" : "https://llm.example.com/v1"} />
          </label>
          <KeyField
            stored={doc?.api_key_set ? doc.api_key_hint || "" : null}
            mode={keyMode}
            value={key}
            onChange={setKey}
            onMode={(m) => { setKeyMode(m); setKey(""); }}
            urlChanged={!!doc?.api_key_set && keyMode === "edit" && origin(d.url) !== origin(doc.url)}
          />
          <div className="row end">
            {modelField(kind === "judge" ? "模型（可留空，由端点自报）" : "模型")}
            {canList && (
              <button type="button" disabled={!!probing || !d.url.trim()} onClick={() => void fetchModels()}>
                {probing === "models" ? `获取中… ${elapsed} 秒` : "获取模型"}
              </button>
            )}
          </div>
          {models && !manual && <button type="button" className="link" onClick={() => setManual(true)}>手动填写模型名</button>}
          {listNote && <p className={listNote.ok ? "sub" : "note"} role="status">{listNote.text}</p>}
          {kind === "judge" && (
            <details>
              <summary>高级：超时、批量与资料长度</summary>
              <label>超时（毫秒）<input type="number" value={d.timeout_ms} onChange={(e) => set("timeout_ms", e.target.value)} /></label>
              <label>批量上限<input type="number" value={d.max_batch} onChange={(e) => set("max_batch", e.target.value)} /></label>
              <label>资料字数上限<input type="number" value={d.max_material_chars} onChange={(e) => set("max_material_chars", e.target.value)} /></label>
            </details>
          )}
          <div className="row">
            <button type="button" disabled={!!probing || !d.url.trim() || (kind === "generate" && !d.model.trim())} onClick={() => void test().catch((e) => setProbeErr(msg(e)))}>
              {probing === "test" ? `测试中… ${elapsed} 秒` : "测试连接"}
            </button>
          </div>
          {probeErr && <p role="alert">{probeErr}</p>}
          {probe && <ProbeView r={probe} />}
        </>
      )}
      <SaveLine busy={saver.busy} status={saver.status} onSave={saver.save} label={`保存${title}`} />
    </section>
  );
}

function KeyField({ stored, mode, value, onChange, onMode, urlChanged }: {
  stored: string | null; mode: "keep" | "edit"; value: string; onChange: (v: string) => void; onMode: (m: "keep" | "edit") => void; urlChanged: boolean;
}) {
  if (stored != null && mode === "keep")
    return (
      <div role="group" aria-label="API 密钥" className="row">
        <span className="sub" aria-hidden="true">API 密钥</span>
        <span className="badge plain">已设置{stored && ` ····${stored}`}</span>
        <button type="button" onClick={() => onMode("edit")}>更换</button>
      </div>
    );
  return (
    <>
      <label>
        API 密钥
        <input type="password" autoComplete="off" value={value} onChange={(e) => onChange(e.target.value)} placeholder={stored != null ? "留空表示清除已存的密钥" : "没有可留空"} />
      </label>
      {urlChanged ? (
        <small className="sub">端点地址换了主机，已存的密钥不会发到新地址，请重新输入。</small>
      ) : (
        stored != null && <button type="button" className="link" onClick={() => onMode("keep")}>沿用已存的密钥</button>
      )}
      <small className="sub">密钥只写入，保存后不再显示。</small>
    </>
  );
}

function ProbeView({ r }: { r: ProbeResult }) {
  const mark = { ok: "✓", warning: "!", error: "✗" }[r.status];
  const text = (r.code && CODE_TEXT[r.code]) || r.why;
  const facts: ReactNode[] = [];
  if (r.data?.elapsed_ms != null) facts.push(`耗时 ${r.data.elapsed_ms} ms`);
  if (r.data?.status) facts.push(`HTTP ${r.data.status}`);
  if (r.data?.model) facts.push(`模型 ${r.data.model}`);
  if (r.data?.probability_source) facts.push(`概率来源 ${r.data.probability_source}`);
  return (
    <div role="status" aria-label="测试结果" className={"probe " + r.status}>
      <p><b>{mark} {text}</b>{facts.length > 0 && <span className="sub"> · {facts.join(" · ")}</span>}</p>
      <p className="sub">{r.why}{r.next ? `（${r.next}）` : ""}</p>
    </div>
  );
}

function ScreeningTimeoutCard({ value, save }: { value: number; save: (n: number) => Promise<number> }) {
  const [t, setT] = useState(String(value));
  useEffect(() => setT(String(value)), [value]);
  const saver = useSaver(() => save(posInt(t, "筛选超时")));
  return (
    <section aria-label="筛选超时" className="panel stack">
      <h2>筛选超时</h2>
      <p className="sub">web_only 初筛等待模型的最长时间；超时按规则的结果继续。</p>
      <label>筛选超时（毫秒）<input type="number" value={t} onChange={(e) => setT(e.target.value)} /></label>
      <SaveLine busy={saver.busy} status={saver.status} onSave={saver.save} label="保存筛选超时" />
    </section>
  );
}
