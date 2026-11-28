import { withSession } from "../../app/test-session";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/why（GET /v1/audit/events，序号升序分页）与 api/items（POST /items/{id}/revert）的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface Step { seq: number; type: string; object: string; actor: { type: string; principal: string; via: string; on_behalf_of?: string }; at: string; reason?: string; evidence?: string; resolved_via?: string; grant?: string; payload?: any }
const agent = { type: "agent", principal: "DEL", via: "cli" };
const owner = { type: "human", principal: "owner", via: "web" };
const upd = (seq: number, at: string, over: Partial<Step> = {}): Step => ({ seq, type: "item.updated", object: "i1", actor: agent, at, reason: "改标题", payload: { op: "update", before: { title: "旧标题" }, item: { title: "新标题" } }, ...over });
const dec = (seq: number, at: string): Step => ({ seq, type: "decision.resolved", object: "i2", actor: agent, at, resolved_via: "agent", payload: { item: { title: "要不要上线" } } });

function server(initial: Step[], revert?: (body: any) => Response | undefined) {
  const events = [...initial];
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/settings/commands") return json(200, { declarations: [{ harness: "agent_1", rev: 1, state: "active", declaration: { harness: "agent_1", base: "hermes", name: { zh: "日常", en: "Daily" } } }, { harness: "agent_2", rev: 1, state: "active", declaration: { harness: "agent_2", base: "hermes", name: { zh: "编码", en: "Coding" } } }] });
    if (url === "/v1/settings/declarations") return json(200, { declarations: [] });
    if (url === "/v1/settings/tokens") return json(200, { credentials: [{ id: "DEL", kind: "delegate", harness: "hermes", agent_id: "agent_1" }, { id: "W2", kind: "worker", harness: "hermes", agent_id: "agent_2" }] });
    const q = url.match(/^\/v1\/audit\/events\?after=(\d+)&limit=500$/);
    if (q) return json(200, { events: events.filter((e) => e.seq > +q[1]).slice(0, 500) });
    const r = url.match(/^\/v1\/items\/(.+)\/revert$/);
    if (r && method === "POST") {
      const res = revert?.(body);
      if (res) return res;
      const orig = events.find((e) => e.seq === body.event)!;
      events.push({ seq: events.length + 1, type: orig.type, object: r[1], actor: owner, at: "2030-01-02T00:00:00Z", payload: { op: "revert", reverts: body.event } });
      return json(200, { id: r[1] });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch, at = "/agent-changes") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  sessionStorage.clear();
  localStorage.clear();
});

