// 每类提醒的单选（设计 v2 §3.2）：一类提醒只有一个问题"要不要提醒你、提醒到哪"。这里是纯逻辑：
// 读出当前是哪一个选项（含「产生」类开关），以及选了某一项要写哪些字段（disturb 段的去处与开关，或规则模式）。
// 存储不变：仍是 disturb.delivery.rows 与旧开关（push.*.on、remind_on_deadline、proactive.on、规则模式），页面只是换处读写同一组字段。
// 行键与内核 kernel/policy/delivery.go 一致；每类提醒唯一可编辑的页面见 EDITORS（homes.check.test.ts 守住"每一行都有编辑位置"）。
import { TASK_TYPES, type Delivery, type Target } from "./matrix";

export type Choice = "phone" | "chat_now" | "chat_next" | "both" | "off";

/** 每类提醒唯一可编辑的页面（§4）。 */
export type Home = "decide" | "rules" | "schedule" | "sources" | "pipeline" | "agents" | "proposals" | "status";
export const HOME_PATH: Record<Home, string> = {
  decide: "/settings",
  rules: "/settings/rules",
  schedule: "/briefs/schedule",
  sources: "/sources",
  pipeline: "/settings/source-pipeline",
  agents: "/settings/agents",
  proposals: "/proposals",
  status: "/health",
};

/** 行种类：固定行就是行键；按实例、按类型的行取前缀。 */
export function kindOf(key: string): string {
  const i = key.indexOf(":");
  return i < 0 ? key : key.slice(0, i + 1);
}

/** 每种行的编辑位置。类型行（task_result:、task_failed:）不单独编辑：它们是新建安排的初值，生效值显示并改在每张安排卡上。 */
export const EDITORS: Record<string, Home> = {
  decision: "decide",
  agent_answered: "decide",
  scheduled: "rules",
  overdue: "rules",
  followup: "rules",
  stale: "rules",
  rule_reminder: "rules",
  source_alert: "rules",
  "task_result:": "schedule",
  "task_failed:": "schedule",
  "instance:": "schedule",
  "instance_failed:": "schedule",
  source_failing: "sources",
  source_result: "pipeline",
  run_failed: "agents",
  new_proposals: "proposals",
  health: "status",
};

export const FOLLOWUP_RULE = "follow-up/clock_rules/follow-up-interval";
export const STALE_RULE = "follow-up/clock_rules/stale-check";

/** 「产生」类开关：关掉就"不提醒"，去处原样不动。 */
export type Switch =
  | { kind: "push"; cat: "source_failing" | "template_failed" | "run_failed" | "new_proposals" }
  | { kind: "deadline" }
  | { kind: "proactive" }
  | { kind: "rule"; rule: string };

export function switchOf(key: string): Switch | undefined {
  switch (kindOf(key)) {
    case "source_failing":
    case "run_failed":
    case "new_proposals":
      return { kind: "push", cat: key as "source_failing" };
    case "instance_failed:":
    case "task_failed:":
      return { kind: "push", cat: "template_failed" };
    case "overdue":
      return { kind: "deadline" };
    case "scheduled":
      return { kind: "proactive" };
    case "followup":
      return { kind: "rule", rule: FOLLOWUP_RULE };
    case "stale":
      return { kind: "rule", rule: STALE_RULE };
  }
  return undefined;
}

/** 告警类不给"下次聊天时告诉我"（设计 §3.2：不适用的选项不显示）。 */
export const isAlert = (key: string) => ["instance_failed:", "task_failed:", "source_failing", "run_failed", "health"].includes(kindOf(key));

export interface PushRule { on: boolean; route?: string; min?: number }
export const PUSH_CATS = ["source_failing", "template_failed", "run_failed", "new_proposals"] as const;
/** 与 kernel/policy.PushDefaults 一致。 */
export const PUSH_DEFAULT: Record<string, PushRule> = { source_failing: { on: true }, template_failed: { on: true }, run_failed: { on: true }, new_proposals: { on: false, min: 10 } };

export interface State {
  disturb: Record<string, any>; // GET /settings/policy 的 disturb（delivery 总是生效的矩阵）
  modes: Record<string, string>; // 规则键 → 模式
  packs: Record<string, string>; // 实例 id → 类型
}

export const push = (d: Record<string, any>, cat: string): PushRule => ({ ...PUSH_DEFAULT[cat], ...(d.push?.[cat] ?? {}) });

/** 开关当前是否开着；没有开关的行恒为开。 */
export function switchOn(s: State, key: string): boolean {
  const sw = switchOf(key);
  if (!sw) return true;
  switch (sw.kind) {
    case "push":
      return push(s.disturb, sw.cat).on;
    case "deadline":
      return !!s.disturb.remind_on_deadline;
    case "proactive":
      return !!s.disturb.proactive?.on;
    case "rule":
      return s.modes[sw.rule] === "auto";
  }
}

