import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按附录 B.2 的 /settings/model-endpoints 与两条探测路由应答。
// 读取时只给 api_key_set 与末 4 位；api_key 缺省或为 *** 保持原密钥，"" 清除。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function server(init: { judge?: any; generate?: any } = {}) {
  let version = 1;
  const keys: Record<string, string> = { judge_endpoint: "", generate_endpoint: "sk-test-key-9f2c" };
  const doc: Record<string, any> = {
    judge_endpoint: "judge" in init ? init.judge : { url: "http://cls.local/judge", api_key_set: false, api_key_hint: "", protocol: "probability-judgment/1", model: "", locality: "local", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 },
    generate_endpoint: "generate" in init ? init.generate : { url: "https://llm.example.com/v1", api_key_set: true, api_key_hint: "9f2c", model: "model-a", locality: "external" },
    screening_timeout_ms: 30000,
  };
  const puts: any[] = [];
  const posts: { path: string; body: any }[] = [];
  let models: any = { ok: true, models: ["model-a", "model-b"], truncated: false, elapsed_ms: 80 };
  let probe: any = { subject: "generate_endpoint", status: "ok", code: "ok", why: "answered the structured-output sample request in 820 ms", data: { elapsed_ms: 820, model: "model-b" } };
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ outbound: { local: "full", external: null } }, { outbound: { local: "full", external: "meta" } }, {}] });
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    if (url === "/v1/settings/model-endpoints/models") return posts.push({ path: url, body }), json(200, models);
    if (url === "/v1/settings/model-endpoints/test") return posts.push({ path: url, body }), json(200, probe);
    if (url !== "/v1/settings/model-endpoints") return json(404, { code: "not_found", why: "no such route", next: "x" });
    if (method === "PUT") {
      puts.push(body);
      for (const k of ["judge_endpoint", "generate_endpoint"]) {
        if (!(k in body)) continue;
        if (body[k] && !/^https?:\/\//.test(body[k].url)) return json(400, { code: "invalid_request", why: "bad endpoint url", next: "fix it; nothing was saved" });
        if (!body[k]) { doc[k] = null; continue; }
        const { api_key, ...rest } = body[k];
        if (api_key !== undefined && api_key !== "***") keys[k] = api_key;
        doc[k] = { ...rest, api_key_set: keys[k] !== "", api_key_hint: keys[k].length >= 12 ? keys[k].slice(-4) : "" };
      }
      if ("screening_timeout_ms" in body) doc.screening_timeout_ms = body.screening_timeout_ms;
      version++;
    }
    return json(200, { version, ...doc });
  }) as unknown as typeof fetch;
  return { impl, puts, posts, doc, keys, setModels: (m: any) => (models = m), setProbe: (p: any) => (probe = p) };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/models"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const card = async (name: string) => within(await screen.findByRole("region", { name }));
const save = async (name: string) => userEvent.click((await card(name)).getByRole("button", { name: `保存${name}` }));

beforeEach(() => sessionStorage.clear());

