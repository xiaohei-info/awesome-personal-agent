import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import type { HealthReport, Signal } from "./HealthPage";

// 测试边界替身：按 api/health 的契约应答 GET /v1/health（十一项信号、顺序固定，错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const NEXT: Record<string, string> = {
  source_failures: "source page; auth failures have a needs_input item",
  outcall: "run detail; if queues stay backed up, raise the matching quota in run settings",
  delivery_backlog: "channel ext and interruption policy",
  chain_trips: "item detail; re-delegate by hand",
  agentsmd_drift: "project page",
  pack_rejections: "fix the pack, then hot-reload",
  model_unavailable: "settings page: model endpoint",
  unjudged_backlog: "source page: propose or ignore, one by one or per source",
  rule_fallbacks: "re-review the rule in Web",
  receipt_failures: "audit page; credential or basis problems go to settings, a receipt turned needs_input is handled in decision todos",
  other_alerts: "audit page; open the object named in detail",
};
const ORDER = Object.keys(NEXT);

const DOCTOR = {
  ok: true,
  checks: [
    { name: "tier", status: "ok", why: "tier 3 (kernel + model endpoint + host)", next: "nothing to do" },
    { name: "sources", status: "warning", why: "some sources need attention; see details", next: "choose the outbound scope", details: [{ subject: "src_unset", status: "warning", why: "the external outbound scope is not chosen" }] },
  ],
};

