import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, lang, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 /v1/settings/model-endpoints 与它的两条探测路由（附录 B.2；设计 §4.1、§14.14、§15.5）。
// 校验、规整、默认值都在服务端；本页只搬运编辑结果，原样显示拒绝原因，被拒绝时草稿保留。
// 密钥只写不读：读取只有 api_key_set 与末 4 位；回传占位 *** 表示沿用原密钥。
const MASK = "***";
// 判断端点按协议分适配器（§8.8）：system-one 是原生分类模型服务的判断接口，新配时默认；probability-judgment/1 给自建分类服务。
// chat-logprobs 待 SPIKE-LOGPROB（§21.3）结论后再开放，此前不提供。
const SYSTEM_ONE = "system-one";
const PJ = "probability-judgment/1";
const JUDGE_PROTOCOLS = [SYSTEM_ONE, PJ];
// 新配时的预填值；换协议时，仍是上一个协议预填值的字段换成新协议的预填值，本人改过的不动。
// system-one 的首次调用可能要约 10 秒（建连），超时预填 30000；只影响新配，已存配置照旧。
const PRESETS: Record<string, { url: string; model: string; timeout_ms: string }> = {
  [SYSTEM_ONE]: { url: "https://api.typesafe.ai/v1", model: "jev-latest", timeout_ms: "30000" },
  [PJ]: { url: "", model: "", timeout_ms: "10000" },
};
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
  if (v.trim() === "" || !Number.isInteger(n) || n <= 0) throw new Error(strings[lang()].posInt(what));
  return n;
}

type Draft = Record<string, string>;
const JUDGE_NEW: Draft = { ...PRESETS[SYSTEM_ONE], protocol: SYSTEM_ONE, locality: "", max_batch: "32", max_material_chars: "8000" };

function draftOf(kind: Kind, e: Endpoint | null): Draft {
  if (!e) return kind === "judge" ? { ...JUDGE_NEW } : { url: "", model: "", locality: "" };
  const d: Draft = { url: e.url ?? "", model: e.model ?? "", locality: e.locality ?? "" };
  if (kind === "judge")
    Object.assign(d, { protocol: e.protocol || PJ, timeout_ms: String(e.timeout_ms ?? 10000), max_batch: String(e.max_batch ?? 32), max_material_chars: String(e.max_material_chars ?? 8000) });
  return d;
}

export function SettingsModelsPage() {
  const t = useT(strings);
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

  if (error)
    return (
      <main>
        <PageHead lede={t.lede}>{t.title}</PageHead>
        <p role="alert">{error}</p>
        <button onClick={() => void load()}>{t.retry}</button>
      </main>
    );
  if (!doc) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {!doc.generate_endpoint && !doc.judge_endpoint && (
        <section className="panel" aria-label={t.emptyLabel}>
          <p>{t.empty1}</p>
          <p className="sub">{t.empty2}</p>
        </section>
      )}
      <EndpointCard kind="generate" doc={doc.generate_endpoint} unsetExternal={unsetExternal} save={(e) => save({ generate_endpoint: e as Endpoint | null })} />
      <EndpointCard kind="judge" doc={doc.judge_endpoint} unsetExternal={unsetExternal} save={(e) => save({ judge_endpoint: e as Endpoint | null })} />
      <ScreeningTimeoutCard value={doc.screening_timeout_ms} save={(n) => save({ screening_timeout_ms: n })} />
    </main>
  );
}

function SaveLine({ busy, status, onSave, label }: { busy: boolean; status: { ok: boolean; text: string } | null; onSave: () => void; label: string }) {
  const t = useT(strings);
  return (
    <>
      <div className="row">
        <button type="button" className="primary" disabled={busy} onClick={onSave} aria-label={label}>{t.save}</button>
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
      setStatus({ ok: true, text: strings[lang()].saved(await run()) });
    } catch (e) {
      setStatus({ ok: false, text: msg(e) });
    } finally {
      setBusy(false);
    }
  };
  return { busy, status, save: () => void go() };
}

