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
const TITLES = ["来源读取失败", "后台任务排队或超时", "提醒发不出去", "自动化连锁被叫停", "仓库规范和 APA 不一致", "能力包加载失败", "模型连不上",
  "积压待处理", "自动规则被退回试运行", "agent 回报失败", "其他告警", "委派久无回音", "定时器迟到"];

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
      { subject: "mail", status: "ok", code: "source_ok", why: "command found, outbound scope fits the endpoints, last run: ok", data: { last_run: "ok" } },
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
      { subject: "mail", status: "warning", code: "source_outbound_unset", why: "the external outbound scope is not chosen", next: "choose it in Web", data: { last_run: "failed", consecutive_failures: 3, problems: ["source_outbound_unset", "source_last_failed"] } },
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
    if (url === "/v1/sources") return json(200, { sources: [{ id: "mail", name: "邮件" }] });
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

describe("诊断 › 状态（flow-health / ACT-HLT-01）", () => {
  it("全部正常：页头「一切正常」，十三项都有中文名并折叠在一起；没有写请求", async () => {
    const s = serve(() => json(200, report()));
    mount(s.impl);
    expect(await screen.findByRole("heading", { level: 1, name: "一切正常" })).toBeInTheDocument();
    const fine = main().getByText("13 项都正常").closest("details")!;
    expect([...fine.querySelectorAll("li")].map((li) => li.textContent)).toEqual(TITLES);
    for (const t of TITLES) expect(main().queryByRole("article", { name: t })).not.toBeInTheDocument();
    expect(s.calls.filter((c) => !c.startsWith("GET"))).toEqual([]);
  });

  it("只列要处理的：每条一句中文结论加去处，英文原因不上主界面；正常的折叠", async () => {
    const bad = {
      source_failures: { ok: false, count: 1, severity: "error", why: "a source has consecutive pull failures", detail: [{ source: "mail", enabled: true, consecutive_failures: 5, needs_input_item: "itm_9" }] },
      outcall: { ok: false, count: 2, severity: "error", why: "outcall runs timed out", detail: { long: { running: 1, queued: 3, longest_wait_seconds: 42 }, short: { running: 0, queued: 0, longest_wait_seconds: 0 }, timeouts: ["r1", "r2"] } },
      delivery_backlog: { ok: false, count: 1, severity: "error", why: "deliveries are queued", detail: ["d1"] },
      chain_trips: { ok: false, count: 1, severity: "error", why: "the breaker stopped", detail: ["itm_3"] },
      pack_rejections: { ok: false, count: 1, severity: "error", why: "a pack failed to load", detail: ["pk"], rejections: [{ pack: "pk", source: "user", reason: "band is required" }] },
      model_unavailable: { ok: false, count: 2, severity: "error", why: "x", detail: { judge: "", generate: "not_configured", endpoint: "not_configured", screening: ["x"], judgements: [] } },
      unjudged_backlog: { ok: false, count: 57, severity: "warning", why: "w", detail: { model_unavailable: 50, models_paused: 0, outbound_unset: 3, undecided: 2, restored: 2, expired: 3, sources: [{ source: "mail", count: 57 }] } },
      rule_fallbacks: { ok: false, count: 1, severity: "error", why: "fell back to observe", detail: ["rule-1"] },
      receipt_failures: { ok: false, count: 2, severity: "error", why: "rejected", detail: ["itm_7", "itm_8"] },
      stalled_delegations: { ok: false, count: 1, severity: "warning", why: "stalled", detail: ["itm_5"] },
    };
    mount(serve(() => json(200, report(bad))).impl);
    expect(await screen.findByRole("heading", { level: 1, name: "10 项要处理" })).toBeInTheDocument();
    const src = main().getByRole("article", { name: "来源读取失败" });
    expect(src).toHaveTextContent("mail 连续 5 次没读到");
    expect(within(src).getByRole("link", { name: "去补充" })).toHaveAttribute("href", "/items/itm_9");
    expect(src).not.toHaveTextContent("consecutive pull failures");
    const out = main().getByRole("article", { name: "后台任务排队或超时" });
    expect(out).toHaveTextContent("后台任务超时 2 次");
    expect(out).toHaveTextContent("还有 3 个在排队，最久等了 42 秒");
    expect(within(out).getByRole("link", { name: "去看后台运行" })).toHaveAttribute("href", "/runs");
    expect(within(main().getByRole("article", { name: "提醒发不出去" })).getByRole("link", { name: "去看提醒设置" })).toHaveAttribute("href", "/settings/notify");
    expect(within(main().getByRole("article", { name: "自动化连锁被叫停" })).getByRole("link", { name: "去看事项" })).toHaveAttribute("href", "/items/itm_3");
    const pk = main().getByRole("article", { name: "能力包加载失败" });
    expect(pk).toHaveTextContent("pk");
    expect(pk).not.toHaveTextContent("band is required"); // 加载器的英文原因只在「显示原始值」里
    expect(within(pk).queryByRole("link")).not.toBeInTheDocument();
    const m = main().getByRole("article", { name: "模型连不上" });
    expect(m).toHaveTextContent("写作模型还没配置，有消息在等它");
    expect(m).toHaveTextContent("1 次风险判断没完成");
    expect(within(m).getAllByRole("link", { name: "去看模型" })[0]).toHaveAttribute("href", "/settings/models");
    // 积压按原因分列，各带去处；undecided 按内核口径并进「等模型恢复」
    const b = main().getByRole("article", { name: "积压待处理" });
    expect(b).toHaveTextContent("52 条等模型恢复");
    expect(within(b).getByRole("link", { name: "去选" })).toHaveAttribute("href", "/sources");
    expect(b).toHaveTextContent("3 条等你选外发范围");
    expect(b).toHaveTextContent("2 条已放回，等你放开");
    expect(within(b).getByRole("link", { name: "去放开" })).toHaveAttribute("href", "/sources");
    expect(b).toHaveTextContent("另有 3 条已过期");
    expect(within(main().getByRole("article", { name: "自动规则被退回试运行" })).getByRole("link", { name: "去看规则" })).toHaveAttribute("href", "/rules");
    const rc = main().getByRole("article", { name: "agent 回报失败" });
    expect(within(rc).getByRole("link", { name: "去看操作记录" })).toHaveAttribute("href", "/audit");
    expect(within(rc).getByRole("link", { name: "去看后台权限" })).toHaveAttribute("href", "/settings/access");
    expect(within(main().getByRole("article", { name: "委派久无回音" })).getByRole("link", { name: "去看委派" })).toHaveAttribute("href", "/delegation");
    expect(main().getByText("其余 3 项正常")).toBeInTheDocument();
    expect(main().queryByRole("button", { name: /修复|fix/i })).not.toBeInTheDocument();
  });

  it("模型已暂停：中性的「已暂停」卡，不计入要处理、页头不变成异常；暂停期间积下的条数并在卡里", async () => {
    const r = report({
      model_unavailable: { detail: { judge: "paused", generate: "paused", endpoint: "", paused: true } },
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, rejudge_days: 7, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    expect(await screen.findByRole("heading", { level: 1, name: "模型已暂停" })).toBeInTheDocument();
    const card = main().getByRole("article", { name: "模型已暂停" });
    expect(card.querySelector(".dot")).toHaveClass("paused");
    expect(card).toHaveTextContent("暂停期间积下 2 条 · 恢复后自动判断（最近 7 天的）");
    expect(within(card).getByRole("link", { name: "去恢复" })).toHaveAttribute("href", "/settings/models");
    expect(main().queryByRole("article", { name: "积压待处理" })).not.toBeInTheDocument();
    expect(main().queryByRole("article", { name: "模型连不上" })).not.toBeInTheDocument();
  });

  it("自动重判关闭（rejudge_days 为 0）：暂停卡说恢复后在积压里放开", async () => {
    const r = report({
      model_unavailable: { detail: { judge: "paused", generate: "paused", endpoint: "", paused: true } },
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, rejudge_days: 0, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    await screen.findByRole("heading", { level: 1, name: "模型已暂停" });
    expect(main().getByRole("article", { name: "模型已暂停" })).toHaveTextContent("暂停期间积下 2 条 · 恢复后在积压里放开");
  });

  it.each([
    [7, "2 条是模型暂停期间积下的 · 恢复后自动判断（最近 7 天的）"],
    [0, "2 条是模型暂停期间积下的 · 恢复后在积压里放开"],
    [undefined, "2 条是模型暂停期间积下的"],
  ])("恢复之后：暂停期间积下的仍在积压里列出，去向随 rejudge_days（%s）", async (days, text) => {
    const r = report({
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, rejudge_days: days, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    await screen.findByRole("heading", { level: 1 , name: "1 项要处理" });
    const line = within(main().getByRole("article", { name: "积压待处理" })).getByText(/条是模型暂停期间积下的/);
    expect(line.textContent).toBe(text);
  });

  it.each([
    [7, "3 条等模型恢复 · 恢复后自动判断（最近 7 天的）"],
    [0, "3 条等模型恢复 · 恢复后在积压里放开"],
  ])("v0.1 的 undecided 并进「等模型恢复」，去向随 rejudge_days（%s）", async (days, text) => {
    const r = report({
      unjudged_backlog: { ok: false, count: 3, severity: "warning", why: "w", detail: { model_unavailable: 1, models_paused: 0, outbound_unset: 0, undecided: 2, restored: 0, expired: 0, rejudge_days: days, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    await screen.findByRole("heading", { level: 1, name: "1 项要处理" });
    expect(within(main().getByRole("article", { name: "积压待处理" })).getByText(/条等模型恢复/).textContent).toBe(text);
  });

  it("读取失败：显示服务端原因并可重试", async () => {
    let fail = true;
    mount(serve(() => (fail ? json(500, { code: "internal", why: "boom", next: "retry" }) : json(200, report()))).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("heading", { level: 1, name: "一切正常" })).toBeInTheDocument();
  });

  it("没有读权限（403）：显示权限原因，不显示任何信号", async () => {
    mount(serve(() => json(403, { code: "permission_denied", why: "credential kind cannot read the ledger", next: "use an owner or agent credential" })).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(main().queryByRole("article")).not.toBeInTheDocument();
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
    const sources = within(sec).getByLabelText("来源");
    expect(sources.tagName).toBe("DETAILS");
    expect(sources).not.toHaveAttribute("open");
    expect(sources).toHaveTextContent("2 个来源都正常");
    expect(sources).toHaveTextContent("mail · 上次读取成功");
    expect(sources).toHaveTextContent("src_new · 还没读过");
    expect(s.calls).toContain("GET /v1/doctor");
  });

  it("要处理的展开显示，多处问题逐条说出；英文说明不上主界面", async () => {
    DOCTOR = BAD;
    mount(serve(() => json(200, report())).impl);
    const sources = within(await section()).getByRole("article", { name: "来源" });
    expect(sources).toHaveTextContent("有来源要处理");
    expect(sources).toHaveTextContent("mail · 还没选外发范围，新消息先压着；连续 3 次没读到");
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