function report(bad: Record<string, Partial<Signal>> = {}): HealthReport {
  const signals = ORDER.map((id) => ({ id, ok: true, count: 0, next: NEXT[id], ...bad[id] }) as Signal);
  return { healthy: signals.every((s) => s.ok), signals };
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
    if (url === "/v1/health") return get();
    if (url === "/v1/doctor") return json(200, DOCTOR);
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

beforeEach(() => sessionStorage.clear());

describe("健康页（flow-health / ACT-HLT-01）", () => {
  it("全部正常：十一类信号都显示且顺序与服务端一致，且不出现修复链接以外的写请求", async () => {
    const s = serve(() => json(200, report()));
    mount(s.impl);
    expect(await screen.findByText("一切正常")).toBeInTheDocument();
    for (const t of ["来源失败", "外呼超时与排队", "投递积压", "链路熔断", "AGENTS.md 漂移", "能力包加载失败", "模型不可用", "未判定积压", "规则回退 observe", "回执与捕获失败", "其他健康告警"])
      expect(screen.getByRole("article", { name: t })).toBeInTheDocument();
    expect(s.calls.filter((c) => !c.startsWith("GET"))).toEqual([]);
  });

  it("异常信号显示数量、原因与来自接口的修复入口，并链接到对应页面；不提供修复按钮", async () => {
    const bad = {
      source_failures: { ok: false, count: 1, why: "a source has consecutive pull failures", detail: [{ source: "mail", enabled: false, consecutive_failures: 5, needs_input_item: "it-9" }] },
      outcall: { ok: false, count: 2, why: "outcall runs timed out", detail: { long: { running: 1, queued: 3, longest_wait_seconds: 42 }, short: { running: 0, queued: 0, longest_wait_seconds: 0 }, timeouts: ["r1", "r2"] } },
      delivery_backlog: { ok: false, count: 1, why: "deliveries are queued", detail: ["d1"] },
      chain_trips: { ok: false, count: 1, why: "the breaker stopped", detail: ["it-3"] },
      agentsmd_drift: { ok: false, count: 1, why: "drift", detail: ["it-4"] },
      pack_rejections: { ok: false, count: 1, why: "a pack failed to load", detail: ["pk"] },
      model_unavailable: { ok: false, count: 2, severity: "error", why: "a judge run failed", detail: { endpoint: "not_configured", screening: ["x"], judgements: [] } },
      unjudged_backlog: { ok: false, count: 54, severity: "warning", why: "source events are still waiting for a judgement", detail: { model_unavailable: 50, undecided: 4, expired: 3, sources: [{ source: "mail", count: 54 }] } },
      rule_fallbacks: { ok: false, count: 1, why: "fell back to observe", detail: ["rule-1"] },
      receipt_failures: { ok: false, count: 2, why: "a receipt or capture was rejected by the kernel", detail: ["it-7", "it-8"] },
      other_alerts: { ok: false, count: 1, why: "a health alert without a dedicated signal", detail: ["rule-2"] },
    };
    mount(serve(() => json(200, report(bad))).impl);
    const src = await screen.findByRole("article", { name: "来源失败" });
    expect(within(src).getByText(/a source has consecutive pull failures/)).toBeInTheDocument();
    expect(within(src).getByText(/source page; auth failures/)).toBeInTheDocument();
    expect(within(src).getByText(/mail/)).toBeInTheDocument();
    expect(within(src).getByRole("link", { name: "来源" })).toHaveAttribute("href", "/sources");
    const out = screen.getByRole("article", { name: "外呼超时与排队" });
    expect(within(out).getByText(/长时运行：运行 1，排队 3，最长等待 42 秒/)).toBeInTheDocument();
    expect(within(out).getByRole("link", { name: "运行记录" })).toHaveAttribute("href", "/runs");
    expect(within(screen.getByRole("article", { name: "投递积压" })).getByRole("link", { name: "策略与人格" })).toHaveAttribute("href", "/settings");
    expect(within(screen.getByRole("article", { name: "链路熔断" })).getByRole("link", { name: "事项" })).toHaveAttribute("href", "/items");
    expect(within(screen.getByRole("article", { name: "AGENTS.md 漂移" })).getByRole("link", { name: "项目" })).toHaveAttribute("href", "/projects");
    expect(within(screen.getByRole("article", { name: "能力包加载失败" })).queryByRole("link")).not.toBeInTheDocument();
    const m = screen.getByRole("article", { name: "模型不可用" });
    expect(within(m).getByText("异常（2）")).toBeInTheDocument();
    expect(within(m).getByText(/模型端点没有配置/)).toBeInTheDocument();
    expect(within(m).getByRole("link", { name: "模型" })).toHaveAttribute("href", "/settings/models");
    // 积压是提醒级：显示待处理而不是异常，不计入异常类数，状态值不露出内部值
    const b = screen.getByRole("article", { name: "未判定积压" });
    expect(within(b).getByText("待处理（54）")).toBeInTheDocument();
    expect(within(b).getByText(/等模型恢复后自动重判：50 条/)).toBeInTheDocument();
    expect(within(b).getByText(/另有 3 条已过期/)).toBeInTheDocument();
    expect(within(b).queryByText(/unjudged/)).not.toBeInTheDocument();
    expect(within(b).getByRole("link", { name: "来源" })).toHaveAttribute("href", "/sources");
    expect(within(screen.getByRole("article", { name: "规则回退 observe" })).getByRole("link", { name: "规则模式" })).toHaveAttribute("href", "/rules");
    const rc = screen.getByRole("article", { name: "回执与捕获失败" });
    expect(within(rc).getByText(/it-7、it-8/)).toBeInTheDocument();
    expect(within(rc).getByRole("link", { name: "审计" })).toHaveAttribute("href", "/audit");
    expect(within(rc).getByRole("link", { name: "决策待办" })).toHaveAttribute("href", "/decisions");
    expect(within(screen.getByRole("article", { name: "其他健康告警" })).getByRole("link", { name: "审计" })).toHaveAttribute("href", "/audit");
    expect(screen.queryByText("一切正常")).not.toBeInTheDocument();
    expect(screen.getByText("存在 10 类异常")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /修复|重试|fix/i })).not.toBeInTheDocument();
  });

  it("读取失败：显示服务端原因并可重试", async () => {
    let fail = true;
    mount(serve(() => (fail ? json(500, { code: "internal", why: "boom", next: "retry" }) : json(200, report()))).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("一切正常")).toBeInTheDocument();
  });

  it("owner/agent 以外的凭据被拒（403）：显示权限原因，不显示任何信号", async () => {
    mount(serve(() => json(403, { code: "permission_denied", why: "credential kind cannot read the ledger", next: "use an owner or agent credential" })).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot read the ledger");
    expect(screen.queryByRole("article")).not.toBeInTheDocument();
  });
});

describe("健康页的自检区（flow-doctor / ACT-DOC-12）", () => {
  it("展示 GET /v1/doctor 的每项结论、对象与下一步", async () => {
    const s = serve(() => json(200, report()));
    mount(s.impl);
    const sec = await screen.findByRole("region", { name: "自检" });
    expect(within(sec).getByRole("article", { name: "自检：tier" })).toHaveTextContent("tier 3");
    const sources = within(sec).getByRole("article", { name: "自检：sources" });
    expect(sources).toHaveTextContent("提醒");
    expect(sources).toHaveTextContent("src_unset");
    expect(sources).toHaveTextContent("下一步：choose the outbound scope");
    expect(s.calls).toContain("GET /v1/doctor");
  });
});
