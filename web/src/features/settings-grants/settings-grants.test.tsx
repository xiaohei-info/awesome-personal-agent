import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { GRANTABLE_ACTIONS } from "./SettingsGrantsPage";

// 测试边界替身：按 api/settings/grants 与 /audit/events?grant= 的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const DAY = 86400_000;

interface Grant {
  id: string; principal: string; grantee: { credential: string; harness?: string };
  scope: { projects: string[]; kinds: string[]; items: string[] }; actions: string[]; risk_ceiling: string;
  expires_at: string; reason?: string; created_at: string; revoked_at?: string; revoked_reason?: string;
}
const grant = (id: string, over: Partial<Grant> = {}): Grant => ({
  id, principal: "me", grantee: { credential: "tok_d1" }, scope: { projects: ["p1"], kinds: [], items: [] },
  actions: ["decision.answer"], risk_ceiling: "normal", expires_at: new Date(Date.now() + 3 * DAY).toISOString(),
  created_at: "2026-01-01T00:00:00Z", ...over,
});
const EVENTS = [
  { seq: 7, type: "grant.issued", object: "grt_a", actor: { type: "human", principal: "me", via: "web" }, at: "2026-01-01T00:00:00Z", reason: "away" },
  { seq: 9, type: "item.updated", object: "it_1", actor: { type: "agent", principal: "tok_d1", via: "api" }, at: "2026-01-02T00:00:00Z" },
];

function server(initial: Grant[] = [], opt: { denyIssue?: Response; denyRevoke?: Response } = {}) {
  let grants = initial;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/settings/tokens") {
      return json(200, { credentials: [
        { id: "tok_d1", kind: "delegate", label: "main", created_at: "x" },
        { id: "tok_d2", kind: "delegate", label: "old", created_at: "x", revoked_at: "y" },
        { id: "tok_o1", kind: "observer", label: "obs", created_at: "x" },
      ] });
    }
    if (url === "/v1/settings/grants" && method === "GET") return json(200, { grants });
    if (url === "/v1/settings/grants" && method === "POST") {
      if (opt.denyIssue) return opt.denyIssue.clone();
      const g = grant("grt_new", { grantee: body.grantee, scope: body.scope, actions: body.actions, expires_at: body.expires_at, reason: body.reason });
      grants = [...grants, g];
      return json(201, g);
    }
    const rv = url.match(/^\/v1\/settings\/grants\/(.+)\/revoke$/);
    if (rv && method === "POST") {
      if (opt.denyRevoke) return opt.denyRevoke.clone();
      const g = grants.find((x) => x.id === rv[1]);
      if (!g) return json(404, { code: "not_found", why: "grant not found", next: "x" });
      g.revoked_at = "2026-01-03T00:00:00Z";
      g.revoked_reason = body.reason;
      return json(200, g);
    }
    if (url.startsWith("/v1/audit/events")) return json(200, { events: EVENTS });
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/grants"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const post = (s: ReturnType<typeof server>) => s.calls.find((c) => c.method === "POST" && c.path === "/v1/settings/grants")!;

beforeEach(() => sessionStorage.clear());

