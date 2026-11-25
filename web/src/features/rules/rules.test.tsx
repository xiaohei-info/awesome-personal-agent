import { withSession } from "../../app/test-session";
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
const K4 = "background-run/event_rules/handle"; // 以 Agent 本人身份运行
const N1 = "周期简报", N2 = "14 天没动静的事项问我一声";

// v28 的到点提醒、截止已过、跟进与停滞都是"在对话里马上告诉我"（线上 2026-11-23 的值）。
const MATRIX = { default_channel: "channel:ch-iphone", rows: { scheduled: { agent: "main", user: "off" }, overdue: { agent: "main", user: "off" }, followup: { agent: "main", user: "off" }, stale: { agent: "main", user: "off" } } };
const PROACTIVE = { on: true, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6 };
const OVERVIEW = {
  since: "2026-11-16T15:05:00Z",
  rows: ["scheduled", "overdue", "followup", "stale"].map((key) => ({ key, produced: true, events: 0, pushed: 0 })),
  now: { decisions_pending: 0, deadlines: 0, overdue: 0, waiting: 0, stale_now: 0, stale_from: "2026-12-06T02:00:00Z", proposals_new: 275, rule_observed: { [K2]: 4 } },
  source_alert: { streak_failures: 3, low_frequency_over_sec: 3600, sources: [] },
};

function server(opts: { denied?: boolean; auditFails?: boolean; matrix?: boolean } = {}) {
  const rules = [
    { key: K1, pack: "brief", group: "event_rules", id: "tag-hit", mode: "observe", hash: "h1", class: "action" },
    { key: K2, pack: "follow-up", group: "clock_rules", id: "stale-check", mode: "auto", hash: "h2", class: "reminder" },
    { key: K3, pack: "gone", group: "event_rules", id: "x", mode: "off", hash: "h3" },
    { key: K4, pack: "background-run", group: "event_rules", id: "handle", mode: "auto", hash: "h4", class: "action", runner: "main_session" },
    { key: "follow-up/clock_rules/follow-up-interval", pack: "follow-up", group: "clock_rules", id: "follow-up-interval", mode: "auto", hash: "h5", class: "reminder" },
  ];
  let launch: any = { by_harness: { hermes: "wake" }, by_rule: {} };
  let disturb: any = opts.matrix ? { remind_on_deadline: true, daily_cap: 20, mutes: [], merge_window_sec: 1800, proactive: PROACTIVE, delivery: MATRIX } : undefined;
  const puts: any[] = [];
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
    if (url === "/v1/settings/policy") {
      if (method === "PUT") {
        puts.push(JSON.parse(init.body as string));
        if (puts.at(-1).main_session_launch) launch = puts.at(-1).main_session_launch;
        if (puts.at(-1).disturb) disturb = { ...puts.at(-1).disturb, delivery: puts.at(-1).disturb.delivery ?? disturb.delivery }; // 没带矩阵时沿用已存的（同服务端）
      }
      return json(200, { version: 1, main_session_launch: launch, ...(disturb ? { disturb } : {}) });
    }
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
    if (url === "/v1/settings/notify/overview" && opts.matrix) return json(200, OVERVIEW);
    if (url === "/v1/settings/proactive/preview" && opts.matrix) return json(200, { total: 3, batches: 1, next_count: 3, route: "wake:agt", entries: [] });
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, patches, audits, rules, puts, disturb: () => disturb };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/rules"]}>
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}
const modes = async (name: string) => within(await screen.findByRole("radiogroup", { name: `${name}：开关` }));
const checked = async (name: string) => ((await modes(name)).getAllByRole("radio") as HTMLInputElement[]).find((r) => r.checked)?.parentElement?.textContent;

beforeEach(() => sessionStorage.clear());