describe("agent 变更视图（flow-agent-revert）", () => {
  it("ACT-REV-01：列出 agent 的标题修改与它替你答的问题，带操作者与说明；人的操作不在列表", async () => {
    const s = server([upd(1, "2030-01-01T00:00:00Z"), dec(2, "2030-01-01T00:01:00Z"), upd(3, "2030-01-01T00:02:00Z", { actor: owner, reason: "人改的" }), upd(4, "2030-01-01T00:03:00Z", { type: "command.rejected", reason: "被拒" })]);
    mount(s.impl);
    const a = (await screen.findByRole("heading", { name: /新标题/ })).closest("article")!;
    // 操作者显示 Agent 名；名字表（/settings/tokens + 声明）可能晚于事件列表到，等名字出现再断言，不依赖先后
    expect(await within(a).findByText(/日常/)).toBeInTheDocument();
    expect(within(a).getByText("改标题")).toBeInTheDocument();
    const d = screen.getByRole("heading", { name: "Agent 替你答了「要不要上线」" }).closest("article")!;
    expect(within(d).getByRole("button", { name: "撤销" })).toBeEnabled(); // Agent 自答可按次撤回（设计 §14.3）
    expect(within(d).queryByText(/已拉起的运行撤不回/)).toBeNull();
    expect(screen.queryByText("人改的")).not.toBeInTheDocument();
    expect(screen.queryByText("被拒")).not.toBeInTheDocument();
    expect(screen.getAllByRole("article")).toHaveLength(2);
  });

  it("ACT-REV-01：自上次访问以来——只显示上次离开页面之后的变更，可展开更早的", async () => {
    localStorage.setItem("apa.agentChanges.lastVisit", "2030-01-01T00:01:30Z");
    const s = server([upd(1, "2030-01-01T00:00:00Z", { reason: "早的" }), dec(2, "2030-01-01T00:01:00Z"), upd(3, "2030-01-01T00:02:00Z", { reason: "新的" })]);
    mount(s.impl);
    await screen.findByText("新的");
    expect(screen.getAllByRole("article")).toHaveLength(1);
    expect(screen.queryByText("早的")).not.toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("显示更早的改动"));
    expect(screen.getAllByRole("article")).toHaveLength(3);
  });

  it("离开页面记下访问时间，下次进入以它为界", async () => {
    const before = Date.now();
    const { unmount } = mount(server([upd(1, "2030-01-01T00:00:00Z")]).impl);
    await screen.findByRole("heading", { name: /新标题/ });
    expect(localStorage.getItem("apa.agentChanges.lastVisit")).toBeNull();
    unmount();
    expect(Date.parse(localStorage.getItem("apa.agentChanges.lastVisit")!)).toBeGreaterThanOrEqual(before);
  });

  it("ACT-REV-02：撤销（先确认）→ POST revert 带 CSRF 与事件序号；原事件仍在，标为已撤销；提示外部副作用不撤销", async () => {
    const s = server([upd(1, "2030-01-01T00:00:00Z")]);
    mount(s.impl);
    const a = (await screen.findByRole("heading", { name: /新标题/ })).closest("article")!;
    await userEvent.click(within(a).getByRole("button", { name: "撤销" }));
    expect(within(a).getByText(/已发出的消息、已提的 PR 不会收回/)).toBeInTheDocument();
    expect(s.calls.some((c) => c.method === "POST")).toBe(false); // 先确认，才发撤销
    await userEvent.click(within(a).getByRole("button", { name: "确认撤销" }));
    expect(await within(screen.getByRole("heading", { name: /新标题/ }).closest("article")!).findByText(/已撤销/)).toBeInTheDocument();
    const w = s.calls.find((c) => c.method === "POST")!;
    expect(w.path).toBe("/v1/items/i1/revert");
    expect(w.body).toEqual({ event: 1 });
    expect(w.csrf).toBe("tok-1");
    expect(screen.getByRole("heading", { name: /新标题/ }).closest("article")!.querySelector("button")).toBeNull();
  });

  it("撤销被拒（后面还有别的改动）：显示服务端原因，事件保持可撤销", async () => {
    const s = server([upd(1, "2030-01-01T00:00:00Z")], () => json(409, { code: "conflict", why: "later changes exist on this item", next: "revert the later changes first" }));
    mount(s.impl);
    const a = (await screen.findByRole("heading", { name: /新标题/ })).closest("article")!;
    await userEvent.click(within(a).getByRole("button", { name: "撤销" }));
    await userEvent.click(within(a).getByRole("button", { name: "确认撤销" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("later changes exist");
    expect(within(a).getByRole("button", { name: "撤销" })).toBeEnabled();
  });

  it("空列表", async () => {
    mount(server([]).impl);
    expect(await screen.findByText("Agent 还没改过什么")).toBeInTheDocument();
  });

  it("读取失败：显示错误并可重试", async () => {
    let fail = true;
    const s = server([upd(1, "2030-01-01T00:00:00Z")]);
    const impl = ((url: string, init?: RequestInit) =>
      fail && url.startsWith("/v1/audit") ? Promise.resolve(json(500, { code: "internal", why: "boom", next: "retry" })) : s.impl(url, init)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("heading", { name: /新标题/ })).toBeInTheDocument();
  });

  it("多页事件：按 after 游标翻页取全", async () => {
    const many = Array.from({ length: 501 }, (_, i) => upd(i + 1, "2030-01-01T00:00:00Z", { object: "i" + i, reason: "r" + i }));
    const s = server(many);
    mount(s.impl);
    await screen.findByText("r500");
    expect(s.calls.filter((c) => c.path.startsWith("/v1/audit")).map((c) => c.path)).toEqual(["/v1/audit/events?after=0&limit=500", "/v1/audit/events?after=500&limit=500"]);
  });
});

describe("Agent 自答之后续接了运行；切换前的旧记录", () => {
  it("回答后为这件事续接了运行：行上写明已拉起的运行撤不回，回答仍可撤销", async () => {
    const resumed: Step = { seq: 3, type: "resume.launched", object: "i2", actor: { type: "system", principal: "rule:x", via: "rule" }, at: "2030-01-01T00:02:00Z" };
    const s = server([dec(2, "2030-01-01T00:01:00Z"), resumed]);
    mount(s.impl);
    const d = (await screen.findByRole("heading", { name: "Agent 替你答了「要不要上线」" })).closest("article")!;
    expect(within(d).getByText(/已拉起的运行撤不回/)).toBeInTheDocument();
    expect(within(d).getByRole("button", { name: "撤销" })).toBeEnabled();
  });

  it("切换前凭授权单或原话的记录标为旧模型记录，仍可撤销", async () => {
    const old: Step = { seq: 1, type: "decision.resolved", object: "i3", actor: { ...agent, on_behalf_of: "owner" }, at: "2030-01-01T00:05:00Z", resolved_via: "grant", grant: "grt_ok", payload: { item: { title: "要不要发布" } } };
    const s = server([old]);
    mount(s.impl);
    const a = (await screen.findByRole("heading", { name: /要不要发布/ })).closest("article")!;
    expect(within(a).getByText(/旧模型记录/)).toBeInTheDocument();
    expect(within(a).queryByRole("link")).toBeNull();
    await userEvent.click(within(a).getByRole("button", { name: "撤销" }));
    await userEvent.click(within(a).getByRole("button", { name: "确认撤销" }));
    expect(s.calls.find((c) => c.method === "POST")).toMatchObject({ path: "/v1/items/i3/revert", body: { event: 1 }, csrf: "tok-1" });
  });
});

describe("起草建议失败（APA 调模型出错，不是 Agent 后台运行）", () => {
  const apa = { type: "agent", principal: "kernel:inbound", via: "source" };
  const run = (seq: number, payload: unknown): Step => ({ seq, type: "run.finished", object: "run_x", actor: apa, at: "2030-01-01T00:00:00Z", reason: "generate failed", payload });

  it("说成起草建议失败，操作者是 APA 自己，写明不是 Agent 后台运行；别的运行失败仍按原样", async () => {
    const s = server([run(1, { category: "generate", template: "triage-draft", state: "failed", error: "schema" }), run(2, { category: "background", template: "t", state: "failed" })]);
    mount(s.impl);
    const a = (await screen.findByRole("heading", { name: "起草建议失败" })).closest("article")!;
    expect(within(a).getByText(/APA 自己/)).toBeInTheDocument();
    expect(within(a).getByText(/不是 Agent 后台运行/)).toBeInTheDocument();
    expect(within(a).queryByText("generate failed")).not.toBeInTheDocument();
    expect(within(a).queryByText(/kernel:inbound/)).not.toBeInTheDocument();
    const other = screen.getByRole("heading", { name: /Agent 后台运行结束/ }).closest("article")!;
    expect(within(other).getByText("generate failed")).toBeInTheDocument();
  });

  it("从 Agent 页带 ?agent= 进来只列这个 Agent 的钥匙做的改动；可换成全部", async () => {
    const other = { type: "agent", principal: "W2", via: "cli" };
    mount(server([upd(1, "2030-01-01T00:00:00Z"), upd(2, "2030-01-01T00:01:00Z", { actor: other, payload: { op: "update", item: { title: "别人改的" } } })]).impl, "/agent-changes?agent=agent_2");
    expect(await screen.findByRole("heading", { name: /别人改的/ })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /新标题/ })).not.toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("哪个 Agent"), "");
    expect(await screen.findByRole("heading", { name: /新标题/ })).toBeInTheDocument();
  });
});
