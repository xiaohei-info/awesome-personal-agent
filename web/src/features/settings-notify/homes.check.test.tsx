import { withSession } from "../../app/test-session";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { features } from "../../app/registry";
import { policyServer, v28Overview } from "../settings-policy/test-server";
import { EDITORS, HOME_PATH, kindOf, type Home } from "./reminders";

// 防遗漏（设计 v2 §5 第 1 条）：每一类提醒都必须有一个编辑位置。
// 一、行表对照内核源码 kernel/policy/delivery.go 的行键常量：内核新增一类而这里没给编辑页，这个测试失败。
// 二、用线上 v28 的数据逐页渲染：总览里每一条有产生方的行，都要在它的编辑页上出现一个单选（已选中当前值）并带按当前数据的预览。
const go = import.meta.glob(["../../../../kernel/policy/delivery.go"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const src = Object.values(go)[0];

describe("每类提醒都有编辑位置", () => {
  it("内核的每个行键（含按实例、按类型的前缀）在 EDITORS 里有编辑页，编辑页是已登记的页面", () => {
    expect(src, "读不到 kernel/policy/delivery.go：检查 glob").toBeTruthy();
    const block = src.slice(src.indexOf("RowDecision "), src.indexOf("TaskOther = "));
    const keys = [...block.matchAll(/^\s*Row\w+\s*=\s*"([^"]+)"/gm)].map((m) => m[1]);
    expect(keys.length).toBeGreaterThanOrEqual(16);
    const missing = keys.filter((k) => !EDITORS[kindOf(k)]);
    expect(missing, "内核有这些行，但没有编辑位置：在 reminders.ts 的 EDITORS 里给它一个页面，并在那个页面放上 <Reminder>").toEqual([]);
    const paths = new Set(features.map((f) => "/" + f.path));
    for (const [home, path] of Object.entries(HOME_PATH)) expect(paths.has(path), `${home} → ${path} 不是已登记的页面`).toBe(true);
    for (const home of Object.values(EDITORS)) expect(HOME_PATH[home]).toBeTruthy();
  });
});

beforeEach(() => sessionStorage.clear());

const shown = (key: string) => !/^task_(result|failed):/.test(key);
const homes = [...new Set(Object.values(EDITORS))] as Home[];

describe("用当前数据（v28）渲染每个编辑页", () => {
  it.each(homes)("%s：这一页负责的每一类都有单选、当前值已选中、带预览", async (home) => {
    const s = policyServer({ v28: true });
    s.doc.disturb.delivery.rows.source_result ??= { agent: "main", user: "off", else: "next_chat" };
    const overview = v28Overview();
    if (!overview.rows.some((r) => r.key === "source_result")) overview.rows.push({ key: "source_result", produced: true, events: 0, pushed: 0 });
    const impl = (async (url: string, init?: RequestInit) => {
      if (url === "/v1/settings/notify/overview") return new Response(JSON.stringify(overview), { status: 200, headers: { "Content-Type": "application/json" } });
      if (url === "/v1/settings/source-pipeline") return new Response(JSON.stringify({ version: 28, source_pipeline: {} }), { status: 200, headers: { "Content-Type": "application/json" } });
      if (url === "/v1/settings/judgment") return new Response(JSON.stringify({ version: 28, bands: [], shadow: [], spot_check: { per_brief: 5 } }), { status: 200, headers: { "Content-Type": "application/json" } });
      if (url.startsWith("/v1/agents")) return new Response(JSON.stringify({ agents: [], selected: {}, reason: "" }), { status: 200, headers: { "Content-Type": "application/json" } });
      if (url === "/v1/items?kind=brief") return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
      if (url === "/v1/packs") return new Response(JSON.stringify({ packs: [{ name: "brief", instance_params: {} }, { name: "review", instance_params: {} }] }), { status: 200, headers: { "Content-Type": "application/json" } });
      return s.impl(url, init);
    }) as unknown as typeof fetch;
    sessionStorage.setItem("apa.csrf", "tok-1");
    const { container } = render(
      <MemoryRouter initialEntries={[HOME_PATH[home]]}>
        <App features={features} fetchImpl={withSession(impl)} />
      </MemoryRouter>,
    );
    const rows = overview.rows.filter((r) => r.produced && shown(r.key) && EDITORS[kindOf(r.key)] === home);
    expect(rows.length, `${home} 在 v28 下应至少负责一类提醒`).toBeGreaterThan(0);
    await screen.findByRole("main");
    for (const r of rows) {
      await waitFor(() => expect(container.querySelector(`[data-reminder="${r.key}"]`), `${home} 上找不到 ${r.key} 的单选`).not.toBeNull(), { timeout: 3000 });
      const el = container.querySelector(`[data-reminder="${r.key}"]`)!;
      expect(el.querySelector("input[type=radio]:checked"), `${r.key} 没有选中的值`).not.toBeNull();
      await waitFor(() => expect(el.querySelector('[role="note"]')?.textContent, `${r.key} 没有按当前数据的预览`).toBeTruthy());
    }
    expect(screen.getByRole("main")).not.toHaveTextContent(/产生|推给 Agent|Agent 收不到时|主会话/);
  });
});
