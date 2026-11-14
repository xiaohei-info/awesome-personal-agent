import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/settings/policy 的契约应答（三条路由按"段"GET/PUT，密钥读取时遮蔽为 ***，
// 人格超限、未知类别拒绝且不保存，非 Web 会话 403 web_only，错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const SECTIONS: Record<string, string[]> = {
  "/v1/settings/policy": ["web_only", "disturb", "material_write", "persona"],
  "/v1/settings/allowlists": ["forward_allowlist", "channel_answerers"],
  "/v1/settings/access-limits": ["permission_cap"],
};
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix it; nothing was saved" });

function server(opts: { webOnlyDenied?: boolean } = {}) {
  let version = 1;
  const doc: Record<string, any> = {
    web_only: { categories: ["funds", "irreversible"], rules: [{ kind: "payment", category: "funds" }] },
    disturb: { routes: { decision: "inject", health: "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] },
    material_write: { default: "proposed", kinds: { minutes: "direct" } },
    persona: "简洁",
    forward_allowlist: { telegram: ["alice"] },
    channel_answerers: {},
    permission_cap: { default: "read", by_harness: {} },
  };
  const puts: { path: string; body: any; csrf?: string }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    const keys = SECTIONS[url];
    if (!keys) return err(404, "not_found", "no such route");
    if (opts.webOnlyDenied) return err(403, "web_only", "settings can only be used by the owner in the Web UI");
    const render = () => ({ version, ...Object.fromEntries(keys.map((k) => [k, doc[k]])) });
    if (method === "GET") return json(200, render());
    const body = JSON.parse(init.body as string);
    puts.push({ path: url, body, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] });
    if (typeof body.persona === "string" && body.persona.length > 400) return err(400, "invalid_request", "persona is about 500 tokens, limit 400");
    if ((body.web_only?.rules ?? []).some((r: any) => !body.web_only.categories.includes(r.category))) return err(400, "invalid_request", "web_only rule category is not in categories");
    const before = JSON.stringify(doc);
    for (const [k, v] of Object.entries(body) as [string, any][]) {
      doc[k] = v;
      if (k === "web_only") doc[k] = { ...v, categories: [...v.categories].sort() };
    }
    if (JSON.stringify(doc) !== before) version++;
    return json(200, render());
  }) as unknown as typeof fetch;
  return { impl, puts, doc };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const card = async (name: string) => within(await screen.findByRole("region", { name }));
const save = async (name: string) => userEvent.click((await card(name)).getByRole("button", { name: `保存${name}` }));

beforeEach(() => sessionStorage.clear());

