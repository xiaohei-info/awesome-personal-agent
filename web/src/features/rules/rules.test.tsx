import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/packs 的 GET /v1/rules（含 class）、PATCH /v1/rules/{id}/mode、GET /v1/packs 契约，以及 api/why 的 GET /v1/audit/events 应答。
// 非 Web 会话切模式 403 web_only，非法模式 400；错误体见附录 B.3。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const K1 = "brief/event_rules/tag-hit"; // 不在词表里：名字取包的说明
const K2 = "follow-up/clock_rules/stale-check"; // 内置规则：名字取词表
const K3 = "gone/event_rules/x"; // 包已不在：没有 class
const N1 = "周期简报", N2 = "14 天没动静的事项问我一声";

function server(opts: { denied?: boolean; auditFails?: boolean } = {}) {
  const rules = [
    { key: K1, pack: "brief", group: "event_rules", id: "tag-hit", mode: "observe", hash: "h1", class: "action" },
    { key: K2, pack: "follow-up", group: "clock_rules", id: "stale-check", mode: "auto", hash: "h2", class: "reminder" },
    { key: K3, pack: "gone", group: "event_rules", id: "x", mode: "off", hash: "h3" },
  ];
  const now = Date.now();
  const observed = [
    { seq: 7, type: "rule.observed", object: "itm_a", actor: { type: "system", principal: "rule:" + K1, via: "rule" }, at: new Date(now - 3600e3).toISOString(), chain_depth: 0, reason: "observe", payload: { would_do: [{ action: "create_timer" }] } },
    { seq: 8, type: "rule.observed", object: "itm_b", actor: { type: "system", principal: "rule:" + K1, via: "rule" }, at: new Date(now - 2 * 86400e3).toISOString(), chain_depth: 0, payload: { would_do: [] } },
    { seq: 3, type: "rule.observed", object: "itm_c", actor: { type: "system", principal: "rule:" + K1, via: "rule" }, at: new Date(now - 20 * 86400e3).toISOString(), chain_depth: 0, payload: { would_do: [] } },
  ];
  const patches: { path: string; body: any; csrf?: string }[] = [];
  const audits: URLSearchParams[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/packs") return json(200, { packs: [{ name: "brief", description: N1 }, { name: "follow-up", description: "跟进" }] });
    if (url === "/v1/items/itm_a") return json(200, { item: { id: "itm_a", title: "续保材料确认" } });
    if (url === "/v1/rules" && method === "GET") return json(200, { rules });
    const m = /^\/v1\/rules\/([^/]+)\/mode$/.exec(url);
    if (m && method === "PATCH") {
      const body = JSON.parse(init.body as string);
      patches.push({ path: url, body, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] });
      if (opts.denied) return json(403, { code: "web_only", why: "rule modes can only be changed by the owner in the Web UI", next: "sign in to the Web UI" });
      const r = rules.find((x) => x.key === decodeURIComponent(m[1]));
      if (!r) return json(404, { code: "not_found", why: "no such rule", next: "list rules" });
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
    if (url.startsWith("/v1/items/")) return json(404, { code: "not_found", why: "no", next: "" });
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
const modes = async (name: string) => within(await screen.findByRole("radiogroup", { name: `${name}：开关` }));
const checked = async (name: string) => ((await modes(name)).getAllByRole("radio") as HTMLInputElement[]).find((r) => r.checked)?.parentElement?.textContent;

beforeEach(() => sessionStorage.clear());

describe("自动规则 › 规则", () => {
  it("ACT-RULE-03: 按类别分「提醒我 / 替我动手 / 其他」，名字是中文，模式是「关 / 试运行 / 开」", async () => {
    mount(server().impl);
    const remind = within(await screen.findByRole("region", { name: "提醒我" }));
    expect(remind.getByText(N2)).toBeInTheDocument();
    expect(await checked(N2)).toBe("开");
    const act = within(screen.getByRole("region", { name: "替我动手" }));
    expect(act.getByText(N1)).toBeInTheDocument();
    expect(await checked(N1)).toBe("试运行");
    expect(within(screen.getByRole("region", { name: "其他" })).getByText("x")).toBeInTheDocument();
    expect(screen.queryByText(K1)).not.toBeInTheDocument(); // 原始 key 只在「显示原始值」里
  });

  it("ACT-RULE-03: 本人切开，请求带 CSRF，路径为编码后的规则 key，页面显示服务端返回的模式", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.click((await modes(N1)).getByRole("radio", { name: "开" }));
    await waitFor(async () => expect(await checked(N1)).toBe("开"));
    expect(s.patches).toEqual([{ path: `/v1/rules/${encodeURIComponent(K1)}/mode`, body: { mode: "auto" }, csrf: "tok-1" }]);
  });

  it("ACT-RULE-03: 可切关", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.click((await modes(N2)).getByRole("radio", { name: "关" }));
    await waitFor(() => expect(s.rules[1].mode).toBe("off"));
  });

  it("ACT-RULE-03: 服务端拒绝（web_only）时显示原因，模式保持原值", async () => {
    const s = server({ denied: true });
    mount(s.impl);
    await userEvent.click((await modes(N1)).getByRole("radio", { name: "开" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(await checked(N1)).toBe("试运行");
    expect(s.rules[0].mode).toBe("observe");
  });

  it("ACT-RULE-03: 试运行的规则显示本周本来会做几次，点开是一句话列表；只按 rule.observed 与该规则查", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.click(await screen.findByRole("button", { name: "本周本来会做 2 次 ›" }));
    const list = within(await screen.findByRole("list", { name: `${N1}：本来会做的事` }));
    expect(await list.findByText(/续保材料确认/)).toBeInTheDocument();
    expect(list.getAllByRole("listitem")).toHaveLength(2); // 20 天前的那条不算本周
    expect(list.queryByText(/create_timer/)).not.toBeInTheDocument();
    expect(s.audits[0].get("type")).toBe("rule.observed");
    expect(s.audits[0].get("actor")).toBe("rule:" + K1);
  });

  it("ACT-RULE-03: 开着或关着的规则不查观察记录", async () => {
    const s = server();
    mount(s.impl);
    await screen.findByRole("region", { name: "提醒我" });
    await waitFor(() => expect(s.audits.length).toBe(1));
    expect(s.audits.map((a) => a.get("actor"))).toEqual(["rule:" + K1]);
  });

  it("ACT-RULE-03: 观察记录读取失败显示错误", async () => {
    mount(server({ auditFails: true }).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });
});
