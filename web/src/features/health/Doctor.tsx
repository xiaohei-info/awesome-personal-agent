import { Link } from "react-router-dom";
import { useLang, useT } from "../../app/i18n";
import { useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { useLabel } from "../../app/values";
import { vocab, type DoctorData } from "../../app/vocab";
import type { DoctorCode } from "../../app/ids";
import { strings } from "./strings";

// 自检详情（设计 §15.5）：形状对应 GET /v1/doctor（kernel/doctor.Report）。每项按机器码 code 与 data 显示一句中文结论；
// 内核的英文 why、next 和原始 ID 只在「显示原始值」里出现。正常项折叠成一行，展开后每个对象一行。
type Status = "ok" | "warning" | "error";
export interface DoctorDetail {
  subject: string;
  status: Status;
  code: DoctorCode;
  why: string;
  next?: string;
  data?: Record<string, unknown>;
}
export interface DoctorCheck {
  name: string;
  status: Status;
  code: DoctorCode;
  why: string;
  next: string;
  details?: DoctorDetail[];
  data?: Record<string, unknown>;
}
export interface DoctorReport {
  ok: boolean;
  checks: DoctorCheck[];
}

const BADGE = { ok: "ok", warning: "warn", error: "bad" } as const;
/** 没接入的可选项：灰色「未接入」，不是提醒。 */
const NOT_CONNECTED = new Set<string>(["not_configured", "models_none", "sources_none", "host_none", "channels_none"]);
type Go = keyof (typeof strings)["zh"]["doctorGo"];
/** 结论旁的去处。 */
const GO: Partial<Record<DoctorCode, [string, Go]>> = {
  tier_kernel_only: ["/settings/models", "model"],
  tier_paused: ["/settings/models", "resume"],
  models_paused: ["/settings/models", "resume"],
  tier_no_host: ["/settings-exec", "host"],
  models_failing: ["/settings/models", "models"],
  sources_attention: ["/sources", "sources"],
  sources_restored_missing: ["/sources", "sources"],
  channels_backlog: ["/settings/notify", "notify"],
};

/** 一个结论码（加 data）的中文一句；多处问题时逐条接起来。 */
function useSay(): (code: DoctorCode, data?: DoctorData) => string {
  const say = vocab[useLang()].doctorCode;
  return (code, data) => {
    const problems = Array.isArray(data?.problems) ? (data!.problems as DoctorCode[]) : [];
    return (problems.length ? problems : [code]).map((c) => say[c]?.(data) ?? c).join("；");
  };
}

/** 对象的显示名：来源用来源名，端点叫「写作模型」「打分模型」，能力包与渠道走词表。 */
function useSubject(): (check: string, d: DoctorDetail) => string {
  const t = useT(strings);
  const label = useLabel();
  const source = useNames("source");
  return (check, d) => {
    switch (check) {
      case "model_endpoint":
        return t.models[d.subject.replace(/_endpoint$/, "")] ?? d.subject;
      case "sources":
        return source(d.subject);
      case "packs":
        return label("pack", d.subject);
      case "channels":
        return d.subject === "export" ? t.exportExt : label("route", d.subject.startsWith("channel:") ? d.subject : "channel:" + d.subject.replace(/^channel\./, ""));
      case "overlaps":
        return String(d.data?.name ?? d.subject);
      default:
        return d.subject;
    }
  };
}

function Badge({ c }: { c: { status: Status; code: string } }) {
  const t = useT(strings);
  if (c.status === "ok" && NOT_CONNECTED.has(c.code)) return <span className="badge">{t.notConnected}</span>;
  return <span className={"badge " + BADGE[c.status]}>{t.status[c.status]}</span>;
}

function Raw({ id, code, why, next }: { id: string; code: string; why: string; next?: string }) {
  return (
    <span className="raw-why">
      <code className="raw-id">{id} · {code}</code>{" "}
      <span lang="en" className="sub">{why}{next ? ` — ${next}` : ""}</span>
    </span>
  );
}

function CheckView({ c }: { c: DoctorCheck }) {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const say = useSay();
  const subject = useSubject();
  const title = label("doctorCheck", c.name);
  const go = GO[c.code];
  const head = (
    <>
      {title} <Badge c={c} /> <span className="sub">{say(c.code, c.data)}</span>
      {go && c.status === "ok" && <> <Link className="btn" to={go[0]}>{t.doctorGo[go[1]]}</Link></>}
      {raw && <Raw id={c.name} code={c.code} why={c.why} next={c.next} />}
    </>
  );
  const body = (
    <>
      {!!c.details?.length && (
        <ul className="sub-list">
          {c.details.map((d) => (
            <li key={d.subject}>
              {d.status !== "ok" && <Badge c={d} />} {subject(c.name, d)} · {say(d.code, d.data)}
              {raw && <Raw id={d.subject} code={d.code} why={d.why} next={d.next} />}
            </li>
          ))}
        </ul>
      )}
      {go && c.status !== "ok" && <Link className="btn" to={go[0]}>{t.doctorGo[go[1]]}</Link>}
    </>
  );
  // 正常项默认折叠成一行；要处理的直接展开。
  if (c.status === "ok") {
    return c.details?.length ? (
      <details aria-label={title} className="signal doctor-check">
        <summary>{head}</summary>
        {body}
      </details>
    ) : (
      <p aria-label={title} className="signal doctor-check">{head}</p>
    );
  }
  return (
    <article aria-label={title} className="signal doctor-check">
      <h3>{title} <Badge c={c} /></h3>
      <p>{say(c.code, c.data)}</p>
      {raw && <Raw id={c.name} code={c.code} why={c.why} next={c.next} />}
      {body}
    </article>
  );
}

export function DoctorDetails({ report }: { report: DoctorReport }) {
  const t = useT(strings);
  return (
    <details className="panel">
      <summary>{t.doctor}</summary>
      {report.checks.map((c) => <CheckView key={c.name} c={c} />)}
    </details>
  );
}