describe("策略与人格设置页", () => {
  it("flow-bg-guards ACT-BGG-08: 载入显示当前所有设置，密钥不回显", async () => {
    mount(server().impl);
    expect(await (await card("web_only 分类")).findByLabelText("类别（每行一个）")).toHaveValue("funds\nirreversible");
    expect((await card("打扰策略")).getByLabelText("打扰路由类别 1")).toHaveValue("decision");
    expect((await card("材料写入策略")).getByLabelText("材料 kind策略 1")).toHaveValue("direct");
    expect((await card("权限上限")).getByLabelText("默认上限")).toHaveValue("read");
    expect((await card("白名单")).getByLabelText("转发白名单发送者 1")).toHaveValue("alice");
    expect((await card("人格")).getByRole("textbox")).toHaveValue("简洁");
    // 模型端点移到"设置 › 模型"（settings-models 的测试覆盖密钥不回显），这里只留指引。
    expect(screen.queryByRole("region", { name: "生成端点" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "设置 › 模型" })).toHaveAttribute("href", "/settings/models");
  });

  it("flow-bg-guards ACT-BGG-08: 人格超限被服务端拒绝，页面显示原因且不保存", async () => {
    const s = server();
    mount(s.impl);
    const box = (await card("人格")).getByRole("textbox");
    await userEvent.clear(box);
    await userEvent.click(box);
    await userEvent.paste("长".repeat(500));
    await save("人格");
    expect(await (await card("人格")).findByRole("alert")).toHaveTextContent("limit 400");
    expect(s.doc.persona).toBe("简洁");
    expect(box).toHaveValue("长".repeat(500)); // 草稿保留，便于删减后重试
    await userEvent.clear(box);
    await userEvent.type(box, "直接");
    await save("人格");
    expect(await (await card("人格")).findByRole("status")).toHaveTextContent("已保存（版本 2）");
    expect(s.doc.persona).toBe("直接");
    expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/policy", body: { persona: "直接" }, csrf: "tok-1" });
  });

  it("flow-bg-guards ACT-BGG-08: 编辑 web_only 类别与规则，服务端拒绝未登记类别", async () => {
    const s = server();
    mount(s.impl);
    const c = await card("web_only 分类");
    await userEvent.click(c.getByRole("button", { name: "添加匹配规则" }));
    await userEvent.type(c.getByLabelText("规则2 tag"), "legal");
    await userEvent.type(c.getByLabelText("规则2 类别"), "external_commitment");
    await save("web_only 分类");
    expect(await c.findByRole("alert")).toHaveTextContent("not in categories");
    expect(s.doc.web_only.rules).toHaveLength(1);

    const cats = c.getByLabelText("类别（每行一个）");
    await userEvent.type(cats, "{Enter}external_commitment");
    await save("web_only 分类");
    expect(await c.findByRole("status")).toHaveTextContent("已保存");
    expect(s.doc.web_only.rules[1]).toMatchObject({ tag: "legal", category: "external_commitment" });
    expect(c.getByLabelText("类别（每行一个）")).toHaveValue("external_commitment\nfunds\nirreversible"); // 显示服务端规整后的结果
    await userEvent.click(c.getByRole("button", { name: "删除规则 2" }));
    await save("web_only 分类");
    await c.findByRole("status");
    expect(s.doc.web_only.rules).toHaveLength(1);
  });

  it("flow-bg-guards ACT-BGG-08: 打扰策略、材料写入、权限上限逐段保存，只发各自的段", async () => {
    const s = server();
    mount(s.impl);
    const d = await card("打扰策略");
    await userEvent.click(d.getByRole("button", { name: "添加打扰路由" }));
    await userEvent.type(d.getByLabelText("打扰路由类别 3"), "reminder:high");
    await userEvent.type(d.getByLabelText("打扰路由路由 3"), "web");
    await save("打扰策略");
    await d.findByRole("status");
    // 整段回传：静默、合并窗口、豁免等不被抹掉。
    expect(s.puts.at(-1)!.body).toEqual({ disturb: { routes: { decision: "inject", health: "web", "reminder:high": "web" }, merge_window_sec: 1800, quiet_exempt: true, daily_cap: 20, mutes: [] } });

    const m = await card("材料写入策略");
    await userEvent.selectOptions(m.getByLabelText("未登记 kind 的默认策略"), "direct");
    await userEvent.selectOptions(m.getByLabelText("材料 kind策略 1"), "proposed");
    await save("材料写入策略");
    await m.findByRole("status");
    expect(s.puts.at(-1)!.body).toEqual({ material_write: { default: "direct", kinds: { minutes: "proposed" } } });

    const l = await card("权限上限");
    await userEvent.click(l.getByRole("button", { name: "添加harness 上限" }));
    await userEvent.type(l.getByLabelText("harness 上限harness 1"), "hermes");
    await userEvent.selectOptions(l.getByLabelText("harness 上限上限 1"), "edit");
    await save("权限上限");
    await l.findByRole("status");
    expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/access-limits", body: { permission_cap: { default: "read", by_harness: { hermes: "edit" } } } });
  });

  it("flow-bg-guards ACT-BGG-08: 两份白名单独立编辑", async () => {
    const s = server();
    mount(s.impl);
    const a = await card("白名单");
    await userEvent.type(a.getByLabelText("转发白名单发送者 1"), ", bob");
    await userEvent.click(a.getByRole("button", { name: "添加渠道回答人" }));
    await userEvent.type(a.getByLabelText("渠道回答人渠道 1"), "slack");
    await userEvent.type(a.getByLabelText("渠道回答人回答人 1"), "carol");
    await save("白名单");
    await a.findByRole("status");
    expect(s.puts.at(-1)).toMatchObject({ path: "/v1/settings/allowlists", body: { forward_allowlist: { telegram: ["alice", "bob"] }, channel_answerers: { slack: ["carol"] } } });

  });

  it("保存同一路由下的另一张卡片，不会清掉本卡片未保存的编辑", async () => {
    const s = server();
    mount(s.impl);
    const d = await card("打扰策略");
    await userEvent.click(d.getByRole("button", { name: "添加打扰路由" }));
    await userEvent.type(d.getByLabelText("打扰路由类别 3"), "wake");
    const p = (await card("人格")).getByRole("textbox");
    await userEvent.clear(p);
    await userEvent.type(p, "直接");
    await save("人格");
    await (await card("人格")).findByRole("status");
    expect(d.getByLabelText("打扰路由类别 3")).toHaveValue("wake"); // 未保存的草稿仍在
    expect(s.doc.disturb.routes).toEqual({ decision: "inject", health: "web" });
  });

  it("ACT-DLV-16/24: 在 Web 设置日上限与静音并保存", async () => {
    const s = server();
    mount(s.impl);
    const d = await card("打扰策略");
    await userEvent.clear(d.getByLabelText("日上限"));
    await userEvent.type(d.getByLabelText("日上限"), "3");
    await userEvent.click(d.getByRole("button", { name: "添加静音" }));
    await userEvent.selectOptions(d.getByLabelText("静音1 范围"), "project");
    await userEvent.type(d.getByLabelText("静音1 id"), "acc-home");
    await save("打扰策略");
    await d.findByRole("status");
    expect(s.puts.at(-1)!.body.disturb).toMatchObject({ daily_cap: 3, mutes: [{ scope: "project", id: "acc-home" }], merge_window_sec: 1800 });
    expect(s.doc.disturb.mutes).toEqual([{ scope: "project", id: "acc-home" }]);

    await userEvent.type(d.getByLabelText("静音1 到期"), "2030-01-01T10:00");
    await save("打扰策略");
    await d.findByRole("status");
    expect(s.doc.disturb.mutes[0].until).toBe(new Date("2030-01-01T10:00").toISOString());

    await userEvent.click(d.getByRole("button", { name: "删除静音 1" }));
    await userEvent.clear(d.getByLabelText("日上限"));
    await userEvent.type(d.getByLabelText("日上限"), "0");
    await save("打扰策略");
    await d.findByRole("status");
    expect(s.doc.disturb).toMatchObject({ daily_cap: 0, mutes: [] });
  });

  it("日上限不是非负整数时在本地提示，不发请求", async () => {
    const s = server();
    mount(s.impl);
    const d = await card("打扰策略");
    const before = s.puts.length;
    await userEvent.clear(d.getByLabelText("日上限"));
    await userEvent.type(d.getByLabelText("日上限"), "-2");
    await save("打扰策略");
    expect(await d.findByRole("alert")).toHaveTextContent("日上限");
    expect(s.puts.length).toBe(before);
  });

  it("同一 map 内重复的键在本地提示，不发请求", async () => {
    const s = server();
    mount(s.impl);
    const d = await card("打扰策略");
    await userEvent.click(d.getByRole("button", { name: "添加打扰路由" }));
    await userEvent.type(d.getByLabelText("打扰路由类别 3"), "decision");
    await userEvent.type(d.getByLabelText("打扰路由路由 3"), "web");
    await save("打扰策略");
    expect(await d.findByRole("alert")).toHaveTextContent("重复");
    expect(s.puts).toHaveLength(0);
  });

  it("flow-web-only: 非 Web 会话读取被拒绝时显示 web_only 原因并可重试", async () => {
    mount(server({ webOnlyDenied: true }).impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("only be used by the owner in the Web UI");
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
