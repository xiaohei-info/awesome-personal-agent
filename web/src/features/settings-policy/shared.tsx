import { useCallback, useEffect, useState, type KeyboardEvent, type ReactNode } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, lang, useT } from "../../app/i18n";
import { shared } from "./strings";

// 「我来拍板」「提醒与打扰」「后台权限」「通用 › 助手的语气」共用：形状对应 /v1/settings/{policy,allowlists,access-limits}（附录 B.2）。
// 校验、规整、人格上限（约 400 token）全部由服务端判定，页面只搬运编辑结果并原样显示拒绝原因；被拒绝时什么都没保存，草稿保留。
// PUT 按段替换：每张卡片只发自己的段，且与改版前的请求体逐字段一致（settings-payload.test.tsx 守住）。
export type PolicyRoute = "policy" | "allowlists" | "access-limits";
export type Doc = Record<string, any> & { version: number };
export type Save = (route: PolicyRoute, body: unknown) => Promise<number>;

/** 读一组设置路由；保存后用服务端规整过的结果替换对应路由的文档。 */
export function usePolicyDocs(routes: readonly PolicyRoute[]) {
  const { api } = useSession();
  const [docs, setDocs] = useState<Partial<Record<PolicyRoute, Doc>> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const key = routes.join(",");
  const load = useCallback(async () => {
    setError(null);
    try {
      const all = await Promise.all(routes.map((r) => api.get<Doc>("/settings/" + r)));
      setDocs(Object.fromEntries(routes.map((r, i) => [r, all[i]])));
    } catch (e) {
      setDocs(null);
      setError(msg(e));
    }
  }, [api, key]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    void load();
  }, [load]);
  const save: Save = async (route, body) => {
    const next = await api.put<Doc>("/settings/" + route, body);
    setDocs((d) => ({ ...d, [route]: next }));
    return next.version;
  };
  return { docs, error, load, save };
}

/** 页面骨架：出错时给原因和重试，加载中给状态。 */
export function PolicyPage({ title, lede, error, ready, onRetry, children }: {
  title: string; lede?: string; error: string | null; ready: boolean; onRetry: () => void; children?: ReactNode;
}) {
  const t = useT(shared);
  if (error)
    return (
      <main>
        <PageHead>{title}</PageHead>
        <p role="alert">{error}</p>
        <button onClick={onRetry}>{t.retry}</button>
      </main>
    );
  if (!ready) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <PageHead lede={lede}>{title}</PageHead>
      {children}
    </main>
  );
}

/** 一张可单独保存的卡片：标题即说明，至多一条短提示。 */
export function Card({ title, hint, onSave, children }: { title: string; hint?: string; onSave: () => Promise<number>; children: ReactNode }) {
  const t = useT(shared);
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
      {hint && <p className="sub">{hint}</p>}
      <div>
        <button type="button" className="primary" disabled={busy} onClick={() => void save()} aria-label={t.saveCard(title)}>{t.save}</button>
      </div>
      {status && <p role={status.ok ? "status" : "alert"}>{status.text}</p>}
    </section>
  );
}

// 仅当本段的服务端内容真正变化（保存后返回规整结果、或重新加载）时，草稿才重置；同路由其它段保存不影响本段草稿。
export function useDraft<T>(doc: unknown, init: () => T) {
  const [d, setD] = useState<T>(init);
  const sig = JSON.stringify(doc);
  useEffect(() => setD(init()), [sig]); // eslint-disable-line react-hooks/exhaustive-deps
  return [d, setD] as const;
}

/** 分段按钮：两三个互斥取值，按下的那个就是当前值。 */
export function Seg({ label, value, options, onChange, disabled }: {
  label: string; value: string; options: [string, string][]; onChange: (v: string) => void; disabled?: boolean;
}) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {options.map(([id, text]) => (
        <button key={id} type="button" aria-pressed={value === id} disabled={disabled} onClick={() => onChange(id)}>
          {text}
        </button>
      ))}
    </div>
  );
}

/** 标签输入：已有的值是可删的标签，回车或逗号添加。 */
export function TagInput({ label, values, onChange, placeholder }: { label: string; values: string[]; onChange: (v: string[]) => void; placeholder?: string }) {
  const t = useT(shared);
  const [text, setText] = useState("");
  const add = () => {
    const more = text.split(/[,，\s]+/).map((s) => s.trim()).filter((s) => s && !values.includes(s));
    if (more.length) onChange([...values, ...more]);
    setText("");
  };
  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      add();
    }
  };
  return (
    <div className="chips" role="group" aria-label={label}>
      {values.map((v) => (
        <span key={v} className="badge plain">
          {v}
          <button type="button" className="link" aria-label={t.removeTag(v)} onClick={() => onChange(values.filter((x) => x !== v))}>✕</button>
        </span>
      ))}
      <input aria-label={label} value={text} placeholder={placeholder} onChange={(e) => setText(e.target.value)} onKeyDown={onKey} onBlur={add} />
    </div>
  );
}

/** 本地校验失败：不发请求，原因显示在卡片里。 */
export const reject = (text: string) => Promise.reject(new Error(text));

/** 当前语言的共用文案（给非组件代码用）。 */
export const sharedNow = () => shared[lang()];
