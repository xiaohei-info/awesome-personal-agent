import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../../app/App";
import { withSession } from "../../app/test-session";
import { feature } from "./index";

// 替身只回答 GET /v1/usage，形状同附录 B.2。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const c = (requests: number, reported: number, input_tokens = 0, output_tokens = 0, cache_read_tokens = 0, cache_write_tokens = 0) => ({ requests, reported, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens });
const since = "2026-11-20T00:00:00Z";
const WEEK = {
  since,
  rows: [
    { part: "judge", purpose: "inbound_screen", model: "judge-model", ...c(3, 2, 1000, 20) },
    { part: "model", purpose: "tmpl_unknown_x", model: "gen-model", ...c(1, 1, 7, 8) },
    { part: "agent", purpose: "task", agent: "甲", ...c(2, 0) },
    { part: "agent", purpose: "background", agent: "甲", model: "gen-model", ...c(1, 1, 100, 3, 60, 20) },
  ],
  slots: ["2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23", "2026-11-24", "2026-11-25", "2026-11-26"],
  series: [
    { slot: "2026-11-25", part: "agent", ...c(2, 0) },
    { slot: "2026-11-26", part: "judge", ...c(3, 2, 1000, 20) },
    { slot: "2026-11-26", part: "model", ...c(1, 1, 7, 8) },
    { slot: "2026-11-26", part: "agent", ...c(1, 1, 100, 3, 60, 20) },
  ],
};
const hours = Array.from({ length: 24 }, (_, h) => String(h).padStart(2, "0"));
let week: unknown = WEEK;
let asked: string[] = [];
const impl = (async (url: string) => {
  if (url === "/v1/projects") return json(200, { projects: [] });
  if (url.startsWith("/v1/usage?")) {
    asked.push(url);
    return json(200, url.endsWith("days=1") ? { since, rows: [], slots: hours, series: [] } : week);
  }
  return json(404, { code: "not_found", why: "no such route", next: "" });
}) as unknown as typeof fetch;

beforeEach(() => {
  sessionStorage.clear();
  sessionStorage.setItem("apa.csrf", "tok-1");
  week = WEEK;
  asked = [];
  // jsdom 不排版，图表容器量到 0 宽就不画：给一个固定尺寸。
  vi.spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue({ width: 640, height: 220, top: 0, left: 0, right: 640, bottom: 220, x: 0, y: 0, toJSON: () => ({}) });
});
afterEach(() => void vi.restoreAllMocks());

const open = () =>
  render(
    <MemoryRouter initialEntries={["/usage"]}>
      <App features={[feature]} fetchImpl={withSession(impl)} />
    </MemoryRouter>,
  );
const region = async (name: string) => within(await screen.findByRole("region", { name }));

describe("用量", () => {
  it("默认近 7 天：数字卡、图、按用途与模型；缓存不重复计、部分未回报有标记、不出现内核取值；分类联动", async () => {
    const { container } = open();
    const tokens = await region("总 Token");
    expect(asked).toEqual(["/v1/usage?days=7"]);
    expect(tokens.getByText("1,138")).toBeInTheDocument(); // 输入已含缓存，不再叠加
    expect((await region("次数")).getByText("3 次未回报用量")).toBeInTheDocument();
    expect((await region("缓存读取占比")).getByText("5.5%")).toBeInTheDocument(); // 60 /（60 + 1027），分母不含缓存写
    expect((await region("主要去向")).getByText("来源判断")).toBeInTheDocument();
    expect(container.querySelectorAll(".usage-unreported")).toHaveLength(2); // 11-25、11-26 柱顶
    expect((await region("每日 Token")).getByText("新输入")).toBeInTheDocument();
    const uses = await region("按用途");
    expect(uses.getByText("甲 · 任务").parentElement).toHaveTextContent("2 次 · 该 Agent 未回报用量");
    expect(uses.getByText("其他")).toBeInTheDocument();
    expect((await region("按模型")).getByText("默认模型").parentElement).toHaveTextContent("未回报用量");
    const row = within(screen.getByRole("table", { name: "按用途", hidden: true })).getByRole("row", { name: /甲 · 任务/, hidden: true });
    expect(row).toHaveTextContent(/默认模型22(未回报){5}$/); // 没回报的不写 0
    expect(document.body.textContent).not.toMatch(/inbound_screen|tmpl_unknown_x/);

    await userEvent.click((await region("每日 Token")).getByRole("button", { name: "按用途" }));
    expect((await region("每日 Token")).getByText("其他模型请求")).toBeInTheDocument();

    await userEvent.click(screen.getByLabelText("其他模型请求"));
    expect((await region("总 Token")).getByText("15")).toBeInTheDocument();
    expect(screen.queryByText("甲 · 任务")).toBeNull();
  });

  it("全部未回报：写未回报、图按次数画，不写 0", async () => {
    week = { since, rows: [WEEK.rows[2]], slots: WEEK.slots, series: [WEEK.series[0]] };
    open();
    const tokens = await region("总 Token");
    expect(tokens.getByText("未回报用量")).toBeInTheDocument();
    expect((await region("每日次数")).getByRole("note")).toHaveTextContent("图按次数画");
    expect((await region("按模型")).getByText("默认模型").parentElement).toHaveTextContent("2 次 · 未回报用量");
    expect(screen.queryByRole("region", { name: "每日 Token" })).toBeNull();
  });

  it("今天没有记录：空态并可切回近 7 天；读取失败可重试", async () => {
    open();
    await region("总 Token");
    await userEvent.click(screen.getByLabelText("今天"));
    expect(await screen.findByText("这段时间还没有记录")).toBeInTheDocument();
    week = undefined; // 空响应体：读取失败
    await userEvent.click(screen.getByRole("button", { name: "看近 7 天" }));
    expect(await screen.findByRole("button", { name: "重试" })).toBeInTheDocument();
    week = WEEK;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("region", { name: "总 Token" })).toBeInTheDocument();
  });
});
