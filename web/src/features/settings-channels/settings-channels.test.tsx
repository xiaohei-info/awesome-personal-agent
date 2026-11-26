import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { setChoice } from "../../app/i18n";
import { App } from "../../app/App";
import { feature } from "./index";
import { policyServer } from "../../app/test-policy-server";

// 测试边界替身：策略三条路由用 policyServer；另按 api/settings/policy 与 api/channel 的契约应答
// PUT /settings/channels/{名}/answerers（只改这一个键）与识别（只在内存里，不写名单）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

// 内置渠道连接的替身：按 api/connections 的契约应答；凭据只回是否已设置。
const BARK = {
  type: "bark", direction: "channel", title: { zh: "Bark", en: "Bark" }, tagline: { zh: "发到 iPhone", en: "Push to iPhone" },
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
  // 删除一个自写程序渠道的替身：按 api/settings/policy 的契约，吊销该渠道的钥匙并删掉策略里的两处。
  const keys = (extKeys ?? [{ id: "k1", kind: "ext", scope: "channel.slack" }, { id: "k2", kind: "ext", scope: "channel.old", revoked_at: "x" }]).map((k) => ({ ...k }));
  const dels: string[] = [];
  const ident: { state: string; sender?: string; display?: string } = { state: "waiting" };
  const calls: { method: string; url: string; body?: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url.startsWith("/v1/connections")) return conns.impl(url, init);
    if (url === "/v1/settings/ext-keys" && method === "GET") return json(200, { credentials: keys });
    const del = /^\/v1\/settings\/channels\/([^/]+)$/.exec(url);
    if (del && method === "DELETE") {
      const name = decodeURIComponent(del[1]);
      dels.push(name);
      for (const k of keys) if (k.scope === "channel." + name && !k.revoked_at) k.revoked_at = "2030-01-01T00:00:00Z";
      delete s.doc.channels[name];
      delete s.doc.channel_answerers[name];
      return json(200, { version: 2, revoked: 1 });
    }
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
  return { ...s, impl, ident, calls, conns, keys, dels };
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/channels"]}>
      <App features={[feature]} fetchImpl={withSession(f)} />
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
    expect(a.getByText(/推到这里的有：有问题等你决定时/)).toBeInTheDocument(); // 只读，按矩阵：旧路由换算后问题卡走默认渠道，默认渠道就是它
    expect(a.getByText("默认渠道")).toBeInTheDocument();
    expect(a.getByRole("link", { name: "去改" })).toHaveAttribute("href", "/settings/notify#overview");
    const b = within(screen.getByRole("region", { name: "备用手机" }));
    expect(b.getByText("有问题")).toBeInTheDocument();
    expect(b.getByRole("alert")).toHaveTextContent("密钥没填或已失效，重新填写");
    expect(b.getByRole("button", { name: "重新填写" })).toBeInTheDocument();
    const c = within(screen.getByRole("region", { name: "旧手机" }));
    expect(c.getByText("已停用")).toBeInTheDocument();
    expect(c.getByRole("button", { name: "删除" })).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/ch-a1|ch-b2|credentials_missing/);
  });

  it("添加四步：选类型 → 填表单 → 发一条测试 → 启用；最后一步只给默认渠道的初值，冻结的旧路由原样带回", async () => {
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
    expect(screen.queryByText("用它来送")).toBeNull();
    expect(screen.getByLabelText("设为默认渠道")).toBeChecked(); // 还没有默认渠道：默认勾上
    await userEvent.click(screen.getByRole("button", { name: "启用" }));
    expect(await screen.findByRole("region", { name: "我的 iPhone" })).toBeInTheDocument();
    expect(conns.calls[1]).toEqual({ method: "POST", url: "/v1/connections", body: { type: "bark", name: "我的 iPhone", config: {}, secrets: { device_key: "dev-1" }, enabled: true } });
    expect(s.puts).toHaveLength(1);
    const d = s.puts[0].body.disturb;
    expect(d.routes).toEqual({ decision: "inject", health: "web" });
    expect(d).not.toHaveProperty("delivery_saved");
    expect(d.delivery.default_channel).toBe("channel:ch-new1");
    expect(d.delivery.rows.decision).toEqual({ agent: "next_chat", user: "off" }); // 只改默认渠道，各行原样
  });

  it("编辑表单只改连接本身：没有「用它来送」，保存不写打扰策略", async () => {
    const s = server();
    conns.list.push({ id: "ch-a1", type: "bark", name: "我的 iPhone", config: {}, secrets: { device_key: true }, enabled: true, state: "ok" });
    s.doc.disturb.routes.decision = "channel:ch-a1";
    mount(s.impl);
    const c = within(await screen.findByRole("region", { name: "我的 iPhone" }));
    await userEvent.click(c.getByRole("button", { name: "编辑" }));
    const form = within(c.getByRole("form", { name: "编辑" }));
    expect(form.queryByText("用它来送")).toBeNull();
    expect(form.queryByRole("checkbox", { name: "等你决定" })).toBeNull();
    await userEvent.click(form.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(conns.calls).toEqual([{ method: "PUT", url: "/v1/connections/ch-a1", body: { config: {}, secrets: {} } }]));
    expect(s.puts).toHaveLength(0);
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

  it("只签了钥匙不算接通：按钥匙最近一次真实调用显示「还没连通过 / 最近使用」", async () => {
    const s = server([{ id: "k1", kind: "ext", scope: "channel.slack" }, { id: "k3", kind: "ext", scope: "channel.mine", last_seen_at: "2030-01-01T10:42:00Z" }]);
    mount(s.impl);
    const a = within(await screen.findByRole("region", { name: "slack" }));
    expect(a.getByLabelText("连接状态")).toHaveTextContent("还没连通过");
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

describe("设置 › 提醒渠道：删除", () => {
  it("启用中的内置渠道也能删除：先停止发送，再删除；不改任何去处（指向它的由服务端改落今天页），不会悄悄改成下次聊天时告诉我", async () => {
    const s = server();
    conns.list.push({ id: "ch-a1", type: "bark", name: "我的 iPhone", config: {}, secrets: { device_key: true }, enabled: true, state: "ok" });
    s.doc.disturb.routes.decision = "channel:ch-a1";
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    mount(s.impl);
    const c = within(await screen.findByRole("region", { name: "我的 iPhone" }));
    await userEvent.click(c.getByRole("button", { name: "删除" }));
    await waitFor(() => expect(conns.calls).toEqual([
      { method: "POST", url: "/v1/connections/ch-a1/disable", body: {} },
      { method: "DELETE", url: "/v1/connections/ch-a1", body: undefined },
    ]));
    expect(confirm.mock.calls[0][0]).toMatch(/会先停止发送，指向它的推送改放在今天页的提醒里/);
    expect(s.puts).toHaveLength(0);
    expect(s.doc.disturb.routes.decision).toBe("channel:ch-a1");
    confirm.mockRestore();
  });

  it("删除一个自写程序渠道：一个请求吊销钥匙并删掉两处名字，卡片随之消失；取消则什么都不做", async () => {
    const s = server([
      { id: "k1", kind: "ext", scope: "channel.slack" },
      { id: "k2", kind: "ext", scope: "channel.keep" },
    ]);
    s.doc.channels = { slack: { text_only: true }, keep: {} };
    s.doc.channel_answerers = { slack: ["1"], keep: ["2"] };
    s.doc.disturb.routes.decision = "channel:slack";
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    mount(s.impl);
    const card = within(await screen.findByRole("region", { name: "slack" }));

    await userEvent.click(card.getByRole("button", { name: "删除" }));
    expect(s.dels).toEqual([]); // 确认框点了取消
    expect(screen.getByRole("region", { name: "slack" })).toBeInTheDocument();

    confirm.mockReturnValue(true);
    await userEvent.click(card.getByRole("button", { name: "删除" }));
    await waitFor(() => expect(screen.queryByRole("region", { name: "slack" })).not.toBeInTheDocument());
    expect(confirm.mock.calls.at(-1)![0]).toMatch(/钥匙全部停用/);
    expect(s.dels).toEqual(["slack"]);
    expect(s.calls).toEqual([]); // 页面不发拼状态的第二次请求
    expect(screen.getByRole("region", { name: "keep" })).toBeInTheDocument();
    expect(s.doc.channels.keep).toBeDefined();
    expect(s.doc.channel_answerers.keep).toEqual(["2"]);
    expect(s.doc.disturb.routes.decision).toBe("channel:slack"); // 路由不动，由「提醒与打扰」标明
    confirm.mockRestore();
  });
});

describe("设置 › 提醒渠道：给自己写的程序发钥匙的说明", () => {
  it("中文：适用场景、签了不等于接通、与内置渠道的区别、最小示例", async () => {
    const s = server();
    mount(s.impl);
    const f = within(await screen.findByRole("form", { name: "给自己写的程序发钥匙" }));
    expect(f.getByText(/你自己的脚本或机器人想收提醒/)).toBeInTheDocument();
    expect(f.getByText(/签了钥匙不等于接通/)).toBeInTheDocument();
    expect(f.getByText(/「添加渠道」接的是 APA 自带渠道/)).toBeInTheDocument();
    expect(f.getByText(/想让程序来取事项或接转发/)).toBeInTheDocument();
    expect(f.getByText("怎么调（最小示例）")).toBeInTheDocument();
    expect(f.getByText(/\/v1\/deliveries\/next\?wait=60/)).toBeInTheDocument();
    expect(f.getByText(/Authorization: Bearer/)).toBeInTheDocument();
    expect(f.getByText(/"ref":"msg-1"/)).toBeInTheDocument();
  });

  it("英文：同一份说明的英文文案", async () => {
    setChoice("en");
    const s = server();
    mount(s.impl);
    const f = within(await screen.findByRole("form", { name: "Issue a key for my own program" }));
    expect(f.getByText(/Your own script or bot that receives reminders/)).toBeInTheDocument();
    expect(f.getByText(/A key is not a connection/)).toBeInTheDocument();
    expect(f.getByText(/is for the types APA ships/)).toBeInTheDocument();
    expect(f.getByText(/Want a program to pull items or take forwards/)).toBeInTheDocument();
    expect(f.getByText("How to call it (minimal example)")).toBeInTheDocument();
    expect(f.getByText(/\/v1\/deliveries\/next\?wait=60/)).toBeInTheDocument();
  });
});

describe("设置 › 提醒渠道：不再放导出与转发密钥", () => {
  it("页面上没有导出与转发密钥的入口，「用自己的程序接入」仍在", async () => {
    mount(server().impl);
    expect(await screen.findByText("用自己的程序接入", { selector: "summary" })).toBeVisible();
    expect(screen.queryByText("导出与转发密钥")).not.toBeInTheDocument();
    expect(screen.queryByText("导出到文件夹")).not.toBeInTheDocument();
  });
});