function EndpointCard({ kind, doc, unsetExternal, save }: { kind: Kind; doc: Endpoint | null; unsetExternal: number | null; save: (e: object | null) => Promise<number> }) {
  const t = useT(strings);
  const { api } = useSession();
  const title = kind === "generate" ? t.generate : t.judge;
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
  const setProtocol = (p: string) =>
    setD((x) => {
      const from = PRESETS[x.protocol], to = PRESETS[p];
      const next: Draft = { ...x, protocol: p };
      if (from && to) for (const k of ["url", "model", "timeout_ms"] as const) if (x[k] === from[k]) next[k] = to[k];
      return next;
    });
  const so = kind === "judge" && d.protocol === SYSTEM_ONE;
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
    return { ...base, protocol: d.protocol, timeout_ms: posInt(d.timeout_ms, t.timeoutMs), max_batch: posInt(d.max_batch, t.maxBatch), max_material_chars: posInt(d.max_material_chars, t.maxChars) };
  };
  const saver = useSaver(async () => {
    if (!on) return save(null);
    if (!d.locality) throw new Error(t.needWhere);
    return save(config());
  });
  // 生成端点与 system-one 判断接口有模型列表；probability-judgment/1 没有。
  const canList = kind === "generate" || so;

  const fetchModels = async () => {
    setProbing("models");
    setListNote(null);
    setProbeErr(null);
    try {
      const r = await api.post<ModelList>("/settings/model-endpoints/models", { endpoint: kind, url: d.url.trim(), api_key: apiKey, protocol: d.protocol });
      if (r.ok && r.models) {
        setModels(r.models);
        setManual(r.models.length === 0);
        setListNote({ ok: true, text: r.models.length ? t.got(r.models.length, !!r.truncated) : t.gotNone });
      } else {
        setModels(null);
        setManual(true);
        const why = t.reasons[r.reason ?? ""] ?? r.reason ?? "";
        setListNote({ ok: false, text: t.listFailed(why, r.status) });
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
          {!d.model && <option value="">{t.pickModel}</option>}
          {options.map((m) => <option key={m} value={m}>{m}</option>)}
          <option value={MANUAL}>{t.manual}</option>
        </select>
      </label>
    ) : (
      <label>
        {label}
        <input value={d.model} onChange={(e) => set("model", e.target.value)} placeholder={kind === "judge" && !canList ? t.noList : undefined} />
      </label>
    );
  };

  return (
    <section aria-label={title} className="panel stack">
      <h2>{title}</h2>
      <p className="sub">
        {kind === "generate" ? t.generateHint : t.judgeHint}
      </p>
      <label>
        <input type="checkbox" checked={on} aria-label={t.configure(title)} onChange={(e) => setOn(e.target.checked)} />
        {t.configure(title)}
      </label>
      {on && (
        <>
          <fieldset className="choice">
            <legend>{t.where}</legend>
            <label>
              <input type="radio" name={kind + "-locality"} value="local" checked={d.locality === "local"} onChange={() => set("locality", "local")} />
              <span><b>{t.local}</b><small>{t.localHint}</small></span>
            </label>
            <label>
              <input type="radio" name={kind + "-locality"} value="external" checked={d.locality === "external"} onChange={() => set("locality", "external")} />
              <span><b>{t.external}</b><small>{t.externalHint}</small></span>
            </label>
          </fieldset>
          {d.locality === "external" && unsetExternal != null && unsetExternal > 0 && (
            <p className="note">
              {t.unsetPre(unsetExternal)}
              <Link to="/sources">{t.toSources}</Link>
            </p>
          )}
          {kind === "judge" && (
            <>
              <label>
                {t.protocol}
                <select value={d.protocol} onChange={(e) => setProtocol(e.target.value)}>
                  {JUDGE_PROTOCOLS.map((p) => <option key={p} value={p}>{t.protocolNames[p] ?? p}</option>)}
                </select>
              </label>
              {t.protocolHints[d.protocol] && <small className="sub">{t.protocolHints[d.protocol]}</small>}
            </>
          )}
          <label>
            {so ? t.urlSystemOne : kind === "judge" ? t.urlJudge : t.url}
            <input value={d.url} onChange={(e) => setURL(e.target.value)} placeholder={so ? PRESETS[SYSTEM_ONE].url : kind === "judge" ? "http://127.0.0.1:8090/judge" : "https://llm.example.com/v1"} />
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
            {modelField(kind === "judge" && !so ? t.modelJudge : t.model)}
            {canList && (
              <button type="button" disabled={!!probing || !d.url.trim()} onClick={() => void fetchModels()}>
                {probing === "models" ? t.fetching(elapsed) : t.fetch}
              </button>
            )}
          </div>
          {models && !manual && <button type="button" className="link" onClick={() => setManual(true)}>{t.manualLink}</button>}
          {listNote && <p className={listNote.ok ? "sub" : "note"} role="status">{listNote.text}</p>}
          {kind === "judge" && (
            <details>
              <summary>{t.advanced}</summary>
              <label>{t.timeoutMs}<input type="number" value={d.timeout_ms} onChange={(e) => set("timeout_ms", e.target.value)} /></label>
              <label>{t.maxBatch}<input type="number" value={d.max_batch} onChange={(e) => set("max_batch", e.target.value)} /></label>
              <label>{t.maxChars}<input type="number" value={d.max_material_chars} onChange={(e) => set("max_material_chars", e.target.value)} /></label>
            </details>
          )}
          <div className="row">
            <button type="button" disabled={!!probing || !d.url.trim() || ((kind === "generate" || so) && !d.model.trim())} onClick={() => void test().catch((e) => setProbeErr(msg(e)))}>
              {probing === "test" ? t.testing(elapsed) : t.test}
            </button>
          </div>
          {probeErr && <p role="alert">{probeErr}</p>}
          {probe && <ProbeView r={probe} />}
        </>
      )}
      <SaveLine busy={saver.busy} status={saver.status} onSave={saver.save} label={t.saveCard(title)} />
    </section>
  );
}

