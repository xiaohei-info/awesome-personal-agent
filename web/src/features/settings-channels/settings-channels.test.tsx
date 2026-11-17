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

// 内置渠道连接的替身：按 api/connections 的契约应答；凭据只回是否已设置。
const BARK = {
  type: "bark", direction: "channel", title: { zh: "Bark", en: "Bark" }, tagline: { zh: "推送到 iPhone", en: "Push to iPhone" },
  default: { zh: "我的 iPhone", en: "My iPhone" }, via: { zh: "推送服务器", en: "the push server" },
  capabilities: { buttons: false, reply: false, identify: false },
  fields: [
    { key: "server", kind: "url", label: { zh: "服务器地址", en: "Server" }, default: "https://api.day.app", required: true },
    { key: "device_key", kind: "secret", label: { zh: "设备密钥", en: "Device key" }, required: true },
    { key: "group", kind: "text", label: { zh: "通知分组", en: "Group" }, default: "APA", group: "advanced" },
  ],
};
function connServer() {
  const list: any[] = [];
  const calls: { method: string; url: string; body?: any }[] = [];
  const impl = async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    if (method !== "GET") calls.push({ method, url, body });
    if (url.startsWith("/v1/connections/types")) return json(200, { types: [BARK] });
    if (url === "/v1/connections/test") return json(200, { ok: true });
    if (url === "/v1/connections" && method === "POST") {
      const c = { id: "ch-new1", type: body.type, name: body.name, config: body.config, secrets: { device_key: !!body.secrets?.device_key }, enabled: body.enabled, state: body.enabled ? "ok" : "disabled" };
      list.push(c);
      return json(201, c);
    }
    if (url.startsWith("/v1/connections")) return json(200, { connections: list });
    return json(404, { code: "not_found", why: "no", next: "no" });
  };
  return { list, calls, impl };
}
const conns = connServer();

function server(extKeys?: Record<string, unknown>[]) {
  conns.list.length = 0;
  conns.calls.length = 0;
  const s = policyServer({ extKeys });
  const ident: { state: string; sender?: string; display?: string } = { state: "waiting" };
  const calls: { method: string; url: string; body?: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url.startsWith("/v1/connections")) return conns.impl(url, init);
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
  return { ...s, impl, ident, calls, conns };
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

describe("设置 › 提醒渠道：内置渠道", () => {
  it("卡片只显示现状：在用、有问题（一句话加重新填写）、已停用；不露连接 id 与问题代码", async () => {
    const s = server();
    conns.list.push(
      { id: "ch-a1", type: "bark", name: "我的 iPhone", config: {}, secrets: { device_key: true }, enabled: true, state: "ok", last_ok_at: "2030-01-01T10:42:00Z" },
      { id: "ch-b2", type: "bark", name: "备用手机", config: {}, secrets: { device_key: false }, enabled: true, state: "problem", problem: "credentials_missing" },
      { id: "ch-c3", type: "bark", name: "旧手机", config: {}, secrets: { device_key: true }, enabled: false, state: "disabled" },
    );
    s.doc.disturb.routes.decision = "channel:ch-a1";
    mount(s.impl);
    const a = within(await screen.findByRole("region", { name: "我的 iPhone" }));
    expect(a.getByText("在用")).toBeInTheDocument();
    expect(a.getByText("只能看")).toBeInTheDocument();
    expect(a.getByText(/用来送：等你决定/)).toBeInTheDocument();
    const b = within(screen.getByRole("region", { name: "备用手机" }));
    expect(b.getByText("有问题")).toBeInTheDocument();
    expect(b.getByRole("alert")).toHaveTextContent("凭据没填或已失效，重新填写");
    expect(b.getByRole("button", { name: "重新填写" })).toBeInTheDocument();
    const c = within(screen.getByRole("region", { name: "旧手机" }));
    expect(c.getByText("已停用")).toBeInTheDocument();
    expect(c.getByRole("button", { name: "删除" })).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/ch-a1|ch-b2|credentials_missing/);
  });

  it("添加四步：选类型 → 填表单 → 发一条测试 → 启用；「用它来送」写打扰策略的路由", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.click(await screen.findByRole("button", { name: "+ 添加渠道" }));
    await userEvent.click(screen.getByRole("button", { name: /Bark/ }));
    expect(screen.getByLabelText("名字")).toHaveValue("我的 iPhone");
    await userEvent.type(screen.getByLabelText(/设备密钥/), "dev-1");
    await userEvent.click(screen.getByRole("button", { name: "下一步" }));
    await userEvent.click(screen.getByRole("button", { name: "发一条测试" }));
    expect(await screen.findByText("已送达，去手机上看看")).toBeInTheDocument();
    expect(conns.calls[0]).toEqual({ method: "POST", url: "/v1/connections/test", body: { type: "bark", config: {}, secrets: { device_key: "dev-1" }, send: true } });
    await userEvent.click(screen.getByRole("button", { name: "下一步" }));
    await userEvent.click(screen.getByLabelText("系统状态"));
    await userEvent.click(screen.getByRole("button", { name: "启用" }));
    expect(await screen.findByRole("region", { name: "我的 iPhone" })).toBeInTheDocument();
    expect(conns.calls[1]).toEqual({ method: "POST", url: "/v1/connections", body: { type: "bark", name: "我的 iPhone", config: {}, secrets: { device_key: "dev-1" }, enabled: true } });
    expect(s.puts.at(-1)!.body.disturb.routes).toEqual({ decision: "channel:ch-new1", reminder: "channel:ch-new1", health: "channel:ch-new1" });
  });
});

describe("设置 › 提醒渠道：用自己的程序接入", () => {
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

  it("只签了钥匙不算接通：按钥匙最近一次真实调用显示「还没连上过 / 最近使用」", async () => {
    const s = server([{ id: "k1", kind: "ext", scope: "channel.slack" }, { id: "k3", kind: "ext", scope: "channel.mine", last_seen_at: "2030-01-01T10:42:00Z" }]);
    mount(s.impl);
    const a = within(await screen.findByRole("region", { name: "slack" }));
    expect(a.getByLabelText("连接状态")).toHaveTextContent("还没连上过");
    const b = within(screen.getByRole("region", { name: "mine" }));
    expect(b.getByLabelText("连接状态")).toHaveTextContent(/^最近使用 /);
    expect(screen.getByRole("form", { name: "给自己写的程序发钥匙" })).toBeInTheDocument();
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
