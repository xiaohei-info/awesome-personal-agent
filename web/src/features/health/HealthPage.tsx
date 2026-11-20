import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { Go, PageHead } from "../../app/page";
import { useLang, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle, useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { entryOf, useLabel } from "../../app/values";
import { rejudgeHint } from "../../app/strings";
import { assess, type HealthReport, type Row, type Status } from "../../app/status";
import { strings } from "./strings";
import { useFaultSay, useNoticeSay } from "../../app/notice";
import { DoctorDetails, type DoctorReport } from "./Doctor";

// 诊断 › 状态（界面设计 §5.3）：形状对应 GET /v1/health 与 GET /v1/doctor（附录 B.2），判定全在服务端，这里只归类与措辞。
// 三节：要你处理（每行：问题一句、影响一句、一个主按钮，按对象计数）、仅供知晓（本人自己的选择、会自己恢复的、没有可做的，不计数）、
// 正常（折叠）。只报不修：按钮落到修复的权威位置，并能一键回来（§3.3）。
export type { HealthReport, Signal } from "../../app/status";
export type { DoctorCheck, DoctorDetail, DoctorReport } from "./Doctor";

interface Run { id: string; target: string }

/** 一行的问题、影响、主按钮与落点。 */
interface Said { problem: string; impact?: string; button?: string; to?: string; second?: { button: string; to: string } }

// 被拒原因码 → 下一步：缺依据的去签发授权（Agent 页的「可以替我决定」），没权限的看钥匙，其余看操作记录。
const GRANT_CODES = new Set(["basis_required", "session_not_direct", "grant_required"]);

function useSay(runTarget: (run?: string) => string | undefined): (r: Row, st: Status) => Said {
  const t = useT(strings);
  const lang = useLang();
  const source = useNames("source");
  const credential = useNames("credential");
  const label = useLabel();
  const fault = useFaultSay();
  const notice = useNoticeSay();
  return (r, st) => {
    const [kind, sub] = r.kind.split(":");
    const model = sub ? t.models[sub] : "";
    switch (kind) {
      case "sourceFail": return { problem: t.sourceFail(source(r.obj ?? ""), r.n), impact: t.sourceImpact, button: t.fix, to: r.to };
      case "sourceAuth": return { problem: t.sourceAuth(source(r.obj ?? "")), impact: t.sourceImpact, button: t.fix, to: r.to };
      case "sourceOff": return { problem: t.sourceOff(source(r.obj ?? "")), button: t.enable, to: r.to };
      case "outcall": {
        const item = runTarget(r.obj);
        return item ? { problem: t.outcallItem, impact: t.notDone, button: t.seeItem, to: "/items/" + encodeURIComponent(item) } : { problem: t.outcall(r.n), impact: t.notDone, button: t.seeRuns, to: r.to };
      }
      case "queue": return { problem: t.queue(r.n) };
      case "delivery": return { problem: t.delivery(r.n), impact: t.deliveryImpact, button: t.seeChannels, to: r.to };
      case "chain": return { problem: t.chain, impact: t.halfway, button: r.obj ? t.seeItem : t.seeItems, to: r.to };
      case "drift": return { problem: t.drift(r.n), impact: t.driftImpact, button: t.seeProjects, to: r.to };
      case "packs": return { problem: t.packs(r.n), impact: t.packsImpact, button: undefined, to: undefined };
      case "modelUnset": return { problem: t.modelUnset(model), impact: t.modelImpact[sub] ?? "", button: t.setupModel, to: r.to };
      case "modelFail": return { problem: t.modelFail(model), impact: t.modelImpact[sub] ?? "", button: t.testModel, to: r.to };
      case "screening": return { problem: t.screening(r.n), impact: t.screeningImpact, button: t.testModel, to: r.to };
      case "waitScope": return { problem: t.waitScope(r.n), impact: t.waitScopeImpact, button: t.choose, to: r.to };
      case "restored": return { problem: t.restored(r.n), button: t.release, to: r.to };
      case "waitModel": return { problem: [t.waitModel(r.n), rejudgeHint(lang, "model_unavailable", st.rejudgeDays)].filter(Boolean).join(" · ") };
      case "waitPaused": return { problem: [t.waitPaused(r.n), rejudgeHint(lang, "models_paused", st.rejudgeDays)].filter(Boolean).join(" · "), button: t.seeSources, to: r.to };
      case "expired": return { problem: t.expired(r.n) };
      case "fallback": return { problem: r.obj ? t.fallbackOne(ruleName(lang, r.obj)) : t.fallbacks(r.n), impact: t.fallbackImpact, button: t.reopen, to: r.to };
      case "runFail": {
        const f = fault(r.code ? { code: r.code, params: r.params } : undefined);
        return { problem: f?.say ?? t.outcallItem, impact: t.runFailImpact(f?.next ?? ""), button: t.seeItem, to: r.to };
      }
      case "receipt": {
        const reason = r.params?.reason;
        const why = reason ? label("grantReason", reason) : r.code ? label("error", r.code) : t.receiptNoReason;
        // 初筛待定：授权本身没问题，第一步是去处理那件事上的卡
        if (reason === "grant_screening_pending" && r.obj)
          return { problem: t.receiptOne(credential(r.who ?? ""), t.receiptCmd[r.cmd ?? ""] ?? r.cmd ?? "", why), impact: t.receiptImpact, button: t.goCard, to: "/items/" + encodeURIComponent(r.obj),
            second: { button: t.seeWhy, to: "/audit?object=" + encodeURIComponent(r.obj) } };
        const grant = !!r.code && GRANT_CODES.has(r.code);
        return {
          problem: t.receiptOne(credential(r.who ?? ""), t.receiptCmd[r.cmd ?? ""] ?? r.cmd ?? "", why),
          impact: t.receiptImpact,
          button: grant ? t.goGrant : r.code === "permission_denied" ? t.seeKey : t.seeWhy,
          to: grant || r.code === "permission_denied" ? "/settings/agents" : r.to,
          second: { button: t.seeWhy, to: r.obj ? "/audit?object=" + encodeURIComponent(r.obj) : "/audit" },
        };
      }
      case "receipts": return { problem: t.receipts(r.n), impact: t.receiptsImpact, button: t.seeWhy, to: r.to, second: { button: t.seeAccess, to: "/settings/agents" } };
      case "notice": {
        // 动作没能完成：一句话 + 这件事；有更具体的修法（去接模型等）时它是主按钮，"看这件事"退为次按钮。
        const n = notice(r.code, r.params);
        const why = r.code === "action_rejected" && r.params?.code ? `${n?.say ?? ""}：${label("error", r.params.code)}` : n?.say ?? t.noticeUnknown;
        const impact = sub === "dropped" ? t.noticeDropped : t.noticeKept;
        return n?.go
          ? { problem: why, impact, button: n.go.label, to: n.go.to, second: { button: t.seeItem, to: r.to ?? "/audit" } }
          : { problem: why, impact, button: t.seeItem, to: r.to };
      }
      case "others": return { problem: t.others(r.n), button: t.look, to: r.to };
      case "stalled": return { problem: t.stalled, impact: t.stalledImpact, button: t.nudge, to: r.to };
      case "lag": return { problem: t.lag(r.n) };
      default: return { problem: t.unknown(r.n) };
    }
  };
}

/** 规则名：词表按「包/规则 id」查，规则键是「包/组/id」；查不到时原样。 */
const ruleName = (lang: "zh" | "en", key: string) => {
  const [pack, , id] = key.split("/");
  return entryOf(lang, "rule", `${pack}/${id}`)?.label ?? key;
};

function ItemName({ id }: { id?: string }) {
  const title = useItemTitle(id ?? "");
  return id ? <strong>{title}</strong> : null;
}

function Line({ r, said, raw }: { r: Row; said: Said; raw: boolean }) {
  const itemRow = r.kind === "chain" || r.kind === "stalled" || r.kind === "runFail" || (r.kind === "receipt" && !!r.obj?.startsWith("itm_"));
  return (
    <li className="act-row">
      <div>
        <span>{itemRow && <><ItemName id={r.obj} />{" · "}</>}{said.problem}</span>
        {said.impact && <span className="sub">{said.impact}</span>}
        {raw && <code className="raw-id">{[r.signal, r.kind, r.obj].filter(Boolean).join(" · ")}</code>}
      </div>
      {said.to && said.button && <Go className="btn" to={said.to}>{said.button}</Go>}
      {said.second && <Go className="quiet" to={said.second.to}>{said.second.button}</Go>}
    </li>
  );
}

export function HealthPage() {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const { api } = useSession();
  const [report, setReport] = useState<HealthReport | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [doctor, setDoctor] = useState<DoctorReport | null>(null);
  const [runs, setRuns] = useState<Run[]>([]);
  const say = useSay((id) => runs.find((x) => x.id === id)?.target);

  const load = useCallback(() => {
    setError(null);
    // 自检是补充信息：取不到不影响状态的展示。自检测通了已保存的端点会清掉该端点此前的失败，所以回来后再取一次状态。
    api.get<DoctorReport>("/doctor").then(
      (d) => {
        setDoctor(d);
        api.get<HealthReport>("/health").then(setReport, () => undefined);
      },
      () => setDoctor(null),
    );
    // 超时的运行：信号只给运行 id，落点要的是它为哪件事跑。取不到时按钮退回运行记录。
    api.get<{ runs: Run[] }>("/runs?state=timeout&limit=100").then((r) => setRuns(r.runs), () => setRuns([]));
    return api.get<HealthReport>("/health").then(setReport, (e) => setError(e));
  }, [api]);
  useEffect(() => {
    load();
  }, [load]);

  const st = report && assess(report);
  const head = !st ? t.title : st.act.length ? t.head.act(st.act.length) : st.paused ? t.head.paused : t.head.ok;
  return (
    <main className="status-page">
      <PageHead>{head}</PageHead>
      {error != null && (
        <>
          <ErrorNote error={error} />
          <button onClick={load}>{t.retry}</button>
        </>
      )}
      {!st ? (
        error == null && <p role="status">{t.loading}</p>
      ) : (
        <>
          {st.act.length > 0 && (
            <section aria-label={t.actTitle} className="panel">
              <h2><span className={"dot " + st.kind} aria-hidden="true" /> {t.actTitle}</h2>
              <ul className="rows">{st.act.map((r, i) => <Line key={i} r={r} said={say(r, st)} raw={raw} />)}</ul>
            </section>
          )}
          {(st.info.length > 0 || st.paused) && (
            <section aria-label={t.infoTitle} className="panel">
              <h2><span className="dot paused" aria-hidden="true" /> {t.infoTitle}</h2>
              <ul className="rows">
                {st.paused && (
                  <li className="act-row">
                    <div><span>{t.paused(st.pausedBacklog)}</span></div>
                    <Go className="btn" to="/settings/models">{t.resume}</Go>
                  </li>
                )}
                {st.info.map((r, i) => <Line key={i} r={r} said={say(r, st)} raw={raw} />)}
              </ul>
            </section>
          )}
          {st.fine.length > 0 && (
            <details className="panel">
              <summary>
                <span className="dot ok" aria-hidden="true" /> {st.act.length || st.info.length || st.paused ? t.fine(st.fine.length) : t.allFine(st.fine.length)}
              </summary>
              <ul className="sub-list">
                {st.fine.map((s) => <li key={s.id}>{label("healthSignal", s.id)}</li>)}
              </ul>
            </details>
          )}
          {raw && report && (
            <details className="panel">
              <summary>{t.rawSignals}</summary>
              <ul className="sub-list">
                {report.signals.filter((s) => !s.ok).map((s) => <li key={s.id}><code className="raw-id">{`${s.id} · ${s.why ?? ""} · ${s.next}`}</code>{s.rejections?.map((x) => <code key={x.pack} className="raw-id">{`${x.pack}: ${x.reason}`}</code>)}</li>)}
              </ul>
            </details>
          )}
          {doctor && <DoctorDetails report={doctor} />}
        </>
      )}
    </main>
  );
}
