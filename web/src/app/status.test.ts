import { describe, expect, it } from "vitest";
import { rowsOf } from "./status";

// 被退回试运行的规则落到它唯一的可编辑处：提醒类（跟进、停滞）在提醒与推送的矩阵行，其余在设置 › 自动动手。
describe("状态 › 规则退回试运行的链接", () => {
  it("跟进、停滞落到矩阵行锚点；其他规则落到自动动手的规则锚点；没有明细时落到自动动手", () => {
    const s = { id: "rule_fallbacks", ok: false, count: 3, next: "", detail: ["follow-up/clock_rules/follow-up-interval", "follow-up/clock_rules/stale-check", "p/event_rules/rule-1"] };
    expect(rowsOf(s).map((r) => r.to)).toEqual(["/settings/notify#row-followup", "/settings/notify#row-stale", "/settings/rules#rule-p%2Fevent_rules%2Frule-1"]);
    expect(rowsOf({ ...s, detail: undefined }).map((r) => r.to)).toEqual(["/settings/rules"]);
  });
});
