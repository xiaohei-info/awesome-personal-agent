import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/delegation（GET /executors、POST /delegations）、api/items（GET /items、PATCH /items/{id}）、
// api/why（GET /audit/events）的契约应答；名单校验由替身按 §14.7 模拟服务端，页面本身不判定。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface It { id: string; phase: string; version: number; kind: string; title: string; status_label?: string; project?: string; assignee?: string }
const work: It = { id: "c1", phase: "open", version: 3, kind: "delegation", title: "写报告", status_label: "写到一半", project: "p_work", assignee: "work-host.cli" };
const fin: It = { id: "c2", phase: "done", version: 5, kind: "delegation", title: "已交付", status_label: "已合并", project: "p_work", assignee: "work-host.cli" };
const report = (seq: number, object: string, outcome: string, summary: string, evidence: object) => ({ seq, type: "delegation.reported", object, at: "2030-01-01T00:00:00Z", payload: { report: { outcome, summary, evidence } } });
const EXECS = [{ id: "work-host.cli", kind: "cli" }, { id: "home.cli", kind: "cli" }];
const ALLOWED = ["work-host.cli", "work-host.alt"];

function server(items: It[], events: unknown[], stalled: string[] = []) {
  const calls: { method: string; path: string; body?: any; headers: Record<string, string> }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, headers: (init.headers ?? {}) as Record<string, string> });
    if (url === "/v1/projects") return json(200, { projects: [{ id: "p_work", name: "工作" }] });
    if (url === "/v1/settings/executors") return json(200, { executors: [{ id: "work-host.cli", kind: "external-cli" }] });
    if (url === "/v1/health") return json(200, { healthy: !stalled.length, signals: [{ id: "stalled_delegations", ok: !stalled.length, detail: stalled }] });
    if (url === "/v1/items?kind=delegation") return json(200, { items });
    if (url.startsWith("/v1/audit/events?type=delegation.reported&after=")) return json(200, { events });
    if (url === "/v1/executors?project=p_work") return json(200, { executors: [{ id: "work-host.cli", kind: "external-cli", allowed: true }, { id: "home.cli", kind: "external-cli", allowed: false }] });
    if (url === "/v1/items/c1" && method === "PATCH") {
      if (!ALLOWED.includes(body.assignee)) return json(403, { code: "executor_not_allowed", why: `executor ${body.assignee} is not allowed in project p_work`, next: "pick an allowed executor", allowed: ALLOWED });
      items[0] = { ...items[0], assignee: body.assignee, version: items[0].version + 1 };
      return json(200, { id: "c1" });
    }
    if (url === "/v1/items/c2") return json(200, { item: fin, relations: [{ type: "parent", from: "c2", to: "root" }] });
    if (url === "/v1/delegations" && method === "POST") {
      items.push({ id: "c3", phase: "open", version: 1, kind: "delegation", title: body.title, project: "p_work", assignee: body.assignee });
      return json(201, { id: "c3", parent: body.parent });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/delegation"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const card = (name: string) => screen.getByRole("article", { name });

beforeEach(() => sessionStorage.clear());

describe("任务委派页（flow-delegate-recover）", () => {
  it("ACT-DLG-10：显示谁在干、干到哪、证据、状态；同一子事项取最新回执", async () => {
    const s = server([work, fin], [
      report(1, "c1", "progress", "旧", { branch: "old" }),
      report(2, "c1", "progress", "写到一半", { branch: "feat/report", pr: "https://x/pr/9" }),
      report(3, "c2", "done", "已合并", { commit: "abc123" }),
    ]);
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "写报告" });
    expect(within(a).getByText("work-host.cli")).toBeInTheDocument();
    expect(within(a).getByText(/进行中/)).toBeInTheDocument();
    expect(within(a).getByText(/写到一半/)).toBeInTheDocument();
    expect(within(a).getByText(/feat\/report/)).toBeInTheDocument();
    expect(within(a).getByText(/https:\/\/x\/pr\/9/)).toBeInTheDocument();
    expect(within(a).queryByText(/old/)).not.toBeInTheDocument();
    const b = card("已交付");
    expect(within(b).getByText(/已完成/)).toBeInTheDocument();
    expect(within(b).getByText(/abc123/)).toBeInTheDocument();
  });

  it("执行者带种类、项目显示名称；健康信号里久无回音的委派标出来", async () => {
    mount(server([work, fin], [], ["c1"]).impl);
    const a = await screen.findByRole("article", { name: "写报告" });
    expect(await within(a).findByText("久无回音")).toBeInTheDocument();
    expect(within(a).getByText(/外部命令行工具/)).toBeInTheDocument();
    expect(within(a).getByText(/项目 工作/)).toBeInTheDocument();
    expect(within(card("已交付")).queryByText("久无回音")).not.toBeInTheDocument();
  });

  it("ACT-DLG-10：本人改派到允许的执行者 → PATCH 带 If-Match 与 CSRF，列表刷新", async () => {
    const s = server([{ ...work }], []);
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "写报告" });
    await userEvent.click(within(a).getByRole("button", { name: "改派" }));
    const sel = await within(a).findByRole("combobox", { name: "目标执行者" });
    expect(within(sel).getByRole("option", { name: "home.cli · 外部命令行工具（项目不允许）" })).toBeInTheDocument();
    await userEvent.selectOptions(sel, "work-host.cli");
    await userEvent.click(within(a).getByRole("button", { name: "确认改派" }));
    const w = s.calls.find((c) => c.method === "PATCH")!;
    expect(w.path).toBe("/v1/items/c1");
    expect(w.body.assignee).toBe("work-host.cli");
    expect(w.headers["If-Match"]).toBe("3");
    expect(w.headers["X-CSRF-Token"]).toBe("tok-1");
    expect(s.calls.filter((c) => c.path === "/v1/items?kind=delegation")).toHaveLength(2);
  });

  it("ACT-ALW-05：改派到名单外 → 服务端拒绝，页面显示原因与允许的执行者，执行者不变", async () => {
    const s = server([{ ...work }], []);
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "写报告" });
    await userEvent.click(within(a).getByRole("button", { name: "改派" }));
    await userEvent.selectOptions(await within(a).findByRole("combobox", { name: "目标执行者" }), "home.cli");
    await userEvent.click(within(a).getByRole("button", { name: "确认改派" }));
    const alert = await within(a).findByRole("alert");
    expect(alert).toHaveTextContent("not allowed in project p_work");
    expect(alert).toHaveTextContent("work-host.cli、work-host.alt");
    expect(a.querySelector("strong")).toHaveTextContent("work-host.cli");
  });

  it("重新委派：已完成的子事项按父事项重新 POST /delegations", async () => {
    const s = server([work, fin], []);
    mount(s.impl);
    const b = await screen.findByRole("article", { name: "已交付" });
    await userEvent.click(within(b).getByRole("button", { name: "重新委派" }));
    await userEvent.selectOptions(await within(b).findByRole("combobox", { name: "目标执行者" }), "work-host.cli");
    await userEvent.click(within(b).getByRole("button", { name: "确认重新委派" }));
    await screen.findAllByRole("article", { name: "已交付" });
    const w = s.calls.find((c) => c.method === "POST" && c.path === "/v1/delegations")!;
    expect(w.body).toMatchObject({ parent: "root", assignee: "work-host.cli", title: "已交付" });
    expect(w.headers["X-CSRF-Token"]).toBe("tok-1");
    expect(screen.getAllByRole("article", { name: "已交付" })).toHaveLength(2);
  });

  it("空列表与读取失败可重试", async () => {
    mount(server([], []).impl);
    expect(await screen.findByText("还没有派出的活")).toBeInTheDocument();
  });

  it("读取失败：显示错误并可重试", async () => {
    let fail = true;
    const s = server([work], []);
    const impl = ((url: string, init?: RequestInit) =>
      fail && url.startsWith("/v1/items?") ? Promise.resolve(json(500, { code: "internal", why: "boom", next: "retry" })) : s.impl(url, init)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("article", { name: "写报告" })).toBeInTheDocument();
  });
});
