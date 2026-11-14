import { describe, expect, it } from "vitest";
import * as I from "./ids";
import { vocab } from "./vocab";

// 防漂移（设计 §4.1）：ids.ts 是内核取值的前端镜像。这里读内核源码里的常量，内核有而 ids.ts 没有就失败，
// 失败信息指出要在 ids.ts 的哪一组、vocab.ts 的哪一类补上什么。单向检查：ids.ts 可以先放进内核即将新增的取值。
const go = import.meta.glob(
  [
    "../../../kernel/domain/*.go",
    "../../../kernel/delivery/delivery.go",
    "../../../kernel/delivery/disturb.go",
    "../../../kernel/policy/policy.go",
    "../../../kernel/policy/grant.go",
    "../../../kernel/auth/auth.go",
    "../../../kernel/executors/executors.go",
    "../../../kernel/rules/rules.go",
    "../../../kernel/delegation/delegation.go",
    "../../../kernel/sources/registry/outbound.go",
    "../../../kernel/sources/runner/runner.go",
    "../../../kernel/judgment/record/record.go",
    "../../../kernel/judgment/spotcheck/spotcheck.go",
    "../../../kernel/agents/direct/judge.go",
    "../../../kernel/doctor/report.go",
    "../../../api/health/health.go",
    "!**/*_test.go",
  ],
  { query: "?raw", import: "default", eager: true },
) as Record<string, string>;

function file(name: string): string {
  const hit = Object.entries(go).find(([f]) => f.endsWith(name));
  if (!hit) throw new Error(`读不到 ${name}：检查 ids.check.test.ts 里的 glob`);
  return hit[1];
}
const all = (text: string, re: RegExp, group = 1) => [...text.matchAll(re)].map((m) => m[group]);
/** 形如 `Name Type = "value"` 的常量（按类型名取）。 */
const typed = (f: string, type: string) => all(file(f), new RegExp(`^\\s*\\w+\\s+${type}\\s*=\\s*"([^"]+)"`, "gm"));
/** 名字以 prefix 开头的常量 `PrefixXxx = "value"`。 */
const named = (f: string, prefix: string) => all(file(f), new RegExp(`^\\s*${prefix}\\w*\\s*(?:\\w+\\s*)?=\\s*"([^"]+)"`, "gm"));
/** 从 marker 开始到下一个右括号或右花括号之间的全部字符串字面量。 */
function block(f: string, marker: string): string[] {
  const t = file(f);
  const at = t.indexOf(marker);
  if (at < 0) throw new Error(`${f} 里找不到「${marker}」：内核改了写法，更新 ids.check.test.ts`);
  const rest = t.slice(at + marker.length);
  return all(rest.slice(0, rest.search(/\n\s*[)}]/)), /"([^"]+)"/g);
}

const builtinUse = block("kernel/agents/direct/judge.go", "var builtinUse = map[string]string{");

