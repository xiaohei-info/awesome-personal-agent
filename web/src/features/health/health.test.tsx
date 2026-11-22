import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import type { HealthReport, Signal } from "./HealthPage";

// 测试边界替身：按 api/health 的契约应答 GET /v1/health（十三项信号、顺序固定，错误体见附录 B.3）与 GET /v1/doctor。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const ORDER = ["source_failures", "outcall", "delivery_backlog", "chain_trips", "agentsmd_drift", "pack_rejections", "model_unavailable",
  "unjudged_backlog", "rule_fallbacks", "receipt_failures", "other_alerts", "stalled_delegations", "clock_lag"];
const TITLES = ["来源读取失败", "Agent 后台运行排队或超时", "提醒发不出去", "自动化连锁被叫停", "仓库规范和 APA 不一致", "能力包加载失败", "模型连不上",
  "积压待处理", "自动规则被退回试运行", "交回被拒", "其他告警", "委派久无回音", "定时器迟到"];

// 形状与取值同 kernel/doctor（codes.go）：全部正常的一份与有问题的一份。
const FINE = {
  ok: true,
  checks: [
    { name: "tier", status: "ok", code: "tier_kernel_only", why: "tier 1 (kernel only): no generate endpoint is configured; this is a complete mode, not an error", next: "to reach tier 2, configure the generate endpoint", data: { tier: 1 } },
    { name: "model_endpoint", status: "ok", code: "models_none", why: "no endpoint is configured (optional)", next: "configure the endpoints", details: [
      { subject: "judge_endpoint", status: "ok", code: "not_configured", why: "not configured (optional); not an error" },
      { subject: "generate_endpoint", status: "ok", code: "not_configured", why: "not configured (optional); not an error" },
    ] },
    { name: "sources", status: "ok", code: "sources_ok", why: "2 sources: commands exist and the outbound scope fits the endpoints", next: "nothing to do", data: { count: 2 }, details: [
      { subject: "src_mail", status: "ok", code: "source_ok", why: "command found, outbound scope fits the endpoints, last run: ok", data: { last_run: "ok" } },
      { subject: "src_new", status: "ok", code: "source_ok", why: "command found, outbound scope fits the endpoints, last run: never", data: { last_run: "never" } },
    ] },
    { name: "packs", status: "ok", code: "packs_ok", why: "10 packs loaded, none rejected", next: "nothing to do", data: { loaded: 10, rejected: 0 } },
    { name: "channels", status: "ok", code: "channels_none", why: "no channel or export ext key is issued (optional)", next: "issue an ext key" },
  ],
};
const BAD = {
  ok: false,
  checks: [
    { name: "sources", status: "warning", code: "sources_attention", why: "some sources need attention; see details", next: "choose the outbound scope", data: { count: 1 }, details: [
      { subject: "src_mail", status: "warning", code: "source_outbound_unset", why: "the external outbound scope is not chosen", next: "choose it in Web", data: { last_run: "failed", consecutive_failures: 3, problems: ["source_outbound_unset", "source_last_failed"] } },
    ] },
  ],
};
let DOCTOR: unknown = FINE;