describe("设置 › 自动规则（原 自动动手）", () => {
  it("ACT-RULE-03: 按类别分「替我动手 / 其他」，名字是中文，模式是「关 / 试运行 / 开」；提醒类规则不在列表里（由「提醒我」的单选写模式），取不到矩阵时「提醒我」不显示", async () => {
    mount(server().impl);
    const act = within(await screen.findByRole("region", { name: "替我动手" }));
    expect(screen.queryByRole("region", { name: "提醒我" })).toBeNull();
    expect(screen.queryByText(N2)).toBeNull();
    expect(screen.queryByRole("radiogroup", { name: `${N2}：开关` })).toBeNull();
    expect(screen.queryByRole("link", { name: /提醒与推送/ })).toBeNull();
    expect(act.getByText(N1)).toBeInTheDocument();
    expect(await checked(N1)).toBe("试运行");
    expect(within(screen.getByRole("region", { name: "其他" })).getByText("x")).toBeInTheDocument();
    expect(screen.queryByText(K1)).not.toBeInTheDocument(); // 原始 key 只在「显示原始值」里
  });

  it("提醒我：v28 的四类都是「在对话里马上告诉我」，各带一句后果与按当前数据的预览；跟进选不提醒只切规则模式为关，不改去处", async () => {
    const s = server({ matrix: true });
    mount(s.impl);
    const g = within(await screen.findByRole("region", { name: "提醒我" }));
    for (const name of ["到点提醒", "截止已过", "等待到期跟进", "14 天没动静"]) {
      expect(within(await g.findByRole("radiogroup", { name })).getByRole("radio", { name: "在对话里马上告诉我" })).toBeChecked();
    }
    expect(await g.findByText("接下来 24 小时 3 条，合成 1 条消息")).toBeInTheDocument();
    expect(g.getByText("现在没有带截止的事项，开着也暂时不会提醒")).toBeInTheDocument();
    expect(g.getByText("现在没有在等的事项")).toBeInTheDocument();
    expect(g.getByText(/^现在没有；最早 12 月 6 日才可能有$/)).toBeInTheDocument();
    expect(g.getByText("在等的事每 72 小时提醒一次，两轮后问去留；选不提醒：也不再问去留。")).toBeInTheDocument();
    expect(screen.getByRole("main")).not.toHaveTextContent(/产生|推给 Agent|主会话|Agent 收不到时/);
    await userEvent.click(within(g.getByRole("radiogroup", { name: "等待到期跟进" })).getByRole("radio", { name: "不提醒（在今天页里能看到）" }));
    await waitFor(() => expect(s.patches).toEqual([{ path: `/v1/rules/${encodeURIComponent("follow-up/clock_rules/follow-up-interval")}/mode`, body: { mode: "off" }, csrf: "tok-1" }]));
    expect(s.puts).toEqual([]);
  });

  it("提醒我：截止已过选不提醒只关 remind_on_deadline；到点提醒改成下次聊天时只写这一行，开着的 proactive 原样带回；「什么时候提醒」里没有第二个开关", async () => {
    const s = server({ matrix: true });
    mount(s.impl);
    const g = within(await screen.findByRole("region", { name: "提醒我" }));
    await userEvent.click(within(await g.findByRole("radiogroup", { name: "截止已过" })).getByRole("radio", { name: "不提醒（在今天页里能看到）" }));
    await waitFor(() => expect(s.puts).toHaveLength(1));
    expect(s.puts[0].disturb.remind_on_deadline).toBe(false);
    expect(s.puts[0].disturb).not.toHaveProperty("delivery");
    await userEvent.click(within(g.getByRole("radiogroup", { name: "到点提醒" })).getByRole("radio", { name: "下次聊天时告诉我" }));
    await waitFor(() => expect(s.puts).toHaveLength(2));
    expect(s.puts[1].disturb.delivery.rows).toEqual({ ...MATRIX.rows, scheduled: { agent: "next_chat", user: "off" } });
    expect(s.puts[1].disturb.proactive).toEqual(PROACTIVE);
    expect(s.puts[1].disturb.remind_on_deadline).toBe(false);
    await userEvent.click(g.getByText("什么时候提醒"));
    expect(g.queryByRole("switch")).toBeNull();
  });

  it("提醒我：规则在试运行时单选在「不提醒」，写明本周本来会提醒几次，点「改成正式」切开", async () => {
    const s = server({ matrix: true });
    s.rules[1].mode = "observe";
    mount(s.impl);
    const row = within(await screen.findByRole("group", { name: "14 天没动静" }));
    expect(await row.findByText("试运行中：本周本来会提醒 4 次")).toBeInTheDocument();
    expect(row.getByRole("radio", { name: "不提醒（在今天页里能看到）" })).toBeChecked();
    await userEvent.click(row.getByRole("button", { name: "改成正式" }));
    await waitFor(() => expect(s.patches.at(-1)).toMatchObject({ body: { mode: "auto" } }));
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
    await userEvent.click((await modes(N1)).getByRole("radio", { name: "关" }));
    await waitFor(() => expect(s.rules[0].mode).toBe("off"));
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
    await screen.findByRole("region", { name: "替我动手" });
    await waitFor(() => expect(s.audits.length).toBe(1));
    expect(s.audits.map((a) => a.get("actor"))).toEqual(["rule:" + K1]);
  });

  it("以 Agent 本人身份运行的规则可覆盖在哪里做：默认跟随默认；改选只写这条规则，Agent 的设置原样带回；改回跟随即删掉覆盖", async () => {
    const s = server();
    mount(s.impl);
    const pick = (await screen.findAllByRole("combobox"));
    expect(pick).toHaveLength(1); // 只有 runner=main_session 的规则有
    expect(pick[0]).toHaveValue("inherit");
    await userEvent.selectOptions(pick[0], "单独在后台做");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ main_session_launch: { by_harness: { hermes: "wake" }, by_rule: { [K4]: "headless" } } }));
    expect(await screen.findByText("已保存")).toBeInTheDocument(); // 即改即存，就地写「已保存」（界面设计 §6.1）
    await userEvent.selectOptions(screen.getByRole("combobox"), "跟随默认");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ main_session_launch: { by_harness: { hermes: "wake" }, by_rule: {} } }));
  });

  it("ACT-RULE-03: 观察记录读取失败显示错误", async () => {
    mount(server({ auditFails: true }).impl);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });
});
