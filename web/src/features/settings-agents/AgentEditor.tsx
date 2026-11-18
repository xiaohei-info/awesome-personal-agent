import { useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { errorText, useLang, useT } from "../../app/i18n";
import { useLabel } from "../../app/values";
import type { Agent } from "./AgentPick";

import { strings } from "./strings";

type Field = { label: { zh: string; en: string }; choices?: string[]; pattern?: string; display_as?: string };
export type Options = { fields?: Record<string, Field>; cwd_choices?: string[] };

export function AgentEditor({ initial, hosts, initialCap, policyVersion, onDone }: {
  initial: Partial<Agent>; hosts: { harness: string; headless?: { access?: Record<string, unknown> }; instance_options?: Options }[]; initialCap: string; policyVersion: number; onDone: (saved?: boolean) => void;
}) {
  const { api } = useSession();
  const lang = useLang();
  const t = useT(strings);
  const label = useLabel();
  const [cap, setCap] = useState(initialCap);
  const [name, setName] = useState(initial.name ?? "");
  const [harness, setHost] = useState(initial.harness ?? hosts[0]?.harness ?? "");
  const [values, setValues] = useState(initial.values ?? {});
  const [enabled, setEnabled] = useState(initial.enabled ?? true);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const host = hosts.find(h => h.harness === harness);
  const options = host?.instance_options;
  function change(key: string, value: string) {
    const next = { ...values };
    if (value) next[key] = value; else delete next[key];
    setValues(next);
  }
  async function submit(e: FormEvent) {
    e.preventDefault(); setSaving(true);
    try {
      await api.post("/settings/agents", { id: initial.id, rev: initial.rev, name, harness, values, enabled, ...(cap === initialCap ? {} : { permission_cap: cap, policy_version: policyVersion }) });
      onDone(true);
    } catch (e) { setError(errorText(e)); } finally { setSaving(false); }
  }
  return <form className="panel stack" onSubmit={submit}>
    <label>{t.cardConnectedProgram}<select required disabled={!!initial.id} value={harness} onChange={e => { setHost(e.target.value); setValues({}); setCap("read"); }}>
      {hosts.map(h => <option key={h.harness}>{h.harness}</option>)}
    </select></label>
    <label>{t.cardAgentName}<input required maxLength={80} value={name} onChange={e => setName(e.target.value)} /></label>
    {Object.entries(options?.fields ?? {}).map(([k, f]) => <label key={k}>{f.label[lang]}
      {f.choices ? <select value={values[k] ?? ""} onChange={e => change(k, e.target.value)}><option value="">{t.cardAgentDefault}</option>{f.choices.map(v => <option key={v}>{v}</option>)}</select>
        : <input value={values[k] ?? ""} placeholder={t.cardAgentDefault} onChange={e => change(k, e.target.value)} />}
    </label>)}
    {options?.cwd_choices?.length && <label>{t.cardWorkingDirectory}<select value={values.cwd ?? ""} onChange={e => change("cwd", e.target.value)}><option value="">{t.cardDefault}</option>{options.cwd_choices.map(v => <option key={v}>{v}</option>)}</select></label>}
    <label>{t.cardPermissionCap}<select value={cap} onChange={event => setCap(event.target.value)}>
      {[...new Set(["read", ...Object.keys(host?.headless?.access ?? {})])].map(tier => <option key={tier} value={tier}>{label("perm",tier)}</option>)}
    </select></label>
    {cap === "full" && cap !== initialCap && <label><input type="checkbox" required />{t.cardConfirmFullAccess}</label>}
    <p className="sub">{t.cardRunsInAnIndependentBackgroundSession}</p>
    <label><input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />{t.cardEnabled}</label>
    <div><button disabled={saving || !harness}>{t.cardSaveAgent}</button> <button type="button" onClick={() => onDone()}>{t.cardCancel}</button></div>
    {error && <p role="alert">{error}</p>}
  </form>;
}