/** 这一行生效的去处：存了就是它；实例行没存时跟它的类型行（再没有取"其他"），与内核 rowOf → Lookup 同一顺序。 */
export function targetOf(m: Delivery, key: string, packs: Record<string, string>): Target {
  const off: Target = { agent: "off", user: "off" };
  if (m.rows[key]) return m.rows[key];
  const inst = /^(instance|instance_failed):(.+)$/.exec(key);
  if (!inst) return off;
  const pack = packs[inst[2]] ?? "";
  const prefix = inst[1] === "instance" ? "task_result:" : "task_failed:";
  const known = (TASK_TYPES as readonly string[]).includes(pack);
  return (known && m.rows[prefix + pack]) || m.rows[prefix + "other"] || off;
}

export function choiceOf(t: Target): Choice {
  if (t.agent === "off") return t.user === "off" ? "off" : "phone";
  if (t.user !== "off") return "both";
  return t.agent === "main" ? "chat_now" : "chat_next";
}

/** 当前选项：开关关着就是"不提醒"；否则按两层去处。 */
export function current(s: State, key: string): Choice {
  if (!switchOn(s, key)) return "off";
  return choiceOf(targetOf(s.disturb.delivery as Delivery, key, s.packs));
}

/** 选项要写的去处：保留按优先级的覆盖与"对话收不到时"的退路（不显示，原样回传）。 */
export function targetFor(c: Choice, old: Target): Target {
  const keep = old.by_priority ? { by_priority: old.by_priority } : {};
  switch (c) {
    case "phone":
      return { agent: "off", user: old.user !== "off" ? old.user : "default", ...keep };
    case "chat_now":
      return { agent: "main", user: "off", ...(old.agent === "main" && old.else ? { else: old.else } : {}), ...keep };
    case "chat_next":
      return { agent: "next_chat", user: "off", ...keep };
    case "both":
      return { ...old };
    case "off":
      return { agent: "off", user: "off", ...keep };
  }
}

/** 选了 c 之后要写的东西：disturb 段（整段，已保存的原样带回）与可选的规则模式。 */
export interface Change { disturb?: Record<string, any>; mode?: { rule: string; mode: string } }

export function apply(s: State, key: string, c: Choice): Change {
  const d = s.disturb;
  const m = d.delivery as Delivery;
  const sw = switchOf(key);
  const rows = { ...m.rows };
  const out: Change = {};
  const next: Record<string, any> = { ...d };
  const setSwitch = (on: boolean) => {
    if (!sw) return;
    switch (sw.kind) {
      case "push": {
        const all: Record<string, PushRule> = {};
        for (const cat of PUSH_CATS) {
          const r = push(d, cat);
          // route 是冻结键：原样带回已存的值
          all[cat] = { on: cat === sw.cat ? on : r.on, ...(d.push?.[cat]?.route ? { route: d.push[cat].route } : {}), ...(cat === "new_proposals" ? { min: r.min ?? 10 } : {}) };
        }
        next.push = all;
        break;
      }
      case "deadline":
        next.remind_on_deadline = on;
        break;
      case "proactive":
        next.proactive = { ...(d.proactive ?? {}), on };
        break;
      case "rule":
        if (s.modes[sw.rule] !== (on ? "auto" : "off")) out.mode = { rule: sw.rule, mode: on ? "auto" : "off" };
        break;
    }
  };
  const failedRow = kindOf(key) === "instance_failed:";
  if (c === "off") {
    if (failedRow) {
      // "没出来"的开关管全部安排：单张卡选"不提醒"只把这张卡两层都设为不推，不动全局开关（§6.2 #2）
      rows[key] = targetFor("off", targetOf(m, key, s.packs));
    } else if (sw) setSwitch(false);
    else rows[key] = targetFor("off", targetOf(m, key, s.packs));
  } else {
    if (failedRow && !push(d, "template_failed").on) {
      // 全局开关原来关着：打开它，其余安排的"没出来"写成两层都不推，保持它们"不提醒"
      for (const id of Object.keys(s.packs)) if ("instance_failed:" + id !== key) rows["instance_failed:" + id] = { agent: "off", user: "off" };
    }
    setSwitch(true);
    rows[key] = targetFor(c, targetOf(m, key, s.packs));
  }
  const rowsChanged = JSON.stringify(rows) !== JSON.stringify(m.rows);
  if (rowsChanged) next.delivery = { ...m, rows };
  if (JSON.stringify(next) !== JSON.stringify(d)) {
    // 去处没变就不带矩阵（服务端沿用已存的那份）；delivery_saved 是只读字段
    const { delivery_saved: _s, delivery: _d, ...body } = next;
    out.disturb = rowsChanged ? { ...body, delivery: next.delivery } : body;
  }
  return out;
}

/** 这一行能选哪些项（"手机和对话都发"只在当前就是它时显示）。 */
export function choicesFor(key: string, cur: Choice): Choice[] {
  const l: Choice[] = ["phone", "chat_now", ...(isAlert(key) ? [] : (["chat_next"] as Choice[])), "off"];
  if (cur === "both" || (cur === "chat_next" && isAlert(key))) l.splice(l.length - 1, 0, cur);
  return l;
}