function report(bad: Record<string, Partial<Signal>> = {}): HealthReport {
  const signals = ORDER.map((id) => ({ id, ok: true, count: 0, next: "server next for " + id, ...bad[id] }) as Signal);
  return { healthy: signals.every((s) => s.ok || s.severity === "warning"), signals };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/health"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

function serve(get: () => Response) {
  const calls: string[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    calls.push((init.method ?? "GET") + " " + url);
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_mail", name: "邮件" }, { id: "src_new", name: "新来源" }] });
    if (url === "/v1/health") return get();
    if (url === "/v1/doctor") return json(200, DOCTOR);
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}
const main = () => within(document.querySelector("main")!);

beforeEach(() => {
  sessionStorage.clear();
  localStorage.clear();
  DOCTOR = FINE;
});

describe("诊断 › 状态（flow-health / ACT-HLT-01；界面设计 §5.3）", () => {
  const act = () => within(main().getByRole("region", { name: "要你处理" }));
  const info = () => within(main().getByRole("region", { name: "仅供知晓" }));
  /** 「要你处理」里含某句话的那一行。 */
  const row = (re: RegExp) => act().getByText(re).closest("li")!;

  it("全部正常：页头「运行正常」，十三项都有中文名并折叠在一起；没有写请求", async () => {
    const s = serve(() => json(200, report()));
    mount(s.impl);
    expect(await screen.findByRole("heading", { level: 1, name: "运行正常" })).toBeInTheDocument();
    const fine = main().getByText("13 项都正常").closest("details")!;
    expect([...fine.querySelectorAll("li")].map((li) => li.textContent)).toEqual(TITLES);
    expect(main().queryByRole("region", { name: "要你处理" })).not.toBeInTheDocument();
    expect(s.calls.filter((c) => !c.startsWith("GET"))).toEqual([]);
  });

  it("要你处理：每行一句问题、一句影响、一个写着要做什么的主按钮，按对象计数；英文原因与 id 不上主界面", async () => {
    const bad = {
      source_failures: { ok: false, count: 2, severity: "error", why: "a source has consecutive pull failures", detail: [{ source: "src_mail", enabled: true, consecutive_failures: 5, needs_input_item: "itm_9" }, { source: "src_new", enabled: false, consecutive_failures: 0 }] },
      outcall: { ok: false, count: 2, severity: "error", why: "outcall runs timed out", detail: { long: { running: 1, queued: 3, longest_wait_seconds: 42 }, short: { running: 0, queued: 0, longest_wait_seconds: 0 }, timeouts: ["r1", "r2"] } },
      delivery_backlog: { ok: false, count: 1, severity: "error", why: "deliveries are queued", detail: ["d1"] },
      chain_trips: { ok: false, count: 1, severity: "error", why: "the breaker stopped", detail: ["itm_3"] },
      pack_rejections: { ok: false, count: 1, severity: "error", why: "a pack failed to load", detail: ["pk"], rejections: [{ pack: "pk", source: "user", reason: "band is required" }] },
      model_unavailable: { ok: false, count: 3, severity: "error", why: "x", detail: { judge: "", generate: "not_configured", endpoint: "not_configured", judgements: [{ kind: "decision-card", object: "itm_1", endpoint: "generate" }] } },
      unjudged_backlog: { ok: false, count: 57, severity: "warning", why: "w", detail: { model_unavailable: 50, models_paused: 0, outbound_unset: 3, undecided: 2, restored: 2, expired: 3, sources: [{ source: "src_mail", count: 57 }] } },
      rule_fallbacks: { ok: false, count: 1, severity: "error", why: "fell back to observe", detail: ["p/event_rules/rule-1"] },
      receipt_failures: { ok: false, count: 2, severity: "error", why: "rejected", detail: ["itm_7", "itm_8"] },
      stalled_delegations: { ok: false, count: 1, severity: "warning", why: "stalled", detail: ["itm_5"] },
    };
    mount(serve(() => json(200, report(bad))).impl);
    // 来源 1（停用的那个只是知晓）+ 超时 2 + 提醒 1 + 连锁 1 + 能力包 1 + 模型 1 + 外发范围 1 + 放开 1 + 规则 1 + 交回 1 + 久无回音 1 = 12
    expect(await screen.findByRole("heading", { level: 1, name: "12 件要处理" })).toBeInTheDocument();
    const src = row(/邮件 要重新登录/);
    expect(src).toHaveTextContent("这个来源的新消息进不来");
    expect(src).not.toHaveTextContent("连续"); // 认证失败不说次数，更不会出现「连续 0 次」
    expect(within(src).getByRole("link", { name: "去修" })).toHaveAttribute("href", "/sources?open=src_mail");
    expect(document.querySelector("main")!.textContent, "主界面不出现来源 id").not.toMatch(/src_/);
    expect(document.querySelector("main")!.textContent).not.toMatch(/consecutive pull failures|band is required/);
    expect(act().getAllByText("这件事没做完")).toHaveLength(2);
    expect(within(row(/条提醒没送到/)).getByRole("link", { name: "去看渠道" })).toHaveAttribute("href", "/settings/channels");
    expect(within(row(/层数太多/)).getByRole("link", { name: "看这件事" })).toHaveAttribute("href", "/items/itm_3");
    const pk = row(/个能力包没加载上/);
    expect(pk).toHaveTextContent("修好包文件后会自动重新加载");
    expect(within(pk).queryByRole("link")).not.toBeInTheDocument(); // 动作在本机，网页上没有按钮
    const gen = row(/写作模型还没配置/);
    expect(gen).toHaveTextContent("建议和简报暂时写不出来");
    expect(within(gen).getByRole("link", { name: "去配置" })).toHaveAttribute("href", "/settings/models");
    expect(within(row(/3 条消息在等你选发给模型多少内容/)).getByRole("link", { name: "去选" })).toHaveAttribute("href", "/sources");
    expect(within(row(/2 条找回的消息等你放进建议/)).getByRole("link", { name: "去放开" })).toHaveAttribute("href", "/sources");
    expect(within(row(/已退回试运行/)).getByRole("link", { name: "去重新打开" })).toHaveAttribute("href", "/rules#rule-p%2Fevent_rules%2Frule-1");
    const rc = row(/交回的结果被拒了 2 次/);
    expect(within(rc).getByRole("link", { name: "看被拒的原因" })).toHaveAttribute("href", "/audit");
    expect(within(rc).getByRole("link", { name: "看这个 Agent 的钥匙与档位" })).toHaveAttribute("href", "/settings/agents");
    expect(within(row(/没回音/)).getByRole("link", { name: "去催或改派" })).toHaveAttribute("href", "/delegation#d-itm_5");
    // 仅供知晓：本人停用的来源、排队、等模型恢复、已过期——不计数
    expect(info().getByText("新来源 已停用")).toBeInTheDocument();
    expect(info().getByText("还有 3 个在排队")).toBeInTheDocument();
    expect(info().getByText("52 条等模型恢复后处理")).toBeInTheDocument();
    expect(info().getByText("3 条已过期，不再自动处理")).toBeInTheDocument();
    expect(main().getByText("其余 3 项正常")).toBeInTheDocument();
    expect(main().queryByRole("button", { name: /修复|fix/i })).not.toBeInTheDocument();
  });

  it("模型已暂停：放在仅供知晓、不计数，页头是中性的「模型已暂停」；暂停期间积下的条数一并说明", async () => {
    const r = report({
      model_unavailable: { detail: { judge: "paused", generate: "paused", endpoint: "", paused: true } },
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, rejudge_days: 7, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    expect(await screen.findByRole("heading", { level: 1, name: "模型已暂停" })).toBeInTheDocument();
    expect(main().queryByRole("region", { name: "要你处理" })).not.toBeInTheDocument();
    const paused = info().getByText("模型已暂停，积下 2 条").closest("li")!;
    expect(within(paused).getByRole("link", { name: "去恢复" })).toHaveAttribute("href", "/settings/models");
    expect(info().getByText("2 条是模型暂停期间积下的 · 恢复后自动判断（最近 7 天的）")).toBeInTheDocument();
  });

  it.each([
    [7, "2 条是模型暂停期间积下的 · 恢复后自动判断（最近 7 天的）"],
    [0, "2 条是模型暂停期间积下的 · 恢复后在积压里放开"],
    [undefined, "2 条是模型暂停期间积下的"],
  ])("恢复之后：暂停期间积下的仍列在仅供知晓里，去向随 rejudge_days（%s）", async (days, text) => {
    const r = report({
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, rejudge_days: days, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    await screen.findByRole("heading", { level: 1, name: "运行正常" });
    expect(info().getByText(/条是模型暂停期间积下的/).textContent).toBe(text);
  });

  it.each([
    [7, "3 条等模型恢复后处理 · 恢复后自动判断（最近 7 天的）"],
    [0, "3 条等模型恢复后处理 · 恢复后在积压里放开"],
  ])("v0.1 的 undecided 并进「等模型恢复」，去向随 rejudge_days（%s）", async (days, text) => {
    const r = report({
      unjudged_backlog: { ok: false, count: 3, severity: "warning", why: "w", detail: { model_unavailable: 1, models_paused: 0, outbound_unset: 0, undecided: 2, restored: 0, expired: 0, rejudge_days: days, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    await screen.findByRole("heading", { level: 1, name: "运行正常" });
    expect(info().getByText(/条等模型恢复后处理/).textContent).toBe(text);
  });

  it("读取失败：显示服务端原因并可重试", async () => {
    let fail = true;
    mount(serve(() => (fail ? json(500, { code: "internal", why: "boom", next: "retry" }) : json(200, report()))).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("heading", { level: 1, name: "运行正常" })).toBeInTheDocument();
  });

  it("没有读权限（403）：显示权限原因，不显示任何信号", async () => {
    mount(serve(() => json(403, { code: "permission_denied", why: "credential kind cannot read the ledger", next: "use an owner or agent credential" })).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(main().queryByRole("region", { name: "要你处理" })).not.toBeInTheDocument();
  });
});

describe("诊断 › 状态的自检详情（flow-doctor / ACT-DOC-12）", () => {
  const section = async () => (await screen.findByText("自检详情")).closest("details")!;

  it("全部正常：每项一行中文结论，不出现「错误」和英文说明；未接入是灰色；去接模型有按钮", async () => {
    const s = serve(() => json(200, report()));
    mount(s.impl);
    const sec = await section();
    const text = sec.textContent!;
    expect(text).not.toMatch(/错误|error/i);
    expect(text).not.toContain("not configured");
    expect(text).not.toContain("tier 1");
    expect(within(sec).getByLabelText("运行形态")).toHaveTextContent("只用内核运行，没有接模型");
    expect(within(within(sec).getByLabelText("运行形态")).getByRole("link", { name: "去接模型" })).toHaveAttribute("href", "/settings/models");
    const models = within(sec).getByLabelText("模型");
    expect(within(models).getAllByText("未接入")[0]).toHaveClass("badge");
    expect(within(models).getAllByText("未接入")[0]).not.toHaveClass("warn");
    expect(models).toHaveTextContent("写作模型 · 未接入");
    expect(within(models).getAllByText("未接入")).toHaveLength(1); // 检查一级只用灰色标记说一次，不再重复一句
    const sources = within(sec).getByLabelText("来源");
    expect(sources.tagName).toBe("DETAILS");
    expect(sources).not.toHaveAttribute("open");
    expect(sources).toHaveTextContent("2 个来源都正常");
    expect(sources).toHaveTextContent("邮件 · 上次读取成功");
    expect(sources).toHaveTextContent("新来源 · 还没读过");
    expect(sec.textContent, "主界面不出现来源 id").not.toMatch(/src_/);
    expect(s.calls).toContain("GET /v1/doctor");
  });

  it("要处理的展开显示，多处问题逐条说出；英文说明不上主界面", async () => {
    DOCTOR = BAD;
    mount(serve(() => json(200, report())).impl);
    const sources = within(await section()).getByRole("article", { name: "来源" });
    expect(sources).toHaveTextContent("有来源要处理");
    expect(sources).toHaveTextContent("邮件 · 还没选外发范围，新消息先压着；连续 3 次没读到");
    expect(sources.textContent, "主界面不出现来源 id").not.toMatch(/src_/);
    expect(within(sources).getByRole("link", { name: "去看来源" })).toHaveAttribute("href", "/sources");
    expect(sources).not.toHaveTextContent("outbound scope");
  });

  it("「显示原始值」打开后才出现检查名、机器码和内核的英文说明", async () => {
    localStorage.setItem("apa.raw", "1");
    mount(serve(() => json(200, report())).impl);
    const sec = await section();
    expect(sec).toHaveTextContent("tier · tier_kernel_only");
    expect(sec).toHaveTextContent("this is a complete mode, not an error");
    expect(sec).toHaveTextContent("judge_endpoint · not_configured");
  });
});

describe("自检结论词表 doctorCode", () => {
  it("每个机器码中英两份都有一句话，且不把机器码原样当文字", async () => {
    const { DOCTOR_CODE } = await import("../../app/ids");
    const { vocab } = await import("../../app/vocab");
    for (const lang of ["zh", "en"] as const) {
      for (const code of DOCTOR_CODE) {
        const say = vocab[lang].doctorCode[code];
        for (const data of [undefined, {}, { count: 2, tier: 1, loaded: 3, rejected: 1, backlog: 2, consecutive_failures: 4, last_run: "never", status: 500, missing_hooks: ["a"] }]) {
          const text = say(data);
          expect(text.trim(), `${lang}.${code}`).not.toBe("");
          expect(text, `${lang}.${code}`).not.toContain(code.includes("_") ? code : "\u0000");
          expect(text, `${lang}.${code}`).not.toMatch(/undefined|NaN/);
        }
      }
    }
  });

  it("正常结论的中文里没有「错误」二字和「不是错误」式的反说", async () => {
    const { vocab } = await import("../../app/vocab");
    const fine = ["ok", "not_configured", "data_dir_ok", "data_dir_no_backup", "config_ok", "tier_paused", "tier_kernel_only", "tier_no_host", "tier_full",
      "models_paused", "models_none", "models_ok", "sources_none", "sources_ok", "source_ok", "packs_no_loader", "packs_ok", "host_none", "host_ok",
      "host_ready", "host_ready_no_selfcheck", "overlaps_skipped", "overlaps_none", "channels_none", "channels_ok", "channel_ok", "export_ok"] as const;
    for (const c of fine) expect(vocab.zh.doctorCode[c]({ count: 1, last_run: "ok" }), c).not.toContain("错误");
  });
});

// 2026-11-20 根因整改：运行故障与被拒的交回是事件，带原因码；诊断页要说清原因、结果没落账，并给一个可点的下一步。
describe("运行故障与被拒的交回说人话（界面设计 §5.3）", () => {
  const act = () => within(main().getByRole("region", { name: "要你处理" }));
  it("退出码 126：说「执行程序无法启动（没有运行权限）」与下一步，不给重新运行；被拒的交回写原因与「去放开它的档位」", async () => {
    const bad = {
      outcall: { ok: false, count: 1, severity: "error", why: "x", detail: { long: { running: 0, queued: 0, longest_wait_seconds: 0 }, short: { running: 0, queued: 0, longest_wait_seconds: 0 }, timeouts: [],
        failures: [{ run: "run_1", target: "itm_brief", category: "task", fault: { code: "exec_denied", params: { exit: "126" } } }] } },
      receipt_failures: { ok: false, count: 1, severity: "error", why: "rejected", detail: ["itm_7"], receipts: [{ object: "itm_7", command: "delegate.report", exit: 23, code: "basis_required", credential: "cred_x" }] },
    };
    mount(serve(() => json(200, report(bad))).impl);
    const fault = (await screen.findByText(/执行程序无法启动（没有运行权限）/)).closest("li")!;
    expect(fault).toHaveTextContent("结果没有落账，事项没被修改");
    expect(fault).toHaveTextContent("先在这台电脑上给它运行权限");
    expect(within(fault).getByRole("link", { name: "看这件事" })).toHaveAttribute("href", "/items/itm_brief#agent");
    expect(within(fault).queryByRole("button", { name: /重新运行/ })).toBeNull();
    const rej = act().getByText(/交回结果被拒：这一项需要你本人决定/).closest("li")!;
    expect(rej).toHaveTextContent("结果没有落账");
    expect(within(rej).getByRole("link", { name: "去决定这件事" })).toHaveAttribute("href", "/items/itm_7");
    expect(within(rej).queryByRole("link", { name: "去放开它的档位" })).toBeNull();
    expect(within(rej).getByRole("link", { name: "看被拒的原因" })).toHaveAttribute("href", "/audit?object=itm_7");
    expect(main().queryByText(/缺少你的原话|exit status|basis_required/)).toBeNull();
  });

  // 2026-11-20 本人裁决：规则动作失败、运行没拉起是事件不是问题；诊断页逐条说清哪件事、为什么、下一步。来源认证失败按 kind 说"要重新登录"。
  it("动作没能完成：模型没接的写「去接模型」与「看这件事」，作废的说明可以重开；没拉起的写原因；来源认证失败按 kind", async () => {
    const bad = {
      other_alerts: { ok: false, count: 2, severity: "error", why: "x", detail: ["ins_1", "itm_open"], notices: [
        { object: "ins_1", rule: "brief/clock_rules/scheduled", code: "model_unset", item: "itm_brief", dropped: true },
        { object: "itm_open", code: "background_refused", params: { detail: "执行者 other-host.cli 不在项目允许的执行者名单内" }, item: "itm_open" },
      ] },
      source_failures: { ok: false, count: 1, severity: "error", why: "x", detail: [{ source: "src_mail", enabled: true, consecutive_failures: 1, kind: "auth_or_config" }] },
    };
    mount(serve(() => json(200, report(bad))).impl);
    const unset = (await screen.findByText("需要的模型还没接")).closest("li")!;
    expect(unset).toHaveTextContent("已作废");
    expect(within(unset).getByRole("link", { name: "去接模型" })).toHaveAttribute("href", "/settings/models");
    expect(within(unset).getByRole("link", { name: "看这件事" })).toHaveAttribute("href", "/items/itm_brief");
    const refused = act().getByText(/没能开始：执行者 other-host.cli 不在项目允许的执行者名单内/).closest("li")!;
    expect(refused).toHaveTextContent("事项没被修改");
    expect(within(refused).getByRole("link", { name: "看这件事" })).toHaveAttribute("href", "/items/itm_open");
    expect(act().getByText(/要重新登录/)).toBeInTheDocument();
    expect(main().queryByText(/其他告警/)).toBeNull();
  });
});