describe("授权单页（flow-authz-matrix）", () => {
  it("路由 settings/grants；动作只列附录 B.1 可用授权单一列，没有全选与 web_only/管理类命令", async () => {
    mount(server().impl);
    const form = await screen.findByRole("form", { name: "签发授权单" });
    const boxes = within(form).getAllByRole("checkbox").map((b) => (b as HTMLInputElement).value);
    expect(boxes.sort()).toEqual([...GRANTABLE_ACTIONS].sort());
    expect(boxes).not.toContain("project.create");
    expect(within(form).queryByLabelText(/全选|\*/)).not.toBeInTheDocument();
    expect(within(form).getAllByRole("checkbox").every((b) => !(b as HTMLInputElement).checked)).toBe(true);
  });

  it("签发：默认 7 天；被授权方只列未吊销的 delegate；POST 带 CSRF、范围与逐条勾选的动作", async () => {
    const s = server();
    mount(s.impl);
    const form = await screen.findByRole("form", { name: "签发授权单" });
    const cred = within(form).getByLabelText("被授权凭据");
    await waitFor(() => expect(within(cred).getAllByRole("option").map((o) => o.textContent)).toEqual(expect.arrayContaining(["main（tok_d1）"])));
    expect(within(cred).queryByText(/tok_d2|tok_o1/)).not.toBeInTheDocument();
    expect(within(form).getByLabelText("有效天数")).toHaveValue(7);

    await userEvent.selectOptions(cred, "tok_d1");
    await userEvent.type(within(form).getByLabelText("项目范围"), "p1, p2");
    await userEvent.type(within(form).getByLabelText("事项范围"), "it_1");
    await userEvent.type(within(form).getByLabelText("原因"), "away");
    await userEvent.click(within(form).getByLabelText("decision.answer"));
    await userEvent.click(within(form).getByLabelText("proposal.accept"));
    const before = Date.now();
    await userEvent.click(within(form).getByRole("button", { name: "签发" }));

    await screen.findByText("grt_new");
    const w = post(s);
    expect(w.csrf).toBe("tok-1");
    expect(w.body.grantee).toEqual({ credential: "tok_d1" });
    expect(w.body.scope).toEqual({ projects: ["p1", "p2"], kinds: [], items: ["it_1"] });
    expect(w.body.actions.sort()).toEqual(["decision.answer", "proposal.accept"]);
    expect(w.body.reason).toBe("away");
    const ttl = Date.parse(w.body.expires_at) - before;
    expect(ttl).toBeGreaterThan(7 * DAY - 60_000);
    expect(ttl).toBeLessThan(7 * DAY + 60_000);
  });

  it("有效期最长 90 天：输入框上限为 90，90 天照实发送", async () => {
    const s = server();
    mount(s.impl);
    const form = await screen.findByRole("form", { name: "签发授权单" });
    const days = within(form).getByLabelText("有效天数");
    expect(days).toHaveAttribute("max", "90");
    await waitFor(() => expect(within(form).getByLabelText("被授权凭据")).toHaveValue("tok_d1"));
    await userEvent.clear(days);
    await userEvent.type(days, "90");
    await userEvent.type(within(form).getByLabelText("项目范围"), "p1");
    await userEvent.click(within(form).getByLabelText("item.update"));
    await userEvent.click(within(form).getByRole("button", { name: "签发" }));
    await screen.findByText("grt_new");
    const ttl = Date.parse(post(s).body.expires_at) - Date.now();
    expect(ttl).toBeGreaterThan(90 * DAY - 120_000);
    expect(ttl).toBeLessThanOrEqual(90 * DAY);
  });

  it("服务端拒绝签发（如超过 90 天）：展示服务端原因，不出现新授权单", async () => {
    const s = server([], { denyIssue: json(400, { code: "invalid_request", why: "expires_at must be in the future and within 90 days", next: "fix the grant and send it again; nothing was saved" }) });
    mount(s.impl);
    const form = await screen.findByRole("form", { name: "签发授权单" });
    await waitFor(() => expect(within(form).getByLabelText("被授权凭据")).toHaveValue("tok_d1"));
    await userEvent.clear(within(form).getByLabelText("有效天数"));
    await userEvent.type(within(form).getByLabelText("有效天数"), "400");
    await userEvent.type(within(form).getByLabelText("项目范围"), "p1");
    await userEvent.click(within(form).getByLabelText("item.update"));
    await userEvent.click(within(form).getByRole("button", { name: "签发" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("within 90 days");
    expect(screen.queryByText("grt_new")).not.toBeInTheDocument();
  });

  it("列表区分有效、已撤销（含原因）、已过期；已撤销与已过期没有撤销按钮；页面没有编辑或延长", async () => {
    mount(server([
      grant("grt_ok"),
      grant("grt_rev", { revoked_at: "2026-01-03T00:00:00Z", revoked_reason: "back home" }),
      grant("grt_exp", { expires_at: new Date(Date.now() - DAY).toISOString() }),
    ]).impl);
    const li = async (id: string) => (await screen.findByText(id)).closest("li")!;
    expect(await li("grt_ok")).toHaveTextContent("有效");
    expect(await li("grt_ok")).not.toHaveTextContent(/已撤销|已过期/);
    expect(await li("grt_rev")).toHaveTextContent("已撤销");
    expect(await li("grt_rev")).toHaveTextContent("back home");
    expect(await li("grt_exp")).toHaveTextContent("已过期");
    expect(within(await li("grt_ok")).getByRole("button", { name: "撤销" })).toBeInTheDocument();
    expect(within(await li("grt_rev")).queryByRole("button", { name: "撤销" })).not.toBeInTheDocument();
    expect(within(await li("grt_exp")).queryByRole("button", { name: "撤销" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /编辑|延长/ })).not.toBeInTheDocument();
  });

  it("撤销必须填原因 → POST /revoke 带 CSRF 与原因，随后列表显示已撤销", async () => {
    const s = server([grant("grt_ok")]);
    mount(s.impl);
    const item = (await screen.findByText("grt_ok")).closest("li")!;
    const btn = within(item).getByRole("button", { name: "撤销" });
    expect(btn).toBeDisabled();
    await userEvent.type(within(item).getByLabelText("撤销原因"), "back home");
    await userEvent.click(btn);
    await waitFor(() => expect(screen.getByText("grt_ok").closest("li")).toHaveTextContent("已撤销"));
    const w = s.calls.find((c) => c.path === "/v1/settings/grants/grt_ok/revoke")!;
    expect(w.method).toBe("POST");
    expect(w.csrf).toBe("tok-1");
    expect(w.body).toEqual({ reason: "back home" });
    expect(screen.getByText("grt_ok").closest("li")).toHaveTextContent("back home");
  });

  it("撤销被服务端拒绝（404）：展示服务端原因，授权单仍按服务端状态显示", async () => {
    const s = server([grant("grt_ok")], { denyRevoke: json(404, { code: "not_found", why: "grant not found", next: "reload" }) });
    mount(s.impl);
    const item = (await screen.findByText("grt_ok")).closest("li")!;
    await userEvent.type(within(item).getByLabelText("撤销原因"), "x");
    await userEvent.click(within(item).getByRole("button", { name: "撤销" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("grant not found");
    expect(screen.getByText("grt_ok").closest("li")).not.toHaveTextContent("已撤销");
  });

  it("使用记录：按 grant=<id> 查审计事件并列出签发、使用", async () => {
    const s = server([grant("grt_a")]);
    mount(s.impl);
    const item = (await screen.findByText("grt_a")).closest("li")!;
    await userEvent.click(within(item).getByRole("button", { name: "使用记录" }));
    const list = await within(item).findByRole("list", { name: "使用记录" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(list).toHaveTextContent("grant.issued");
    expect(list).toHaveTextContent("item.updated");
    expect(list).toHaveTextContent("it_1");
    expect(s.calls.some((c) => c.path.startsWith("/v1/audit/events") && new URL(c.path, "http://x").searchParams.get("grant") === "grt_a")).toBe(true);
  });

  it("没有授权单时给出空状态；列表加载失败展示服务端原因", async () => {
    mount(server().impl);
    expect(await screen.findByText("还没有授权单")).toBeInTheDocument();
  });

  it("加载失败（403）展示服务端原因", async () => {
    const f = (async (url: string) => (url === "/v1/settings/grants" ? json(403, { code: "web_only", why: "Web session only", next: "log in" }) : url === "/v1/settings/tokens" ? json(200, { credentials: [] }) : json(200, { projects: [] }))) as unknown as typeof fetch;
    mount(f);
    expect(await screen.findByRole("alert")).toHaveTextContent("Web session only");
  });
});
