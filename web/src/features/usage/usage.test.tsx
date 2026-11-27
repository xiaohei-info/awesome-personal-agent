import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { withSession } from "../../app/test-session";
import { feature } from "./index";

// 替身只回答 GET /v1/usage，形状同附录 B.2。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const c = (requests: number, reported: number, input_tokens = 0, output_tokens = 0, cache_read_tokens = 0) => ({ requests, reported, input_tokens, output_tokens, cache_read_tokens });
const WEEK = {
  since: "2026-11-20T00:00:00Z",
  rows: [
    { part: "judge", purpose: "inbound_screen", model: "judge-model", ...c(3, 2, 1000, 20) },
    { part: "judge", purpose: "inbound_screen", model: "other-model", ...c(1, 1, 500, 5) },
    { part: "model", purpose: "tmpl_unknown_x", model: "gen-model", ...c(1, 1, 7, 8) },
    { part: "agent", purpose: "task", agent: "甲", ...c(2, 0) },
  ],
  days: [{ part: "judge", date: "2026-11-26", ...c(4, 3, 1500, 25) }],
};
let asked: string[] = [];
const impl = (async (url: string) => {
  if (url === "/v1/projects") return json(200, { projects: [] });
  if (url.startsWith("/v1/usage?")) {
    asked.push(url);
    return json(200, url.endsWith("days=7") ? WEEK : { rows: [], days: [] });
  }
  return json(404, { code: "not_found", why: "no such route", next: "" });
}) as unknown as typeof fetch;

beforeEach(() => {
  sessionStorage.clear();
  sessionStorage.setItem("apa.csrf", "tok-1");
  asked = [];
});

describe("诊断 › 用量", () => {
  it("默认看今天；换时间段后分部分合计、分组、按天，没回报的不写 0，不出现内核取值", async () => {
    render(
      <MemoryRouter initialEntries={["/usage"]}>
        <App features={[feature]} fetchImpl={withSession(impl)} />
      </MemoryRouter>,
    );
    expect(await screen.findByText("还没有记录", { selector: "main > p" })).toBeInTheDocument();
    expect(asked).toEqual(["/v1/usage?days=1"]);

    await userEvent.click(screen.getByLabelText("近 7 天"));
    const judge = within(await screen.findByRole("region", { name: "来源判断" }));
    expect(judge.getByText("输入", { selector: "dt" }).nextSibling).toHaveTextContent("1,500");
    expect(judge.getByText("1 次未回报用量", { selector: "p" })).toBeInTheDocument();
    expect(within(judge.getByRole("table", { name: "来源判断 用途" })).getByRole("row", { name: /来源消息值不值得看.*4 1,500 25/ })).toBeInTheDocument();
    expect(judge.getByRole("table", { name: "来源判断 按天", hidden: true })).toHaveTextContent("2026-11-26");
    expect(within(screen.getByRole("region", { name: "其他模型请求" })).getAllByText("其他").length).toBeGreaterThan(0);

    const agent = within(screen.getByRole("region", { name: "Agent 后台运行" }));
    expect(agent.getAllByText("该 Agent 未回报用量").length).toBeGreaterThan(0);
    expect(agent.getByRole("table", { name: "Agent 后台运行 Agent" })).toHaveTextContent("甲 · 任务");
    expect(document.body.textContent).not.toMatch(/inbound_screen|tmpl_unknown_x/);
  });
});
