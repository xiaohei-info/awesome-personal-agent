import { useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { errorText, useLang } from "../../app/i18n";
import type { Agent } from "./AgentPick";

type Field = { label: { zh: string; en: string }; choices?: string[]; pattern?: string; display_as?: string };
export type Options = { fields?: Record<string, Field>; cwd_choices?: string[] };

export function AgentEditor({ initial, hosts, onDone }: {
  initial: Partial<Agent>; hosts: { harness: string; instance_options?: Options }[]; onDone: (saved?: boolean) => void;
}) {
  const { api } = useSession();
  const lang = useLang();
  const [name, setName] = useState(initial.name ?? "");
  const [harness, setHost] = useState(initial.harness ?? hosts[0]?.harness ?? "");
  const [values, setValues] = useState(initial.values ?? {});
  const [enabled, setEnabled] = useState(initial.enabled ?? true);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const options = hosts.find(h => h.harness === harness)?.instance_options;
  function change(key: string, value: string) {
    const next = { ...values };
    if (value) next[key] = value; else delete next[key];
    setValues(next);
  }
  async function submit(e: FormEvent) {
    e.preventDefault(); setSaving(true);
    try {
      await api.post("/settings/agents", { id: initial.id, rev: initial.rev, name, harness, values, enabled });
      onDone(true);
    } catch (e) { setError(errorText(e)); } finally { setSaving(false); }
  }
  return <form className="panel stack" onSubmit={submit}>
    <label>{({ zh: "已接入的程序", en: "Connected program" })[lang]}<select required disabled={!!initial.id} value={harness} onChange={e => { setHost(e.target.value); setValues({}); }}>
      {hosts.map(h => <option key={h.harness}>{h.harness}</option>)}
    </select></label>
    <label>{({ zh: "Agent 名字", en: "Agent name" })[lang]}<input required maxLength={80} value={name} onChange={e => setName(e.target.value)} /></label>
    {Object.entries(options?.fields ?? {}).map(([k, f]) => <label key={k}>{f.label[lang]}
      {f.choices ? <select value={values[k] ?? ""} onChange={e => change(k, e.target.value)}><option value="">{({ zh: "跟随 Agent 设置", en: "Agent default" })[lang]}</option>{f.choices.map(v => <option key={v}>{v}</option>)}</select>
        : <input value={values[k] ?? ""} placeholder={({ zh: "跟随 Agent 设置", en: "Agent default" })[lang]} onChange={e => change(k, e.target.value)} />}
    </label>)}
    {options?.cwd_choices?.length && <label>{({ zh: "工作目录", en: "Working directory" })[lang]}<select value={values.cwd ?? ""} onChange={e => change("cwd", e.target.value)}><option value="">{({ zh: "默认", en: "Default" })[lang]}</option>{options.cwd_choices.map(v => <option key={v}>{v}</option>)}</select></label>}
    <label><input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />{({ zh: "启用", en: "Enabled" })[lang]}</label>
    <div><button disabled={saving || !harness}>{({ zh: "保存 Agent", en: "Save Agent" })[lang]}</button> <button type="button" onClick={() => onDone()}>{({ zh: "取消", en: "Cancel" })[lang]}</button></div>
    {error && <p role="alert">{error}</p>}
  </form>;
}
