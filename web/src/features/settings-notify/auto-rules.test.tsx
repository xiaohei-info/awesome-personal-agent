import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { policyServer, v28Overview } from "../../app/test-policy-server";
import { feature } from "./index";

// 测试边界替身：按 api/packs 的规则路由与 api/settings/policy 的 disturb 段、总览、预览应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const K2 = "follow-up/clock_rules/stale-check";

// v28 的到点提醒、截止已过、跟进与停滞都是"在对话里马上告诉我"（线上 2026-11-23 的值）。
const MATRIX = { default_channel: "channel:ch-iphone", rows: { scheduled: { agent: "main", user: "off" }, overdue: { agent: "main", user: "off" }, followup: { agent: "main", user: "off" }, stale: { agent: "main", user: "off" } } };
const PROACTIVE = { on: true, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6 };
const OVERVIEW = {
  since: "2026-11-16T15:05:00Z",
  rows: ["scheduled", "overdue", "followup", "stale"].map((key) => ({ key, produced: true, events: 0, pushed: 0 })),
  now: { decisions_pending: 0, deadlines: 0, overdue: 0, waiting: 0, stale_now: 0, stale_from: "2026-12-06T02:00:00Z", proposals_new: 275, rule_observed: { [K2]: 4 } },
  source_alert: { streak_failures: 3, low_frequency_over_sec: 3600, sources: [] },
};

function server() {
  const rules = [
    { key: K2, mode: "auto" },
    { key: "follow-up/clock_rules/follow-up-interval", mode: "auto" },
  ];
  let disturb: any = { remind_on_deadline: true, daily_cap: 20, mutes: [], merge_window_sec: 1800, proactive: PROACTIVE, delivery: MATRIX };
  const puts: any[] = [];
  const patches: { path: string; body: any; csrf?: string }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/rules" && method === "GET") return json(200, { rules });
    if (url === "/v1/settings/policy") {
      if (method === "PUT") {
        puts.push(JSON.parse(init.body as string));
        disturb = { ...puts.at(-1).disturb, delivery: puts.at(-1).disturb.delivery ?? disturb.delivery }; // 没带矩阵时沿用已存的（同服务端）
      }
      return json(200, { version: 1, disturb });
    }
    const m = /^\/v1\/rules\/([^/]+)\/mode$/.exec(url);
    if (m && method === "PATCH") {
      const body = JSON.parse(init.body as string);
      patches.push({ path: url, body, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] });
      const r = rules.find((x) => x.key === decodeURIComponent(m[1]));
      if (!r) return json(404, { code: "not_found", why: "no such rule", next: "list rules" });
      r.mode = body.mode;
      return json(200, r);
    }
    if (url === "/v1/settings/notify/overview") return json(200, OVERVIEW);
    if (url === "/v1/settings/proactive/preview") return json(200, { total: 3, batches: 1, next_count: 3, route: "wake:agt", entries: [] });
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, patches, rules, puts };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/notify"]}>
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("设置 › 提醒与推送 › 自动规则与拍板通知", () => {
  it("自动规则：v28 的四类都是「在对话里马上告诉我」，各带一句后果与按当前数据的预览；跟进选不提醒只切规则模式为关，不改去处", async () => {
    const s = server();
    mount(s.impl);
    const g = within(await screen.findByRole("region", { name: "自动规则" }));
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

  it("自动规则：截止已过选不提醒只关 remind_on_deadline；到点提醒改成下次聊天时只写这一行，开着的 proactive 原样带回；「什么时候提醒」里没有第二个开关", async () => {
    const s = server();
    mount(s.impl);
    const g = within(await screen.findByRole("region", { name: "自动规则" }));
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

  it("自动规则：规则在试运行时单选在「不提醒」，写明本周本来会提醒几次，点「改成正式」切开", async () => {
    const s = server();
    s.rules[0].mode = "observe";
    mount(s.impl);
    const row = within(await screen.findByRole("group", { name: "14 天没动静" }));
    expect(await row.findByText("试运行中：本周本来会提醒 4 次")).toBeInTheDocument();
    expect(row.getByRole("radio", { name: "不提醒（在今天页里能看到）" })).toBeChecked();
    await userEvent.click(row.getByRole("button", { name: "改成正式" }));
    await waitFor(() => expect(s.patches.at(-1)).toMatchObject({ body: { mode: "auto" } }));
  });

  it("拍板通知与规则自带的提醒：两个拍板单选在「有事要你拍板时怎么告诉你」，有规则产生的提醒行并在「自动规则」里", async () => {
    const s = policyServer({ v28: true });
    const o = v28Overview();
    const impl = (async (url: string, init?: RequestInit) =>
      url === "/v1/settings/notify/overview"
        ? json(200, { ...o, rows: o.rows.map((r) => (r.key === "rule_reminder" ? { ...r, produced: true, rules: ["p/event_rules/r"] } : r)) })
        : s.impl(url, init)) as unknown as typeof fetch;
    mount(impl);
    const d = within(await screen.findByRole("region", { name: "有事要你拍板时怎么告诉你" }));
    expect(await d.findByRole("radiogroup", { name: "有问题等你决定时" })).toBeInTheDocument();
    expect(d.getByRole("radiogroup", { name: "Agent 替你答了" })).toBeInTheDocument();
    const auto = screen.getByRole("region", { name: "自动规则" });
    await waitFor(() => expect(auto.querySelector('[data-reminder="rule_reminder"]')).not.toBeNull());
    expect(auto.querySelector('[data-reminder="source_alert"]')).toBeNull(); // 没有规则产生时不显示
  });
});
