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

const DOCTOR = {
  ok: true,
  checks: [
    { name: "tier", status: "ok", why: "tier 3 (kernel + model endpoint + host)", next: "nothing to do" },
    { name: "sources", status: "warning", why: "some sources need attention; see details", next: "choose the outbound scope", details: [{ subject: "src_unset", status: "warning", why: "the external outbound scope is not chosen" }] },
  ],
};

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

beforeEach(() => sessionStorage.clear());

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
    expect(pk).toHaveTextContent("pk：band is required");
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
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    expect(await screen.findByRole("heading", { level: 1, name: "模型已暂停" })).toBeInTheDocument();
    const card = main().getByRole("article", { name: "模型已暂停" });
    expect(card.querySelector(".dot")).toHaveClass("paused");
    expect(card).toHaveTextContent("暂停期间积下 2 条，恢复后在重判期限内自动判断");
    expect(within(card).getByRole("link", { name: "去恢复" })).toHaveAttribute("href", "/settings/models");
    expect(main().queryByRole("article", { name: "积压待处理" })).not.toBeInTheDocument();
    expect(main().queryByRole("article", { name: "模型连不上" })).not.toBeInTheDocument();
  });

  it("恢复之后：暂停期间积下的仍在积压里列出（按重判期限自动判断）", async () => {
    const r = report({
      unjudged_backlog: { ok: false, count: 2, severity: "warning", why: "w", detail: { model_unavailable: 0, models_paused: 2, outbound_unset: 0, undecided: 0, restored: 0, expired: 0, sources: [] } },
    });
    mount(serve(() => json(200, r)).impl);
    await screen.findByRole("heading", { level: 1 , name: "1 项要处理" });
    expect(main().getByRole("article", { name: "积压待处理" })).toHaveTextContent("2 条是模型暂停期间积下的");
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
  it("收在「自检详情」里，名称是中文，保留服务端的结论、对象与下一步", async () => {
    const s = serve(() => json(200, report()));
    mount(s.impl);
    const sec = (await screen.findByText("自检详情")).closest("details")!;
    expect(within(sec).getByRole("article", { name: "运行形态" })).toHaveTextContent("tier 3");
    const sources = within(sec).getByRole("article", { name: "来源" });
    expect(sources).toHaveTextContent("提醒");
    expect(sources).toHaveTextContent("src_unset");
    expect(sources).toHaveTextContent("choose the outbound scope");
    expect(s.calls).toContain("GET /v1/doctor");
  });
});