describe("设置 › 模型", () => {
  it("flow-bg-guards ACT-BGG-08 / flow-endpoint-config ACT-EPC-02: 两个端点分开显示与保存，密钥只显示已设置与末 4 位", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("生成端点");
    expect(g.getByRole("group", { name: "API 密钥" })).toHaveTextContent("已设置 ····9f2c");
    expect(document.body).not.toHaveTextContent("sk-test-key");
    expect(g.getByLabelText("模型")).toHaveValue("model-a");
    expect(g.getByRole("radio", { name: /外部服务/ })).toBeChecked();

    const j = await card("判断端点");
    expect(j.getByLabelText("协议")).toHaveValue("probability-judgment/1");
    expect(j.getByRole("radio", { name: /本机或我自己控制的主机/ })).toBeChecked();
    await userEvent.type(j.getByLabelText("API 密钥"), "acc-cls-key");
    await userEvent.clear(j.getByLabelText("超时（毫秒）"));
    await userEvent.type(j.getByLabelText("超时（毫秒）"), "8000");
    await save("判断端点");
    await j.findByRole("status", {}, { timeout: 2000 });
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: { url: "http://cls.local/judge", api_key: "acc-cls-key", model: "", locality: "local", protocol: "probability-judgment/1", timeout_ms: 8000, max_batch: 32, max_material_chars: 8000 } });

    await save("生成端点");
    await g.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ generate_endpoint: { url: "https://llm.example.com/v1", api_key: "***", model: "model-a", locality: "external" } });
    expect(s.keys.generate_endpoint).toBe("sk-test-key-9f2c"); // 占位沿用原密钥
  });

  it("flow-judge-protocol ACT-JDP-11: 判断端点协议是 system-one 与 probability-judgment/1，不开放 chat-logprobs；已存的自建服务配置原样显示、没有获取模型", async () => {
    mount(server().impl);
    const j = await card("判断端点");
    expect(within(j.getByLabelText("协议")).getAllByRole("option").map((o) => (o as HTMLOptionElement).value)).toEqual(["system-one", "probability-judgment/1"]);
    expect(j.getByLabelText("协议")).toHaveValue("probability-judgment/1");
    expect(j.getByLabelText(/端点 URL/)).toHaveValue("http://cls.local/judge");
    expect(j.queryByRole("button", { name: "获取模型" })).toBeNull();
  });

  it("新配判断端点默认 System One：预填官方地址与模型，可获取模型、测试连接并保存；改成网关地址照存", async () => {
    const s = server({ judge: null });
    s.setModels({ ok: true, models: ["jev-latest", "jev-preview"], truncated: false, elapsed_ms: 50 });
    s.setProbe({ subject: "judge_endpoint", status: "ok", code: "ok", why: "answered", data: { elapsed_ms: 300, model: "jev-1.14.0", probability_source: "native" } });
    mount(s.impl);
    const j = await card("判断端点");
    await userEvent.click(j.getByLabelText("配置判断端点"));
    expect(j.getByLabelText("协议")).toHaveValue("system-one");
    expect(j.getByLabelText(/接口根地址/)).toHaveValue("https://api.typesafe.ai/v1");
    expect(j.getByLabelText("模型")).toHaveValue("jev-latest");
    await userEvent.type(j.getByLabelText("API 密钥"), "ts-key");
    await userEvent.click(j.getByRole("button", { name: "获取模型" }));
    await j.findByText("已从端点取到 2 个模型。");
    expect(s.posts.at(-1)!.body).toEqual({ endpoint: "judge", url: "https://api.typesafe.ai/v1", api_key: "ts-key", protocol: "system-one" });
    await userEvent.click(j.getByRole("radio", { name: /外部服务/ }));
    await userEvent.click(j.getByRole("button", { name: "测试连接" }));
    expect(await j.findByRole("status", { name: "测试结果" })).toHaveTextContent("概率来源 native");
    const url = j.getByLabelText(/接口根地址/);
    await userEvent.clear(url);
    await userEvent.type(url, "http://gw.example.invalid/v1");
    await userEvent.click(j.getByRole("button", { name: "手动填写模型名" }));
    await userEvent.clear(j.getByLabelText("模型"));
    await userEvent.type(j.getByLabelText("模型"), "gw/jev-latest");
    await save("判断端点");
    await j.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: { url: "http://gw.example.invalid/v1", api_key: "ts-key", model: "gw/jev-latest", locality: "external", protocol: "system-one", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 } });
  });

  it("获取模型后从列表选择，再测试连接；请求用占位沿用已存密钥", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("生成端点");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    expect(await g.findByText("已从端点取到 2 个模型。")).toBeInTheDocument();
    expect(s.posts.at(-1)).toEqual({ path: "/v1/settings/model-endpoints/models", body: { endpoint: "generate", url: "https://llm.example.com/v1", api_key: "***" } });
    await userEvent.selectOptions(g.getByLabelText("模型"), "model-b");
    await userEvent.click(g.getByRole("button", { name: "测试连接" }));
    const r = await g.findByRole("status", { name: "测试结果" });
    expect(r).toHaveTextContent("✓ 连通，鉴权通过");
    expect(r).toHaveTextContent("耗时 820 ms");
    expect(s.posts.at(-1)).toEqual({ path: "/v1/settings/model-endpoints/test", body: { endpoint: "generate", config: { url: "https://llm.example.com/v1", api_key: "***", model: "model-b", locality: "external" } } });
    expect(s.puts).toHaveLength(0); // 获取与测试都不保存
  });

  it("拉不到模型列表时说明原因并改为手动填写；测试失败按机器码显示说明", async () => {
    const s = server();
    s.setModels({ ok: false, reason: "not_found", status: 404, elapsed_ms: 30 });
    s.setProbe({ subject: "generate_endpoint", status: "error", code: "unauthorized", why: "the endpoint returned HTTP status 401", next: "check URL, API key and model name", data: { elapsed_ms: 40, status: 401 } });
    mount(s.impl);
    const g = await card("生成端点");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    expect(await g.findByText(/这个端点不提供模型列表（HTTP 404）。可以手动填写模型名/)).toBeInTheDocument();
    await userEvent.clear(g.getByLabelText("模型"));
    await userEvent.type(g.getByLabelText("模型"), "my-model");
    await userEvent.click(g.getByRole("button", { name: "测试连接" }));
    const r = await g.findByRole("status", { name: "测试结果" });
    expect(r).toHaveTextContent("✗ 鉴权失败");
    expect(r).toHaveTextContent("HTTP 401");
    expect(s.posts.at(-1)!.body.config.model).toBe("my-model");
  });

  it("地址换到另一个主机时不再沿用已存密钥，要求重新输入", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("生成端点");
    const url = g.getByLabelText("端点 URL");
    await userEvent.clear(url);
    await userEvent.type(url, "https://other.example.com/v1");
    expect(g.getByText(/已存的密钥不会发到新地址，请重新输入/)).toBeInTheDocument();
    await userEvent.type(g.getByLabelText("API 密钥"), "sk-other");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    await g.findByText(/已从端点取到/);
    expect(s.posts.at(-1)!.body).toEqual({ endpoint: "generate", url: "https://other.example.com/v1", api_key: "sk-other" });
  });

  it("外部端点提示还没设置外部外发范围的来源数，并链接到来源页", async () => {
    mount(server().impl);
    const g = await card("生成端点");
    expect(await g.findByText(/有 2 个来源还没设置外部外发范围/)).toBeInTheDocument();
    expect(g.getByRole("link", { name: "去来源页设置" })).toHaveAttribute("href", "/sources");
  });

  it("flow-endpoint-config: 两个都没配时有空状态；新配端点的位置没有预选，选之前不保存；取消配置发 null", async () => {
    const s = server({ judge: null, generate: null });
    mount(s.impl);
    expect(await screen.findByRole("region", { name: "还没有配置模型端点" })).toHaveTextContent("不配也能用");
    const j = await card("判断端点");
    expect(j.queryByLabelText(/端点 URL/)).toBeNull();
    await userEvent.click(j.getByLabelText("配置判断端点"));
    expect(j.getByRole("radio", { name: /本机/ })).not.toBeChecked();
    expect(j.getByRole("radio", { name: /外部服务/ })).not.toBeChecked();
    expect(j.getByLabelText("超时（毫秒）")).toHaveValue(10000);
    // 换成自建服务协议：仍是预填值的地址与模型清空。
    await userEvent.selectOptions(j.getByLabelText("协议"), "probability-judgment/1");
    expect(j.getByLabelText(/端点 URL/)).toHaveValue("");
    await userEvent.type(j.getByLabelText(/端点 URL/), "http://cls.stub/judge");
    await save("判断端点");
    expect(await j.findByRole("alert")).toHaveTextContent("配置这个端点时必选");
    expect(s.puts).toHaveLength(0);
    await userEvent.click(j.getByRole("radio", { name: /本机/ }));
    await save("判断端点");
    await j.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: { url: "http://cls.stub/judge", api_key: "", model: "", locality: "local", protocol: "probability-judgment/1", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 } });

    await userEvent.click(j.getByLabelText("配置判断端点"));
    await save("判断端点");
    await j.findByText(/已保存（版本 3）/);
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: null });
  });

  it("flow-endpoint-config: 服务端拒绝时显示原因，草稿保留；筛选超时单独保存，非法值本地提示", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("生成端点");
    const url = g.getByLabelText("端点 URL");
    await userEvent.clear(url);
    await userEvent.type(url, "not a url");
    await userEvent.type(g.getByLabelText("API 密钥"), "k");
    await save("生成端点");
    expect(await g.findByRole("alert")).toHaveTextContent("bad endpoint url");
    expect(url).toHaveValue("not a url");

    const t = await card("筛选超时");
    await userEvent.clear(t.getByLabelText("筛选超时（毫秒）"));
    await userEvent.type(t.getByLabelText("筛选超时（毫秒）"), "0");
    await save("筛选超时");
    expect(await t.findByRole("alert")).toHaveTextContent("筛选超时必须是正整数");
    await userEvent.clear(t.getByLabelText("筛选超时（毫秒）"));
    await userEvent.type(t.getByLabelText("筛选超时（毫秒）"), "45000");
    await save("筛选超时");
    await t.findByRole("status");
    expect(s.puts.at(-1)).toEqual({ screening_timeout_ms: 45000 });
  });
});
