import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/runs（GET /runs?state=&target=、GET /runs/{id}）的契约应答，字段对应设计 §6.11。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const bg = { id: "r1", category: "background", runner: "main_session", launch: "headless", target: "i1", harness: "hermes", session_id: "sess-abc", access: "edit", basis: { policy_version: 7, rule_id: "handle-labelled" }, chain_depth: 1, state: "running", started_at: "2030-01-01T00:00:00Z", timeout: 600 };
const q1 = { id: "r2", category: "task", target: "i2", chain_depth: 0, state: "queued" };
const failed = { id: "r3", category: "source", target: "src1", chain_depth: 0, state: "failed", exit_code: 3, started_at: "2030-01-01T00:00:00Z", finished_at: "2030-01-01T00:01:00Z" };
const ALL = [bg, q1, failed];

function server() {
  const calls: string[] = [];
  const impl = (async (url: string) => {
    calls.push(url);
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items/i1") return json(200, { item: { id: "i1", title: "整理周报" } });
    if (url.startsWith("/v1/items/")) return json(404, { code: "not_found", why: "no", next: "" });
    if (url.startsWith("/v1/runs?")) {
      const p = new URLSearchParams(url.split("?")[1]);
      const state = p.get("state");
      return json(200, { runs: ALL.filter((r) => !state || r.state === state) });
    }
    const m = /^\/v1\/runs\/(.+)$/.exec(url);
    if (m) {
      const r = ALL.find((x) => x.id === m[1]);
      return r ? json(200, r) : json(404, { code: "not_found", why: "no such run", next: "list runs" });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());
afterEach(() => localStorage.removeItem("apa.raw"));

describe("诊断 › 后台运行（flow-bg-main-session、flow-outcall-limits）", () => {
  it("ACT-BGM-03：一行看懂谁在为哪件事跑、用什么权限；会话、发起规则在详情里，策略版本在原始值里", async () => {
    localStorage.setItem("apa.raw", "1");
    mount(server().impl);
    const a = await screen.findByRole("article", { name: "后台运行：r1" });
    expect(within(a).getByText("运行中")).toBeInTheDocument();
    expect(await within(a).findByRole("link", { name: "整理周报" })).toHaveAttribute("href", "/items/i1");
    for (const part of ["主会话", "后台启动", "权限：可改文件"]) expect(a).toHaveTextContent(part);
    expect(a).toHaveTextContent("policy v7");
    await userEvent.click(within(a).getByRole("button", { name: "详情" }));
    expect(await within(a).findByText("会话 sess-abc")).toBeInTheDocument();
    expect(within(a).getByText("由规则「handle-labelled」发起")).toBeInTheDocument();
  });

  it("ACT-OUT-01：超限排队的 Run 显示为排队中，缺省字段不伪造；来源读取显示来源，带用时", async () => {
    mount(server().impl);
    const q = await screen.findByRole("article", { name: "后台运行：r2" });
    expect(within(q).getByText("排队中")).toBeInTheDocument();
    expect(q).not.toHaveTextContent(/会话|权限|用时/);
    const f = screen.getByRole("article", { name: "后台运行：r3" });
    expect(within(f).getByText("失败")).toBeInTheDocument();
    expect(f).toHaveTextContent("读取来源 → src1");
    expect(f).toHaveTextContent("用时 1m");
  });

  it("按状态筛选走服务端 ?state=，且没有重试按钮", async () => {
    const s = server();
    mount(s.impl);
    await screen.findByRole("article", { name: "后台运行：r1" });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "状态" }), "queued");
    await screen.findByRole("article", { name: "后台运行：r2" });
    expect(screen.queryByRole("article", { name: "后台运行：r1" })).not.toBeInTheDocument();
    expect(s.calls).toContain("/v1/runs?limit=200&state=queued");
    expect(screen.queryByRole("button", { name: /重试(?!加载)/ })).not.toBeInTheDocument();
  });

  it("详情：展开时读取 GET /runs/{id}", async () => {
    const s = server();
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "后台运行：r3" });
    await userEvent.click(within(a).getByRole("button", { name: "详情" }));
    expect(await within(a).findByText("退出码 3")).toBeInTheDocument();
    expect(s.calls).toContain("/v1/runs/r3");
    const b = screen.getByRole("article", { name: "后台运行：r1" });
    await userEvent.click(within(b).getByRole("button", { name: "详情" }));
    expect(await within(b).findByText("时限 600 秒")).toBeInTheDocument();
  });

  it("空列表与读取失败（显示服务端原因，可重新加载）", async () => {
    let fail = true;
    const s = server();
    const impl = ((url: string, init?: RequestInit) =>
      url.startsWith("/v1/runs?") ? Promise.resolve(fail ? json(403, { code: "permission_denied", why: "credential kind cannot read the ledger", next: "use an owner credential" }) : json(200, { runs: [] })) : s.impl(url)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("credential kind cannot read the ledger");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重新加载" }));
    expect(await screen.findByText("还没有后台运行")).toBeInTheDocument();
  });
});
