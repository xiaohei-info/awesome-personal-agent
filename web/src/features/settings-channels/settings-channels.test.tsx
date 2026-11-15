import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { policyServer } from "../settings-policy/test-server";

// 测试边界替身：策略三条路由用 policyServer；另按 api/settings/policy 与 api/channel 的契约应答
// PUT /settings/channels/{名}/answerers（只改这一个键）与识别（只在内存里，不写名单）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function server() {
  const s = policyServer();
  const ident: { state: string; sender?: string; display?: string } = { state: "waiting" };
  const calls: { method: string; url: string; body?: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const m = /^\/v1\/settings\/channels\/([^/]+)\/(answerers|identify)$/.exec(url);
    if (!m) return s.impl(url, init);
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, url, body });
    if (m[2] === "answerers") {
      s.doc.channel_answerers = { ...s.doc.channel_answerers, [m[1]]: body.answerers };
      return json(200, { version: 2, answerers: body.answerers });
    }
    if (method === "POST") return json(200, { code: "apa-7K3Q", expires_at: "2030-01-01T00:10:00Z" });
    return json(200, ident);
  }) as unknown as typeof fetch;
  return { ...s, impl, ident, calls };
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/channels"]}>
      <App features={[feature]} fetchImpl={f} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("设置 › 渠道", () => {
  it("每个渠道一张卡：回答人只改这个渠道；只能发文字写回 channels 段，别的渠道原样带回", async () => {
    const s = server();
    s.doc.channels = { old: { text_only: true } };
    s.doc.channel_answerers = { old: ["1"] };
    mount(s.impl);
    const c = within(await screen.findByRole("region", { name: "slack" }));
    expect(screen.getByRole("region", { name: "old" })).toBeInTheDocument(); // 已吊销、只在段里的渠道不丢
    await userEvent.type(c.getByRole("textbox", { name: "谁能在这里替我回答" }), "4242{Enter}");
    await userEvent.click(c.getByLabelText("只能发文字"));
    await userEvent.click(c.getByRole("button", { name: "保存slack" }));
    await c.findByRole("status");
    expect(s.calls.at(-1)).toEqual({ method: "PUT", url: "/v1/settings/channels/slack/answerers", body: { answerers: ["4242"] } });
    expect(s.puts.at(-1)!.body).toEqual({ channels: { old: { text_only: true }, slack: { text_only: true } } });
    expect(s.doc.channel_answerers.old).toEqual(["1"]);
  });

  it("T7 识别我的 ID：给出口令；收到后显示是谁，点「加上」才写名单", async () => {
    const s = server();
    mount(s.impl);
    const c = within(await screen.findByRole("region", { name: "slack" }));
    await userEvent.click(c.getByRole("button", { name: "识别我的 ID" }));
    expect(await c.findByText("给机器人发：/start apa-7K3Q")).toBeInTheDocument();
    expect(c.getByText("等你发消息…（10 分钟内有效）")).toBeInTheDocument();
    Object.assign(s.ident, { state: "got", sender: "4242", display: "@me" });
    expect(await c.findByText(/收到：@me（4242）/, {}, { timeout: 4000 })).toBeInTheDocument();
    expect(s.calls.some((x) => x.url.endsWith("/answerers"))).toBe(false);
    await userEvent.click(c.getByRole("button", { name: "加上" }));
    expect(s.calls.at(-1)).toEqual({ method: "PUT", url: "/v1/settings/channels/slack/answerers", body: { answerers: ["4242"] } });
  });

  it("口令过期时显示没收到，可以再试一次", async () => {
    const s = server();
    s.ident.state = "expired";
    mount(s.impl);
    const c = within(await screen.findByRole("region", { name: "slack" }));
    await userEvent.click(c.getByRole("button", { name: "识别我的 ID" }));
    expect(await c.findByText("没收到")).toBeInTheDocument();
    expect(c.getByRole("button", { name: "再试一次" })).toBeInTheDocument();
  });
});