// [ids.ts 里的组, vocab.ts 里的类, 内核里的取值]
const CASES: [string, string, () => string[]][] = [
  ["PHASE", "phase", () => typed("domain/phase.go", "Phase")],
  ["PRIORITY", "priority", () => typed("domain/item.go", "Priority")],
  ["ITEM_KIND", "itemKind", () => typed("domain/item.go", "Kind")],
  ["WAITING", "waiting", () => typed("domain/item.go", "WaitingKind")],
  ["RELATION", "relation", () => named("domain/item.go", "Rel")],
  ["PROJECT_STATUS", "projectStatus", () => typed("domain/entities.go", "ProjectStatus")],
  ["DELIVERY_CLASS", "deliveryClass", () => typed("domain/entities.go", "DeliveryClass")],
  ["DELIVERY_STATE", "deliveryState", () => typed("domain/entities.go", "DeliveryState")],
  ["ROUTE", "route", () => named("kernel/delivery/delivery.go", "Route")],
  ["SUPPRESSED", "suppressed", () => named("kernel/delivery/disturb.go", "Suppressed")],
  ["RUN_CATEGORY", "runCategory", () => typed("domain/entities.go", "RunCategory")],
  ["RUN_STATE", "runState", () => typed("domain/entities.go", "RunState")],
  ["OUTCOME", "outcome", () => named("kernel/delegation/delegation.go", "Outcome")],
  ["ACTOR_TYPE", "actorType", () => typed("domain/event.go", "ActorType")],
  ["VIA", "via", () => typed("domain/event.go", "Via")],
  ["SESSION_KIND", "sessionKind", () => named("domain/event.go", "Session")],
  ["ERROR_CODE", "error", () => typed("domain/errors.go", "Code")],
  ["SOURCE_TYPE", "sourceType", () => typed("domain/entities.go", "SourceType")],
  ["DISPOSITION", "disposition", () => typed("domain/entities.go", "Disposition")],
  ["UNJUDGED_REASON", "unjudgedReason", () => named("domain/entities.go", "Unjudged")],
  ["OUTBOUND_SCOPE", "outboundScope", () => named("kernel/sources/registry/outbound.go", "Scope")],
  ["ATTACH_STATE", "attachState", () => named("kernel/sources/runner/runner.go", "Attach")],
  ["WEB_ONLY", "webOnly", () => named("kernel/policy/policy.go", "Category")],
  ["PERM", "perm", () => named("kernel/policy/policy.go", "Perm")],
  ["WRITE", "write", () => named("kernel/policy/policy.go", "Write")],
  ["MUTE_SCOPE", "muteScope", () => named("kernel/policy/policy.go", "Mute")],
  ["LOCALITY", "locality", () => named("kernel/policy/policy.go", "Locality")],
  // Web 不提供对数概率换算的协议（ACT-JDP-11 断言前端产物里没有它），这里有意排除。
  ["PROTOCOL", "protocol", () => named("kernel/policy/policy.go", "Protocol").filter((p) => p !== "chat-logprobs")],
  ["GRANT_ACTION", "grantAction", () => block("kernel/policy/grant.go", "var GrantableActions = []string{")],
  ["CRED_KIND", "credKind", () => typed("kernel/auth/auth.go", "Kind")],
  ["EXECUTOR_KIND", "executorKind", () => named("kernel/executors/executors.go", "Kind")],
  ["RULE_MODE", "ruleMode", () => named("kernel/rules/rules.go", "Mode")],
  ["JUDGE_TEMPLATE", "judgeTemplate", () => builtinUse.filter((_, i) => i % 2 === 0)],
  ["JUDGE_USE", "judgeUse", () => builtinUse.filter((_, i) => i % 2 === 1)],
  ["JUDGE_BACKEND", "judgeBackend", () => named("kernel/judgment/record/record.go", "Backend")],
  ["DECIDED_BY", "decidedBy", () => named("kernel/judgment/record/record.go", "Decided")],
  ["LEVEL_OUTCOME", "levelOutcome", () => named("kernel/judgment/record/record.go", "Outcome")],
  ["SPOT_RESULT", "spotResult", () => block("kernel/judgment/spotcheck/spotcheck.go", "// 抽检结论的取值")],
  ["HEALTH_SIGNAL", "healthSignal", () => all(file("api/health/health.go"), /\bsig\("([a-z_]+)"/g)],
  ["DOCTOR_CHECK", "doctorCheck", () => block("kernel/doctor/report.go", "// 检查名")],
  ["EVENT", "event", () => named("domain/event.go", "Ev")],
];

describe("ids.ts 覆盖内核取值", () => {
  it.each(CASES)("%s", (group, kind, read) => {
    const kernel = read();
    expect(kernel.length, `从内核读到的 ${group} 为空：出处的写法变了，更新 ids.check.test.ts`).toBeGreaterThan(0);
    const mirror = new Set<string>((I as unknown as Record<string, readonly string[]>)[group]);
    const missing = kernel.filter((v) => !mirror.has(v));
    expect(missing, `内核新增了取值：在 web/src/app/ids.ts 的 ${group} 里加上 ${missing.join("、")}，并在 vocab.ts 的 ${kind} 里写中英两份显示名`).toEqual([]);
  });

  it("每组都在词表里有对应的一类，且词表的键正好是这一组", () => {
    for (const [group, kind] of CASES) {
      const ids = (I as unknown as Record<string, readonly string[]>)[group];
      const table = vocab.zh[kind as keyof typeof vocab.zh] as Record<string, unknown>;
      const keys = group === "EVENT" ? [...I.EVENT, ...I.EVENT_INTERNAL] : ids;
      expect(Object.keys(table).sort(), kind).toEqual([...keys].sort());
    }
  });
});

// 筛选门槛页的拖线起点：内置模板的 band 与包文件一致。
const prompts = import.meta.glob("../../../packs/*/prompts/*.md", { query: "?raw", import: "default", eager: true }) as Record<string, string>;
describe("PACK_BAND 与内置包的 band 一致", () => {
  it("每个声明了 band 的内置模板都在 PACK_BAND 里，取值相同", () => {
    const fromPacks: Record<string, { low?: number; high?: number }> = {};
    for (const [f, text] of Object.entries(prompts)) {
      const m = /^band:\s*\{([^}]*)\}/m.exec(text.split(/^---$/m)[1] ?? text);
      if (!m) continue;
      const band: { low?: number; high?: number } = {};
      for (const [, k, v] of m[1].matchAll(/(low|high):\s*([\d.]+)/g)) band[k as "low" | "high"] = Number(v);
      fromPacks[f.split("/").pop()!.replace(/\.md$/, "")] = band;
    }
    expect(Object.keys(fromPacks).length, "没读到任何包的 band：检查 glob 与 front matter 写法").toBeGreaterThan(0);
    expect(I.PACK_BAND, "内置包的 band 变了：同步 web/src/app/ids.ts 的 PACK_BAND").toEqual(fromPacks);
  });
});
