import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/packs 的 GET /v1/rules、PATCH /v1/rules/{id}/mode 契约，以及 api/why 的 GET /v1/audit/events 应答。
// 非 Web 会话切模式 403 web_only，非法模式 400；错误体见附录 B.3。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const K1 = "brief/event_rules/tag-hit";
const K2 = "brief/clock_rules/morning";

function server(opts: { denied?: boolean; auditFails?: boolean } = {}) {
  const rules = [
    { key: K1, pack: "brief", group: "event_rules", id: "tag-hit", mode: "observe", hash: "h1" },
    { key: K2, pack: "brief", group: "clock_rules", id: "morning", mode: "off", hash: "h2" },
  ];
  const observed = [
    { seq: 7, type: "rule.observed", object: "item_a", actor: { type: "system", principal: "rule:" + K1, via: "rule" }, at: "2030-01-01T00:00:00Z", chain_depth: 0, reason: "observe: would run " + K1, payload: { would_do: [{ action: "create_timer" }] } },
  ];
  const patches: { path: string; body: any; csrf?: string }[] = [];
  const audits: URLSearchParams[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/rules" && method === "GET") return json(200, { rules });
    const m = /^\/v1\/rules\/([^/]+)\/mode$/.exec(url);
    if (m && method === "PATCH") {
      const body = JSON.parse(init.body as string);
      patches.push({ path: url, body, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] });
      if (opts.denied) return json(403, { code: "web_only", why: "rule modes can only be changed by the owner in the Web UI", next: "sign in to the Web UI" });
      const r = rules.find((x) => x.key === decodeURIComponent(m[1]));
      if (!r) return json(404, { code: "not_found", why: "no such rule", next: "list rules" });
      if (!["off", "observe", "auto"].includes(body.mode)) return json(400, { code: "invalid_request", why: "mode must be off, observe or auto", next: "fix the request and retry" });
      r.mode = body.mode;
      return json(200, r);
    }
    const u = new URL(url, "http://x");
    if (u.pathname === "/v1/audit/events") {
      audits.push(u.searchParams);
      if (opts.auditFails) return json(500, { code: "internal", why: "ledger unavailable", next: "retry" });
      const q = u.searchParams;
      return json(200, { events: observed.filter((e) => e.type === q.get("type") && e.actor.principal === q.get("actor")) });
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, patches, audits, rules };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/rules"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const modeOf = async (key: string) => (await screen.findByLabelText(`模式 ${key}`)) as HTMLSelectElement;

beforeEach(() => sessionStorage.clear());

describe("规则模式页", () => {
  it("ACT-RULE-03: 列出规则及当前模式", async () => {
    mount(server().impl);
    expect((await modeOf(K1)).value).toBe("observe");
    expect((await modeOf(K2)).value).toBe("off");
    expect(screen.getByText("简报 · 定时")).toBeInTheDocument();
  });

  it("ACT-RULE-03: 本人切 auto，请求带 CSRF，路径为编码后的规则 key，页面显示服务端返回的模式", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.selectOptions(await modeOf(K1), "auto");
    await waitFor(async () => expect((await modeOf(K1)).value).toBe("auto"));
    expect(s.patches).toEqual([{ path: `/v1/rules/${encodeURIComponent(K1)}/mode`, body: { mode: "auto" }, csrf: "tok-1" }]);
    expect(s.rules[0].mode).toBe("auto");
  });

  it("ACT-RULE-03: 可切 off", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.selectOptions(await modeOf(K1), "off");
    await waitFor(() => expect(s.rules[0].mode).toBe("off"));
  });

  it("ACT-RULE-03: 服务端拒绝（web_only）时显示原因，模式保持原值", async () => {
    const s = server({ denied: true });
    mount(s.impl);
    await userEvent.selectOptions(await modeOf(K1), "auto");
    expect(await screen.findByRole("alert")).toHaveTextContent("rule modes can only be changed by the owner");
    expect((await modeOf(K1)).value).toBe("observe");
    expect(s.rules[0].mode).toBe("observe");
  });

  it("ACT-RULE-03: 审阅观察记录——只按 rule.observed 与该规则筛选，显示本来会做什么", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.click(await screen.findByRole("button", { name: `审阅观察记录 ${K1}` }));
    const list = within(await screen.findByRole("list", { name: `观察记录 ${K1}` }));
    expect(list.getByText(/#7/)).toBeInTheDocument();
    expect(list.getByText(/create_timer/)).toBeInTheDocument();
    expect(s.audits[0].get("type")).toBe("rule.observed");
    expect(s.audits[0].get("actor")).toBe("rule:" + K1);
  });

  it("ACT-RULE-03: 没有观察记录时给出空状态；读取失败时显示错误", async () => {
    mount(server().impl);
    await userEvent.click(await screen.findByRole("button", { name: `审阅观察记录 ${K2}` }));
    expect(await screen.findByText("暂无观察记录")).toBeInTheDocument();
  });

  it("ACT-RULE-03: 观察记录读取失败显示服务端原因", async () => {
    mount(server({ auditFails: true }).impl);
    await userEvent.click(await screen.findByRole("button", { name: `审阅观察记录 ${K1}` }));
    expect(await screen.findByRole("alert")).toHaveTextContent("ledger unavailable");
  });
});