function KeyField({ stored, mode, value, onChange, onMode, urlChanged }: {
  stored: string | null; mode: "keep" | "edit"; value: string; onChange: (v: string) => void; onMode: (m: "keep" | "edit") => void; urlChanged: boolean;
}) {
  const t = useT(strings);
  if (stored != null && mode === "keep")
    return (
      <div role="group" aria-label={t.apiKey} className="row">
        <span className="sub" aria-hidden="true">{t.apiKey}</span>
        <span className="badge plain">
          {t.keySet}
          {stored && ` ····${stored}`}
        </span>
        <button type="button" onClick={() => onMode("edit")}>{t.change}</button>
      </div>
    );
  return (
    <>
      <label>
        {t.apiKey}
        <input type="password" autoComplete="off" value={value} onChange={(e) => onChange(e.target.value)} placeholder={stored != null ? t.clearHint : t.noneHint} />
      </label>
      {urlChanged ? (
        <small className="sub">{t.originChanged}</small>
      ) : (
        stored != null && <button type="button" className="link" onClick={() => onMode("keep")}>{t.keepKey}</button>
      )}
      <small className="sub">{t.writeOnly}</small>
    </>
  );
}

function ProbeView({ r }: { r: ProbeResult }) {
  const t = useT(strings);
  const mark = { ok: "✓", warning: "!", error: "✗" }[r.status];
  const text = (r.code && t.codes[r.code]) || r.why;
  const facts: ReactNode[] = [];
  if (r.data?.elapsed_ms != null) facts.push(t.elapsed(r.data.elapsed_ms));
  if (r.data?.status) facts.push(`HTTP ${r.data.status}`);
  if (r.data?.model) facts.push(t.modelFact(r.data.model));
  if (r.data?.probability_source) facts.push(t.source(r.data.probability_source));
  return (
    <div role="status" aria-label={t.result} className={"probe " + r.status}>
      <p><b>{mark} {text}</b>{facts.length > 0 && <span className="sub"> · {facts.join(" · ")}</span>}</p>
      <p className="sub">
        {r.why}
        {r.next ? t.paren(r.next) : ""}
      </p>
    </div>
  );
}

function ScreeningTimeoutCard({ value, save }: { value: number; save: (n: number) => Promise<number> }) {
  const t = useT(strings);
  const [v, setV] = useState(String(value));
  useEffect(() => setV(String(value)), [value]);
  const saver = useSaver(() => save(posInt(v, t.screening)));
  return (
    <section aria-label={t.screening} className="panel stack">
      <h2>{t.screening}</h2>
      <p className="sub">{t.screeningHint}</p>
      <label>{t.screeningMs}<input type="number" value={v} onChange={(e) => setV(e.target.value)} /></label>
      <SaveLine busy={saver.busy} status={saver.status} onSave={saver.save} label={t.saveCard(t.screening)} />
    </section>
  );
}
