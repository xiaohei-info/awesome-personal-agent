import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText, useLang, useT } from "../../app/i18n";
import { useDirty } from "../../app/leave";
import type { Agent } from "./AgentPick";
import type { Declaration } from "./data";
import { Onboard } from "./OnboardForm";

import { strings } from "./strings";

type Field = { label: { zh: string; en: string }; choices?: string[]; pattern?: string; display_as?: string };
export type Options = { fields?: Record<string, Field>; cwd_choices?: string[] };

const NEW = "__new";

/**
 * 添加 Agent（界面设计 §6.3 第 1 步）：选用哪个程序（没有就「接入新的程序」，与原「接入其他 Agent」合并成一处），填名字，创建，
 * 直接进入这个 Agent 的页面，由接入清单带着做完剩下的步骤。权限上限不在这里：新 Agent 默认「只看」，只在它页面的后台权限里改。
 */
export function AddAgent({ hosts, taken, reload, onCancel }: { hosts: Declaration[]; taken: string[]; reload: () => Promise<void>; onCancel: () => void }) {
  const { api } = useSession();
  const t = useT(strings);
  const lang = useLang();
  const navigate = useNavigate();
  const [harness, setHost] = useState(hosts[0]?.harness ?? NEW);
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  async function submit(e: FormEvent) {
    e.preventDefault();
    setSaving(true);
    try {
      const card = await api.post<Agent>("/settings/agents", { name, harness, values: {}, enabled: true });
      navigate("/settings/agents/" + encodeURIComponent(card.id));
    } catch (err) {
      setError(errorText(err));
    } finally {
      setSaving(false);
    }
  }
  return (
    <section aria-label={t.add} className="panel stack">
      <h2>{t.add}</h2>
      <label>{t.addProgram}
        <select value={harness} onChange={(e) => setHost(e.target.value)}>
          {hosts.map((h) => <option key={h.harness} value={h.harness}>{h.name?.[lang] || h.harness}</option>)}
          <option value={NEW}>{t.addNewProgram}</option>
        </select>
      </label>
      {harness === NEW ? (
        <Onboard taken={taken} reload={reload} startOpen />
      ) : (
        <form className="stack" onSubmit={submit}>
          <label>{t.addName}<input required maxLength={80} value={name} onChange={(e) => setName(e.target.value)} /></label>
          <div className="actions">
            <button className="primary" disabled={saving || !name.trim()}>{t.addCreate}</button>
            <button type="button" onClick={onCancel}>{t.cancel}</button>
          </div>
          {error && <p role="alert">{error}</p>}
        </form>
      )}
    </section>
  );
}

/** 基本信息（整块保存）：名字、程序给出的选项（如模型）、工作目录、启用。没有改动时保存不可点，改了标「未保存」。 */
export function Basics({ card, host, onSaved }: { card: Agent; host?: Declaration; onSaved: () => Promise<void> }) {
  const { api } = useSession();
  const lang = useLang();
  const t = useT(strings);
  const [name, setName] = useState(card.name);
  const [values, setValues] = useState(card.values ?? {});
  const [enabled, setEnabled] = useState(card.enabled);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const [saving, setSaving] = useState(false);
  const options = host?.instance_options;
  const dirty = name !== card.name || enabled !== card.enabled || JSON.stringify(values) !== JSON.stringify(card.values ?? {});
  useDirty(dirty);
  function change(key: string, value: string) {
    const next = { ...values };
    if (value) next[key] = value; else delete next[key];
    setValues(next);
  }
  async function submit(e: FormEvent) {
    e.preventDefault();
    setSaving(true);
    setStatus(null);
    try {
      await api.post("/settings/agents", { id: card.id, rev: card.rev, name, harness: card.harness, values, enabled });
      setStatus({ ok: true, text: t.saved() });
      await onSaved();
    } catch (err) {
      setStatus({ ok: false, text: errorText(err) });
    } finally {
      setSaving(false);
    }
  }
  return (
    <form aria-label={t.basics} className="panel stack" onSubmit={submit}>
      <h2>{t.basics}{dirty && <span className="unsaved">{t.unsaved}</span>}</h2>
      <p className="sub">{t.program}{t.colon}{host?.name?.[lang] || card.harness}</p>
      <label>{t.agentName}<input required maxLength={80} value={name} onChange={(e) => setName(e.target.value)} /></label>
      {Object.entries(options?.fields ?? {}).map(([k, f]) => <label key={k}>{f.label[lang]}
        {f.choices ? <select value={values[k] ?? ""} onChange={(e) => change(k, e.target.value)}><option value="">{t.fieldDefault}</option>{f.choices.map((v) => <option key={v}>{v}</option>)}</select>
          : <input value={values[k] ?? ""} placeholder={t.fieldDefault} onChange={(e) => change(k, e.target.value)} />}
      </label>)}
      {!!options?.cwd_choices?.length && <label>{t.cwd}<select value={values.cwd ?? ""} onChange={(e) => change("cwd", e.target.value)}><option value="">{t.fieldDefault}</option>{options.cwd_choices.map((v) => <option key={v}>{v}</option>)}</select></label>}
      <label><input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />{t.enabled}</label>
      <div>
        <button className="primary" disabled={saving || !dirty}>{t.save}</button>
        {status && <span role={status.ok ? "status" : "alert"} className={"saved-note" + (status.ok ? "" : " bad")}>{status.text}</span>}
      </div>
    </form>
  );
}
