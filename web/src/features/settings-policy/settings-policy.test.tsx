import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/settings/policy 的契约应答（四条路由按"段"GET/PUT，密钥读取时遮蔽为 ***，
// 人格超限、未知类别拒绝且不保存，非 Web 会话 403 web_only，错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const SECTIONS: Record<string, string[]> = {
  "/v1/settings/policy": ["web_only", "disturb", "material_write", "persona"],
  "/v1/settings/allowlists": ["forward_allowlist", "channel_answerers"],
  "/v1/settings/model-endpoints": ["judge_endpoint", "generate_endpoint", "screening_timeout_ms"],
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
    judge_endpoint: { url: "http://cls.local/v1", api_key_set: true, protocol: "probability-judgment/1", model: "", locality: "local", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 },
    generate_endpoint: { url: "https://m.example/v1", api_key_set: true, model: "m", locality: "external" },
    screening_timeout_ms: 30000,
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
    for (const k of ["judge_endpoint", "generate_endpoint"]) if (body[k] && !/^https?:\/\//.test(body[k].url)) return err(400, "invalid_request", "bad endpoint url");
    const before = JSON.stringify(doc);
    for (const [k, v] of Object.entries(body) as [string, any][]) {
      if ((k === "judge_endpoint" || k === "generate_endpoint") && v) {
        // 与服务端一致：api_key 缺省或占位 *** 保持原密钥；读取只给 api_key_set。
        const { api_key, ...rest } = v;
        doc[k] = { ...rest, api_key_set: api_key === undefined || api_key === "***" ? !!doc[k]?.api_key_set : api_key !== "" };
        continue;
      }
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
    const key = (await card("生成端点")).getByLabelText("API 密钥");
    expect(key).toHaveValue("***");
    expect(key).toHaveAttribute("type", "password");
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

  it("flow-endpoint-config ACT-EPC-02: 判断端点与生成端点分开配置，密钥只写不读，保存后显示服务端结果", async () => {
    const s = server();
    mount(s.impl);
    const j = await card("判断端点");
    expect(j.getByLabelText("协议")).toHaveValue("probability-judgment/1");
    expect(j.getByLabelText("位置（locality）")).toHaveValue("local");
    expect(j.getByLabelText("API 密钥")).toHaveValue("***"); // 已设置：占位，不回显明文
    const g = await card("生成端点");
    expect(g.getByLabelText("模型")).toHaveValue("m");
    expect(g.getByLabelText("位置（locality）")).toHaveValue("external");

    await userEvent.clear(j.getByLabelText("端点 URL"));
    await userEvent.type(j.getByLabelText("端点 URL"), "http://cls.stub");
    await userEvent.clear(j.getByLabelText("API 密钥"));
    await userEvent.type(j.getByLabelText("API 密钥"), "acc-cls-key");
    await userEvent.clear(j.getByLabelText("超时（毫秒）"));
    await userEvent.type(j.getByLabelText("超时（毫秒）"), "8000");
    await save("判断端点");
    await j.findByRole("status");
    expect(s.puts.at(-1)).toMatchObject({
      path: "/v1/settings/model-endpoints",
      body: { judge_endpoint: { url: "http://cls.stub", api_key: "acc-cls-key", protocol: "probability-judgment/1", locality: "local", timeout_ms: 8000, max_batch: 32, max_material_chars: 8000 } },
    });
    expect(Object.keys(s.puts.at(-1)!.body)).toEqual(["judge_endpoint"]); // 只发本段，生成端点不受影响
    expect(s.doc.generate_endpoint.url).toBe("https://m.example/v1");
    expect(j.getByLabelText("API 密钥")).toHaveValue("***"); // 保存后仍不回显

    await userEvent.clear(g.getByLabelText("端点 URL"));
    await userEvent.type(g.getByLabelText("端点 URL"), "http://model.stub");
    await userEvent.selectOptions(g.getByLabelText("位置（locality）"), "local");
    await save("生成端点");
    await g.findByRole("status");
    expect(s.puts.at(-1)!.body).toEqual({ generate_endpoint: { url: "http://model.stub", api_key: "***", model: "m", locality: "local" } });
    expect(s.doc.judge_endpoint.url).toBe("http://cls.stub");
  });

  it("flow-judge-protocol ACT-JDP-11: 判断端点协议下拉框只有 probability-judgment/1，没有 chat-logprobs", async () => {
    mount(server().impl);
    const options = within((await card("判断端点")).getByLabelText("协议")).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["probability-judgment/1"]);
  });

  it("flow-endpoint-config: 不配判断端点时发 null；新建判断端点带附录 B 的默认值，位置默认 external", async () => {
    const s = server();
    s.doc.judge_endpoint = null;
    mount(s.impl);
    const j = await card("判断端点");
    expect(j.queryByLabelText("端点 URL")).toBeNull();
    await userEvent.click(j.getByLabelText("配置判断端点"));
    expect(j.getByLabelText("位置（locality）")).toHaveValue("external");
    expect(j.getByLabelText("超时（毫秒）")).toHaveValue(10000);
    expect(j.getByLabelText("批量上限")).toHaveValue(32);
    await userEvent.type(j.getByLabelText("端点 URL"), "http://cls.stub");
    await save("判断端点");
    await j.findByRole("status");
    expect(s.puts.at(-1)!.body).toEqual({ judge_endpoint: { url: "http://cls.stub", api_key: "", protocol: "probability-judgment/1", model: "", locality: "external", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 } });

    await userEvent.click(j.getByLabelText("配置判断端点")); // 取消即清除
    await save("判断端点");
    await j.findByRole("status");
    expect(s.puts.at(-1)!.body).toEqual({ judge_endpoint: null });
    expect(s.doc.judge_endpoint).toBeNull();
  });

  it("flow-endpoint-config: 筛选超时单独保存；数值不合法时本地提示不发请求", async () => {
    const s = server();
    mount(s.impl);
    const t = await card("筛选超时");
    await userEvent.clear(t.getByLabelText("筛选超时（毫秒）"));
    await userEvent.type(t.getByLabelText("筛选超时（毫秒）"), "45000");
    await save("筛选超时");
    await t.findByRole("status");
    expect(s.puts.at(-1)!.body).toEqual({ screening_timeout_ms: 45000 });

    const j = await card("判断端点");
    await userEvent.clear(j.getByLabelText("批量上限"));
    await userEvent.type(j.getByLabelText("批量上限"), "0");
    const before = s.puts.length;
    await save("判断端点");
    expect(await j.findByRole("alert")).toHaveTextContent("批量上限");
    expect(s.puts.length).toBe(before);
  });

  it("flow-endpoint-config: 服务端拒绝端点配置时显示原因，草稿保留", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("生成端点");
    const url = g.getByLabelText("端点 URL");
    await userEvent.clear(url);
    await userEvent.type(url, "not a url");
    await save("生成端点");
    expect(await g.findByRole("alert")).toHaveTextContent("bad endpoint");
    expect(url).toHaveValue("not a url");
    expect(s.doc.generate_endpoint.url).toBe("https://m.example/v1");
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
