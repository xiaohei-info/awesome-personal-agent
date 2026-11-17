import { useCallback, useEffect, useState } from "react";
import { PageHead } from "../../app/page";
import { errorText } from "../../app/i18n";
import { useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { Card, reject, useDraft } from "../settings-policy/shared";
import { strings } from "./strings";

// 设置 › 笔记库：GET/PUT /settings/note-vaults（附录 B.2），PUT 整体替换。协议白名单、重名与模板合法性由服务端判定，被拒绝时原因原样显示、草稿保留。
export interface Vault { name: string; open_url: string }
interface Doc { version: number; vaults: Vault[] }

/** 示例路径只用于预览，不是任何真实文件。 */
export const SAMPLE_PATH = "folder/example note.md";
/** 与服务端 kernel/policy.OpenURL 同一规则（URI 组件编码），两端跑同一份 open-url-cases.json。 */
export const render = (tpl: string, name: string, path = SAMPLE_PATH) =>
  tpl.replaceAll("{vault}", encodeURIComponent(name)).replaceAll("{path}", encodeURIComponent(path));

/** 打开方式的预设（附录 D.1）：只是模板字符串，存的仍是 open_url。 */
export const OBSIDIAN = "obsidian://open?vault={vault}&file={path}";
type Mode = "obsidian" | "none" | "custom";
const modeOf = (tpl: string): Mode => (tpl === OBSIDIAN ? "obsidian" : tpl === "" ? "none" : "custom");

export function SettingsVaultsPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [doc, setDoc] = useState<Doc | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    setError(null);
    try {
      setDoc(await api.get<Doc>("/settings/note-vaults"));
    } catch (e) {
      setDoc(null);
      setError(errorText(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);

  if (error)
    return (
      <main>
        <PageHead>{t.title}</PageHead>
        <p role="alert">{error}</p>
        <button onClick={() => void load()}>{t.retry}</button>
      </main>
    );
  if (!doc) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <VaultsCard doc={doc} save={async (vaults) => { const next = await api.put<Doc>("/settings/note-vaults", { vaults }); setDoc(next); return next.version; }} />
    </main>
  );
}

function VaultsCard({ doc, save }: { doc: Doc; save: (v: Vault[]) => Promise<number> }) {
  const t = useT(strings);
  const [rows, setRows] = useDraft<Vault[]>(doc, () => doc.vaults.map((v) => ({ ...v })));
  const edit = (i: number, p: Partial<Vault>) => setRows(rows.map((r, n) => (n === i ? { ...r, ...p } : r)));
  const submit = () => {
    const seen = new Set<string>();
    for (const [i, r] of rows.entries()) {
      const name = r.name.trim();
      if (!name) return reject(t.nameEmpty(i + 1));
      if (seen.has(name)) return reject(t.nameDup(name));
      seen.add(name);
    }
    return save(rows.map((r) => ({ name: r.name.trim(), open_url: r.open_url.trim() })));
  };
  return (
    <Card title={t.card} onSave={submit}>
      {rows.length === 0 && <p className="sub">{t.empty}</p>}
      {rows.map((r, i) => {
        const mode = modeOf(r.open_url.trim());
        return (
          <div key={i} className="stack">
            <div className="line">
              <input aria-label={t.name(i + 1)} placeholder={t.namePh} value={r.name} onChange={(e) => edit(i, { name: e.target.value })} />
              <select aria-label={t.url(i + 1)} value={mode} onChange={(e) => edit(i, { open_url: e.target.value === "obsidian" ? OBSIDIAN : "" })}>
                <option value="obsidian">{t.obsidian}</option>
                <option value="none">{t.nameOnly}</option>
                {mode === "custom" && <option value="custom">{t.custom}</option>}
              </select>
              <button type="button" aria-label={t.del(i + 1)} onClick={() => setRows(rows.filter((_, n) => n !== i))}>✕</button>
            </div>
            <details open={mode === "custom"}>
              <summary>{t.advanced}</summary>
              <input aria-label={t.customUrl(i + 1)} placeholder={t.urlPh} value={mode === "custom" ? r.open_url : ""} onChange={(e) => edit(i, { open_url: e.target.value })} />
              <p className="sub">{t.hint}</p>
            </details>
            {r.name.trim() && mode !== "none" && (
              <p className="sub" aria-label={t.preview(i + 1)}>{t.sample}: {SAMPLE_PATH} → <code>{render(r.open_url.trim(), r.name.trim())}</code></p>
            )}
          </div>
        );
      })}
      <div>
        <button type="button" className="link" onClick={() => setRows([...rows, { name: "", open_url: OBSIDIAN }])}>{t.add}</button>
      </div>
    </Card>
  );
}
