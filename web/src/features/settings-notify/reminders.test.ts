import { describe, expect, it } from "vitest";
import { V28 } from "../../app/test-policy-server";
import { apply, choicesFor, current, type State } from "./reminders";

// 单选与存储字段的对应（设计 v2 §3.2、§6.2）：读出当前项、选一项写哪些字段；存储形状不变，未显示的字段（else、by_priority、类型行）原样保留。
const v28 = (): State => ({
  disturb: structuredClone(V28.disturb),
  modes: Object.fromEntries(V28.rules.map((r) => [r.key, r.mode])),
  packs: Object.fromEntries(V28.instances.map((i) => [i.id, i.pack])),
});

describe("读出当前项（v28）", () => {
  it("问题卡两边都发；到点、截止、跟进、停滞在对话里马上；没出来与来源失败推到手机；系统告警不提醒；建议攒多了的开关关着即不提醒", () => {
    const s = v28();
    expect(Object.fromEntries(["decision", "agent_answered", "scheduled", "overdue", "followup", "stale", "source_failing", "run_failed", "health", "new_proposals", "instance:ins_fe17", "instance_failed:ins_fe17", "instance:ins_b6b2"].map((k) => [k, current(s, k)]))).toEqual({
      decision: "both", agent_answered: "chat_now", scheduled: "chat_now", overdue: "chat_now", followup: "chat_now", stale: "chat_now",
      source_failing: "phone", run_failed: "phone", health: "off", new_proposals: "off",
      "instance:ins_fe17": "chat_now", "instance_failed:ins_fe17": "phone", "instance:ins_b6b2": "chat_now",
    });
  });

  it("「手机和对话都发」只在当前就是它时出现；告警类没有「下次聊天时」", () => {
    expect(choicesFor("decision", "both")).toEqual(["phone", "chat_now", "chat_next", "both", "off"]);
    expect(choicesFor("decision", "phone")).toEqual(["phone", "chat_now", "chat_next", "off"]);
    expect(choicesFor("source_failing", "phone")).toEqual(["phone", "chat_now", "off"]);
  });
});

describe("选一项写哪些字段", () => {
  it("问题卡改回只推手机：只把这一行写成 (off, default)，其余整段原样", () => {
    const s = v28();
    const c = apply(s, "decision", "phone");
    expect(c.mode).toBeUndefined();
    expect(c.disturb!.delivery.rows).toEqual({ ...V28.disturb.delivery.rows, decision: { agent: "off", user: "default" } });
    const { delivery: _a, ...rest } = c.disturb!;
    const { delivery: _b, ...was } = V28.disturb;
    expect(rest).toEqual(was);
  });

  it("有开关的行选不提醒只关开关、去处不动；再选回来先开开关；对话里马上保留原来的退路，改别的去掉", () => {
    const s = v28();
    expect(apply(s, "source_failing", "off").disturb).toEqual({ ...withoutDelivery(), push: { source_failing: { on: false }, template_failed: { on: true }, run_failed: { on: true }, new_proposals: { on: false, min: 10 } } });
    expect(apply(s, "overdue", "off").disturb).toEqual({ ...withoutDelivery(), remind_on_deadline: false });
    expect(apply(s, "followup", "off")).toEqual({ mode: { rule: "follow-up/clock_rules/follow-up-interval", mode: "off" } });
    expect(apply(s, "scheduled", "off").disturb!.proactive).toEqual({ ...V28.disturb.proactive, on: false });
    const np = apply(s, "new_proposals", "phone").disturb!;
    expect(np.push.new_proposals).toEqual({ on: true, min: 10 });
    expect(np).not.toHaveProperty("delivery"); // 去处原本就是 (off, default)
    const off = { ...s, modes: { ...s.modes, "follow-up/clock_rules/stale-check": "off" } };
    expect(apply(off, "stale", "chat_next")).toEqual({ mode: { rule: "follow-up/clock_rules/stale-check", mode: "auto" }, disturb: { ...withoutDelivery(), delivery: { ...V28.disturb.delivery, rows: { ...V28.disturb.delivery.rows, stale: { agent: "next_chat", user: "off" } } } } });
    expect(apply(s, "health", "chat_now").disturb!.delivery.rows.health).toEqual({ agent: "main", user: "off" });
    expect(apply(s, "instance:ins_fe17", "chat_now").disturb!.delivery.rows["instance:ins_fe17"]).toEqual({ agent: "main", user: "off", else: "next_chat" });
    expect(apply(s, "instance:ins_fe17", "phone").disturb!.delivery.rows["instance:ins_fe17"]).toEqual({ agent: "off", user: "default" });
  });

  it("没出来：单张卡选不提醒只写这张卡两层都不推、不动全局开关；全局开关关着时选回推到手机，开开关并把其余安排写成不提醒", () => {
    const s = v28();
    const c = apply(s, "instance_failed:ins_fe17", "off").disturb!;
    expect(c.delivery.rows["instance_failed:ins_fe17"]).toEqual({ agent: "off", user: "off" });
    expect(c).not.toHaveProperty("push");
    s.disturb.push = { template_failed: { on: false } };
    expect(current(s, "instance_failed:ins_b6b2")).toBe("off");
    const back = apply(s, "instance_failed:ins_fe17", "phone").disturb!;
    expect(back.push.template_failed).toEqual({ on: true });
    expect(back.delivery.rows["instance_failed:ins_fe17"]).toEqual({ agent: "off", user: "default" });
    expect(back.delivery.rows["instance_failed:ins_b6b2"]).toEqual({ agent: "off", user: "off" });
  });

  it("按优先级的覆盖与冻结的推送路由原样回传", () => {
    const s = v28();
    s.disturb.delivery.rows.overdue = { agent: "main", user: "off", by_priority: { high: { agent: "off", user: "default" } } };
    s.disturb.push = { run_failed: { on: true, route: "channel:old" } };
    expect(apply(s, "overdue", "chat_next").disturb!.delivery.rows.overdue).toEqual({ agent: "next_chat", user: "off", by_priority: { high: { agent: "off", user: "default" } } });
    expect(apply(s, "run_failed", "off").disturb!.push.run_failed).toEqual({ on: false, route: "channel:old" });
  });
});

function withoutDelivery() {
  const { delivery: _d, ...rest } = structuredClone(V28.disturb);
  return rest;
}
