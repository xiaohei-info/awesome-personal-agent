import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText, useT } from "../../app/i18n";
import { Go } from "../../app/page";
import { useRaw } from "../../app/raw";
import { pick as strings } from "./strings";

// 形状对应 GET /v1/executors（附录 B.2）：带 runner 时每项有 reason（现在不能用于这种运行的原因），并给 selected（按拉起同一口径的选择预览）。
interface Label { zh: string; en: string }
export interface Executor {
  id: string; kind: string; host?: string; harness?: string; label?: string;
  startable?: boolean; overridable?: Record<string, { label: Label; choices: string[] }>; overrides?: Record<string, string>;
  /** 所属 Agent（声明）给人看的名字。 */
  agent?: string;
  /** 不在 APA 主机上。 */
  remote?: boolean;
  reason?: string;
}
export interface ExecutorView { executors: Executor[]; selected?: { executor: string; source: string; reason?: string } }
export type Runner = "task" | "main_session" | "source_batch";

/** 执行者的显示名：有标签用标签；与 Agent 同名的那一个（声明没列执行者时内核建的）用 Agent 的名字。 */
export const executorName = (e: Pick<Executor, "id" | "label" | "agent" | "harness">) => e.label || (e.id === e.harness && e.agent) || e.id;

/** 一种运行的执行者列表与选择预览；rule 是规则键（预览「跟随规则」），delegated 表示派的是委派（可用不在本机的执行者）。 */
export function useExecutors(runner: Runner, rule = "", delegated = false) {
  const { api } = useSession();
  const [view, setView] = useState<ExecutorView | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    const q = new URLSearchParams({ runner, ...(rule ? { rule } : {}), ...(delegated ? { delegated: "true" } : {}) });
    api.get<ExecutorView>("/executors?" + q).then((v) => { if (active) { setView(v); setError(""); } }, (e) => { if (active) setError(errorText(e)); });
    return () => { active = false; };
  }, [api, runner, rule, delegated]);
  return { view, error };
}

/**
 * 「用哪个执行者」选择器（界面设计 §3.2 公共件，全站只有这一份）：写的是所在对象自己的属性（事项、安排、规则、来源处理），
 * 第一项是「不指定」，并说明不指定时现在会用谁（跟随默认、跟随主执行者、跟随规则）。列出全部执行者，含不在本机的；
 * 现在用不了的置灰并说明，不剔除。生效顺序由服务端判定（§13.6），这里只取预览。collapsed 为真时先收成一行「由某某处理」。
 */
export function ExecutorPick({ value, onChange, runner, rule, delegated, collapsed, label, item }: {
  value: string; onChange: (id: string) => void; runner: Runner; rule?: string; delegated?: boolean; collapsed?: boolean; label?: string; item?: boolean;
}) {
  const t = useT(strings);
  const raw = useRaw();
  const { view, error } = useExecutors(runner, rule, delegated);
  const [open, setOpen] = useState(!collapsed);
  if (!view) return error ? <small className="sub">{error}</small> : null;
  const byId = (id?: string) => view.executors.find((e) => e.id === id);
  const fallbackId = view.selected?.executor ?? "";
  const fallback = fallbackId ? (byId(fallbackId) ? executorName(byId(fallbackId)!) : fallbackId) : "";
  const follow = view.selected?.source === "rule" ? t.followRule : view.selected?.source === "main" ? t.followMain : t.followDefault;
  const chosen = byId(value);
  const now = value ? (chosen ? executorName(chosen) : value) : fallback;
  const reason = value ? chosen?.reason : view.selected?.reason;
  const shown = value || fallbackId;
  if (!open)
    return (
      <p className="sub">
        {now ? t.handledBy(now) : t.none}{" "}
        <button type="button" className="link" onClick={() => setOpen(true)}>{t.change}</button>
      </p>
    );
  return (
    <span className="actions">
      <label>{label ?? (item ? t.forItem : t.label)}
        <select value={value} onChange={(e) => onChange(e.target.value)}>
          <option value="">{fallback ? t.follow(follow, fallback) : t.followNone(follow)}</option>
          {value && !chosen && <option value={value} disabled>{value}</option>}
          {view.executors.map((e) => (
            <option key={e.id} value={e.id} disabled={!!e.reason}>
              {executorName(e)}{e.remote && e.host ? t.onHost(e.host) : ""}{e.reason ? t.unusable : ""}
            </option>
          ))}
        </select>
      </label>
      {shown && reason && <small className="sub">{t.unusableNow}</small>}
      {shown && <Go className="quiet" to={"/executors#" + encodeURIComponent("executor-" + shown)}>{t.see}</Go>}
      {raw && reason && <code className="raw-id">{reason}</code>}
    </span>
  );
}
