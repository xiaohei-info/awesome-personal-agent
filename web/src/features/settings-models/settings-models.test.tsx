import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按附录 B.2 的 /settings/model-endpoints 与两条探测路由应答。
// 读取时只给 api_key_set 与末 4 位；api_key 缺省或为 *** 保持原密钥，"" 清除。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function server(init: { judge?: any; generate?: any; days?: number } = {}) {
  const days = init.days ?? 7;
  let version = 1;
  const keys: Record<string, string> = { judge_endpoint: "", generate_endpoint: "sk-test-key-9f2c" };
  const doc: Record<string, any> = {
    judge_endpoint: "judge" in init ? init.judge : { url: "http://cls.local/judge", api_key_set: false, api_key_hint: "", protocol: "probability-judgment/1", model: "", locality: "local", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 },
    generate_endpoint: "generate" in init ? init.generate : { url: "https://llm.example.com/v1", api_key_set: true, api_key_hint: "9f2c", model: "model-a", locality: "external" },
    models_paused: false,
  };
  const puts: any[] = [];
  const posts: { path: string; body: any }[] = [];
  let models: any = { ok: true, models: ["model-a", "model-b"], truncated: false, elapsed_ms: 80 };
  let probe: any = { subject: "generate_endpoint", status: "ok", code: "ok", why: "answered the structured-output sample request in 820 ms", data: { elapsed_ms: 820, model: "model-b" } };
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [{ id: "p_chat", name: "聊天项目", capture: true, capture_outbound: null }, { id: "p_x", name: "不记聊天", capture: false }] });
    if (url === "/v1/settings/projects/p_chat/capture-outbound" && method === "PUT") return puts.push({ capture: JSON.parse(init.body as string), csrf: (init.headers as Record<string, string>)?.["X-CSRF-Token"] ?? null }), json(200, { value: JSON.parse(init.body as string).value });
    if (url === "/v1/health") return json(200, { healthy: true, signals: [{ id: "unjudged_backlog", ok: false, detail: { models_paused: 14, rejudge_days: days } }] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "s1", name: "甲来源", outbound: { local: "full", external: null } }, { id: "s2", name: "乙来源", outbound: { local: "full", external: "meta" } }, { id: "s3", name: "丙来源" }] });
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
      if ("models_paused" in body) doc.models_paused = body.models_paused;
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
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
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
    const g = await card("写作模型");
    expect(g.getByRole("group", { name: "API 密钥" })).toHaveTextContent("已设置 ····9f2c");
    expect(document.body).not.toHaveTextContent("sk-test-key");
    expect(g.getByLabelText("模型")).toHaveValue("model-a");
    expect(g.getByRole("radio", { name: /外部服务/ })).toBeChecked();

    const j = await card("打分模型");
    expect(j.getByLabelText("协议")).toHaveValue("probability-judgment/1");
    expect(j.getByRole("radio", { name: /本机或我自己控制的主机/ })).toBeChecked();
    await userEvent.type(j.getByLabelText("API 密钥"), "acc-cls-key");
    await userEvent.clear(j.getByLabelText("超时（秒）"));
    await userEvent.type(j.getByLabelText("超时（秒）"), "8");
    await save("打分模型");
    await j.findByRole("status", {}, { timeout: 2000 });
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: { url: "http://cls.local/judge", api_key: "acc-cls-key", model: "", locality: "local", protocol: "probability-judgment/1", timeout_ms: 8000, max_batch: 32, max_material_chars: 8000 } });

    await save("写作模型");
    await g.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ generate_endpoint: { url: "https://llm.example.com/v1", api_key: "***", model: "model-a", locality: "external", protocol: "chat-completions", max_material_tokens: 16000 } });
    expect(s.keys.generate_endpoint).toBe("sk-test-key-9f2c"); // 占位沿用原密钥
  });

  it("flow-material-budget ACT-BUD-01: 写作资料上限缺省显示 16000 token（原 24000 字），说明写明估算系数、范围、当前模型与超出后果；0、负数、过小、过大、小数、空都被拦并写明范围，草稿保留", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("写作模型");
    const f = g.getByLabelText("写作资料上限（token）");
    expect(f).toHaveValue(16000);
    const note = g.getByRole("note");
    expect(note).toHaveTextContent("这是写作任务打包给模型的资料上限");
    expect(note).toHaveTextContent("按约 1.5 个字符 ≈ 1 token 估算（不调用 tokenizer）");
    expect(note).toHaveTextContent("默认 16000（约合原先的 24000 字），可填 2000 到 500000");
    expect(note).toHaveTextContent("当前写作模型：model-a");
    expect(note).toHaveTextContent("这个数要装得进该模型的上下文窗口");
    expect(note).toHaveTextContent("超出会让那次调用失败，并按原有的失败处理重试");
    expect(g.queryByText(/max_material|budget_tokens/)).toBeNull(); // 文案不出现内部字段名

    for (const bad of ["0", "-1", "1999", "500001", "2500.5", ""]) {
      await userEvent.clear(f);
      if (bad) await userEvent.type(f, bad);
      await save("写作模型");
      await g.findByText("资料上限填 2000 到 500000 之间的整数（token）");
      expect(s.puts, bad).toHaveLength(0);
      if (bad) expect(f).toHaveValue(Number(bad));
    }

    await userEvent.clear(f);
    await userEvent.type(f, "500000");
    await save("写作模型");
    await g.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ generate_endpoint: { url: "https://llm.example.com/v1", api_key: "***", model: "model-a", locality: "external", protocol: "chat-completions", max_material_tokens: 500000 } });
    expect(s.doc.generate_endpoint.max_material_tokens).toBe(500000);
    expect(g.getByLabelText("写作资料上限（token）")).toHaveValue(500000);
  });

  it("flow-material-budget ACT-BUD-01: 新配写作模型时资料上限预填 16000", async () => {
    const s = server({ generate: null });
    mount(s.impl);
    const g = await card("写作模型");
    await userEvent.click(g.getByLabelText("配置写作模型"));
    expect(g.getByLabelText("写作资料上限（token）")).toHaveValue(16000);
  });

  it("flow-judge-protocol ACT-JDP-11: 判断端点协议是 system-one 与 probability-judgment/1，不开放 chat-logprobs；已存的自建服务配置原样显示、没有获取模型", async () => {
    mount(server().impl);
    const j = await card("打分模型");
    expect(within(j.getByLabelText("协议")).getAllByRole("option").map((o) => (o as HTMLOptionElement).value)).toEqual(["system-one", "probability-judgment/1"]);
    expect(j.getByLabelText("协议")).toHaveValue("probability-judgment/1");
    expect(j.getByLabelText(/地址/)).toHaveValue("http://cls.local/judge");
    expect(j.queryByRole("button", { name: "获取模型" })).toBeNull();
  });

  it("新配判断端点默认 System One：预填官方地址与模型，可获取模型、测试连接并保存；改成网关地址照存", async () => {
    const s = server({ judge: null });
    s.setModels({ ok: true, models: ["jev-latest", "jev-preview"], truncated: false, elapsed_ms: 50 });
    s.setProbe({ subject: "judge_endpoint", status: "ok", code: "ok", why: "answered", data: { elapsed_ms: 300, model: "jev-1.14.0", probability_source: "native" } });
    mount(s.impl);
    const j = await card("打分模型");
    await userEvent.click(j.getByLabelText("配置打分模型"));
    expect(j.getByLabelText("协议")).toHaveValue("system-one");
    expect(j.getByLabelText(/接口根地址/)).toHaveValue("https://api.typesafe.ai/v1");
    expect(j.getByLabelText("模型")).toHaveValue("jev-latest");
    expect(j.getByLabelText("超时（秒）")).toHaveValue(30);
    expect(j.getByText(/首次调用约需 10 秒/)).toBeInTheDocument();
    expect(j.getByText("将请求：https://api.typesafe.ai/v1/systemone")).toBeInTheDocument();
    await userEvent.type(j.getByLabelText("API 密钥"), "ts-key");
    await userEvent.click(j.getByRole("button", { name: "获取模型" }));
    await j.findByText("已从端点取到 2 个模型。");
    expect(s.posts.at(-1)!.body).toEqual({ endpoint: "judge", url: "https://api.typesafe.ai/v1", api_key: "ts-key", protocol: "system-one" });
    await userEvent.click(j.getByRole("radio", { name: /外部服务/ }));
    await userEvent.click(j.getByRole("button", { name: "测试连接" }));
    expect(await j.findByRole("status", { name: "测试结果" })).toHaveTextContent("模型原生概率");
    const url = j.getByLabelText(/接口根地址/);
    await userEvent.clear(url);
    await userEvent.type(url, "http://gw.example.invalid/v1");
    await userEvent.click(j.getByRole("button", { name: "手动填写模型名" }));
    await userEvent.clear(j.getByLabelText("模型"));
    await userEvent.type(j.getByLabelText("模型"), "gw/jev-latest");
    await save("打分模型");
    await j.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: { url: "http://gw.example.invalid/v1", api_key: "ts-key", model: "gw/jev-latest", locality: "external", protocol: "system-one", timeout_ms: 30000, max_batch: 32, max_material_chars: 8000 } });
  });

  it("生成端点的协议在第一项下拉：旧配置没写协议显示 OpenAI Chat Completions；换协议换地址示例与提示，获取模型、测试与保存都带协议，结果写明原生结构化输出", async () => {
    const s = server({ generate: null });
    mount(s.impl);
    const g = await card("写作模型");
    await userEvent.click(g.getByRole("checkbox", { name: "配置写作模型" }));
    const proto = g.getByLabelText("协议");
    expect(g.getAllByRole("combobox")[0]).toBe(proto); // 第一项
    expect(proto).toHaveValue("chat-completions");
    expect(within(proto).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "OpenAI Chat Completions",
      "OpenAI Responses",
      "Anthropic Messages",
      "Google Gemini 原生",
    ]);
    expect(g.getByText(/最通用/)).toBeInTheDocument();
    expect(g.getByLabelText("地址")).toHaveAttribute("placeholder", "https://llm.example.com/v1");
    await userEvent.selectOptions(proto, "messages");
    expect(g.getByLabelText("地址")).toHaveAttribute("placeholder", "https://api.anthropic.com/v1");
    expect(g.getByLabelText("地址")).toHaveValue(""); // 不预填
    await userEvent.type(g.getByLabelText("地址"), "https://gw.example.invalid/v1");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    expect(s.posts.at(-1)!.body).toMatchObject({ endpoint: "generate", protocol: "messages" });
    await userEvent.selectOptions(g.getByLabelText("模型"), "model-a");
    s.setProbe({ subject: "generate_endpoint", status: "ok", code: "ok", why: "answered", data: { elapsed_ms: 900, protocol: "messages", structured_output: "tool_use" } });
    await userEvent.click(g.getByRole("button", { name: "测试连接" }));
    expect(await g.findByRole("status", { name: "测试结果" })).toHaveTextContent("Anthropic Messages · 原生结构化输出（工具调用）");
    expect(s.posts.at(-1)!.body.config.protocol).toBe("messages");
    await userEvent.selectOptions(proto, "generate-content");
    expect(g.getByText(/Google Gemini 原生接口/)).toBeInTheDocument();
    s.setProbe({ subject: "generate_endpoint", status: "ok", code: "ok", why: "answered", data: { elapsed_ms: 650, protocol: "generate-content", structured_output: "responseJsonSchema" } });
    await userEvent.click(g.getByRole("button", { name: "测试连接" }));
    expect(await g.findByRole("status", { name: "测试结果" })).toHaveTextContent("Google Gemini 原生 · 原生结构化输出（JSON 结构约束）");
    expect(s.posts.at(-1)!.body.config.protocol).toBe("generate-content");
    await userEvent.click(g.getByRole("radio", { name: /外部服务/ }));
    await save("写作模型");
    expect(s.puts.at(-1)).toEqual({ generate_endpoint: { url: "https://gw.example.invalid/v1", api_key: "", model: "model-a", locality: "external", protocol: "generate-content", max_material_tokens: 16000 } });
  });

  it("获取模型后从列表选择，再测试连接；请求用占位沿用已存密钥", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("写作模型");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    expect(await g.findByText("已从端点取到 2 个模型。")).toBeInTheDocument();
    expect(s.posts.at(-1)).toEqual({ path: "/v1/settings/model-endpoints/models", body: { endpoint: "generate", url: "https://llm.example.com/v1", api_key: "***", protocol: "chat-completions" } });
    await userEvent.selectOptions(g.getByLabelText("模型"), "model-b");
    await userEvent.click(g.getByRole("button", { name: "测试连接" }));
    const r = await g.findByRole("status", { name: "测试结果" });
    expect(r).toHaveTextContent("✓ 连通，鉴权通过");
    expect(r).toHaveTextContent("耗时 820 ms");
    expect(s.posts.at(-1)).toEqual({ path: "/v1/settings/model-endpoints/test", body: { endpoint: "generate", config: { url: "https://llm.example.com/v1", api_key: "***", model: "model-b", locality: "external", protocol: "chat-completions", max_material_tokens: 16000 } } });
    expect(s.puts).toHaveLength(0); // 获取与测试都不保存
  });

  it("拉不到模型列表时说明原因并改为手动填写；测试失败按机器码显示说明", async () => {
    const s = server();
    s.setModels({ ok: false, reason: "not_found", status: 404, elapsed_ms: 30 });
    s.setProbe({ subject: "generate_endpoint", status: "error", code: "unauthorized", why: "the endpoint returned HTTP status 401", next: "check URL, API key and model name", data: { elapsed_ms: 40, status: 401 } });
    mount(s.impl);
    const g = await card("写作模型");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    expect(await g.findByText(/这个端点不提供模型列表（HTTP 404）。可以手动填写模型名/)).toBeInTheDocument();
    await userEvent.clear(g.getByLabelText("模型"));
    await userEvent.type(g.getByLabelText("模型"), "my-model");
    await userEvent.click(g.getByRole("button", { name: "测试连接" }));
    const r = await g.findByRole("status", { name: "测试结果" });
    expect(r).toHaveTextContent("✗ 鉴权失败");
    expect(r).toHaveTextContent("HTTP 401");
    expect(r).not.toHaveTextContent("check URL"); // 英文说明只在「显示原始值」里
    expect(s.posts.at(-1)!.body.config.model).toBe("my-model");
  });

  it("地址换到另一个主机时不再沿用已存密钥，要求重新输入", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("写作模型");
    const url = g.getByLabelText("地址");
    await userEvent.clear(url);
    await userEvent.type(url, "https://other.example.com/v1");
    expect(g.getByText(/地址换了主机，请重新输入密钥/)).toBeInTheDocument();
    await userEvent.type(g.getByLabelText("API 密钥"), "sk-other");
    await userEvent.click(g.getByRole("button", { name: "获取模型" }));
    await g.findByText(/已从端点取到/);
    expect(s.posts.at(-1)!.body).toEqual({ endpoint: "generate", url: "https://other.example.com/v1", api_key: "sk-other", protocol: "chat-completions" });
  });

  it("地区不可用：测试连接直接提示改用网关地址或代理，不说协议不符", async () => {
    const s = server();
    s.setProbe({ subject: "judge_endpoint", status: "error", code: "region_unavailable", why: "the service is not available from this network location", next: "use a reachable compatible gateway URL or a proxy", data: { elapsed_ms: 200 } });
    mount(s.impl);
    const j = await card("打分模型");
    await userEvent.click(j.getByRole("button", { name: "测试连接" }));
    const r = await j.findByRole("status", { name: "测试结果" });
    expect(r).toHaveTextContent("✗ 服务在当前网络位置不可用，请改用可访问的网关地址或代理");
    expect(r).not.toHaveTextContent("响应不符合所选协议");
  });

  it("外部端点提示还没设置外部外发范围的来源数，并指到本页「模型端点能收哪些来源」", async () => {
    mount(server().impl);
    const g = await card("写作模型");
    expect(await g.findByText(/2 个来源没选外发范围/)).toBeInTheDocument();
    expect(g.getByRole("link", { name: "在下面选" })).toHaveAttribute("href", "#endpoint-sources");
  });

  it("模型端点能收哪些来源：每个来源一行、记聊天的项目一行，只在这里改（§14.14）", async () => {
    const s = server();
    mount(s.impl);
    const sec = await card("模型端点能收哪些来源");
    expect(await sec.findByRole("listitem", { name: "甲来源" })).toBeInTheDocument();
    expect(sec.getByRole("listitem", { name: "丙来源" })).toBeInTheDocument();
    expect(sec.queryByRole("listitem", { name: "不记聊天" })).not.toBeInTheDocument();
    const chat = within(sec.getByRole("listitem", { name: "聊天项目" }));
    // 不预选、标「还没选」，选之前不能保存；保存只发 value，带 CSRF
    const group = chat.getByRole("radiogroup", { name: "聊天内容发给外部模型时" });
    expect(within(group).getAllByRole("radio").every((r) => !(r as HTMLInputElement).checked)).toBe(true);
    expect(chat.getByText("还没选")).toBeInTheDocument();
    expect(chat.getByRole("button", { name: "保存" })).toBeDisabled();
    await userEvent.click(within(group).getByRole("radio", { name: "只有标题" }));
    await userEvent.click(chat.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(s.puts).toContainEqual({ capture: { value: "meta" }, csrf: "tok-1" }));
  });

  it("flow-endpoint-config: 两个都没配时有空状态；新配端点的位置没有预选，选之前不保存；取消配置发 null", async () => {
    const s = server({ judge: null, generate: null });
    mount(s.impl);
    expect(await screen.findByRole("region", { name: "还没有配置模型端点" })).toHaveTextContent("不接也能用");
    const j = await card("打分模型");
    expect(j.queryByLabelText(/地址/)).toBeNull();
    await userEvent.click(j.getByLabelText("配置打分模型"));
    expect(j.getByRole("radio", { name: /本机/ })).not.toBeChecked();
    expect(j.getByRole("radio", { name: /外部服务/ })).not.toBeChecked();
    expect(j.getByLabelText("超时（秒）")).toHaveValue(30);
    // 换成自建服务协议：仍是预填值的地址与模型清空，超时回到 10000。
    await userEvent.selectOptions(j.getByLabelText("协议"), "probability-judgment/1");
    expect(j.getByLabelText(/地址/)).toHaveValue("");
    expect(j.getByLabelText("超时（秒）")).toHaveValue(10);
    await userEvent.type(j.getByLabelText(/地址/), "http://cls.stub/judge");
    await save("打分模型");
    expect(await j.findByRole("alert")).toHaveTextContent("配置这个端点时必选");
    expect(s.puts).toHaveLength(0);
    await userEvent.click(j.getByRole("radio", { name: /本机/ }));
    await save("打分模型");
    await j.findByText(/已保存/);
    expect(s.puts.at(-1)).toEqual({ judge_endpoint: { url: "http://cls.stub/judge", api_key: "", model: "", locality: "local", protocol: "probability-judgment/1", timeout_ms: 10000, max_batch: 32, max_material_chars: 8000 } });

    await userEvent.click(j.getByLabelText("配置打分模型"));
    await save("打分模型");
    await waitFor(() => expect(s.puts.at(-1)).toEqual({ judge_endpoint: null }));
    expect(j.getByText(/已保存/)).toBeInTheDocument(); // 保存成功只说「已保存」，不带版本号（界面设计 §6.2）
  });

  it("flow-endpoint-config: 服务端拒绝时显示原因，草稿保留", async () => {
    const s = server();
    mount(s.impl);
    const g = await card("写作模型");
    const url = g.getByLabelText("地址");
    await userEvent.clear(url);
    await userEvent.type(url, "not a url");
    await userEvent.type(g.getByLabelText("API 密钥"), "k");
    await save("写作模型");
    expect(await g.findByRole("alert")).toHaveTextContent("bad endpoint url");
    expect(url).toHaveValue("not a url");

  });

  it("暂停开关：运行中 ↔ 已暂停只发 models_paused；暂停时两张卡片照常可编辑并标出已暂停；没有端点时不显示开关", async () => {
    const s = server();
    mount(s.impl);
    const sw = await screen.findByRole("switch", { name: "模型运行开关" });
    expect(sw).toBeChecked();
    expect(screen.getByRole("region", { name: "模型运行开关" })).toHaveTextContent("运行中");
    expect((await card("写作模型")).queryByText("已暂停")).toBeNull();

    await userEvent.click(sw);
    expect(await screen.findByRole("switch", { name: "模型运行开关" })).not.toBeChecked();
    expect(s.puts.at(-1)).toEqual({ models_paused: true });
    expect(screen.getByRole("region", { name: "模型运行开关" })).toHaveTextContent("已暂停");
    expect(await within(screen.getByRole("region", { name: "模型运行开关" })).findByText("暂停期间积下 14 条 · 恢复后自动判断（最近 7 天的）")).toBeInTheDocument();
    const g = await card("写作模型");
    expect(within(g.getByRole("heading")).getByText("已暂停")).toBeInTheDocument();
    expect(within((await card("打分模型")).getByRole("heading")).getByText("已暂停")).toBeInTheDocument();
    expect(g.getByLabelText("模型")).toBeEnabled();
    expect(g.getByRole("button", { name: "测试连接" })).toBeEnabled();

    await userEvent.click(screen.getByRole("switch", { name: "模型运行开关" }));
    expect(await screen.findByRole("switch", { name: "模型运行开关" })).toBeChecked();
    expect(s.puts.at(-1)).toEqual({ models_paused: false });
  });

  it("自动重判关闭（rejudge_days 为 0）时，暂停卡说恢复后在积压里放开", async () => {
    const s = server({ days: 0 });
    mount(s.impl);
    await userEvent.click(await screen.findByRole("switch", { name: "模型运行开关" }));
    expect(await within(screen.getByRole("region", { name: "模型运行开关" })).findByText("暂停期间积下 14 条 · 恢复后在积压里放开")).toBeInTheDocument();
  });

  it("没有任何端点时不显示暂停开关", async () => {
    mount(server({ judge: null, generate: null }).impl);
    await screen.findByRole("region", { name: "还没有配置模型端点" });
    expect(screen.queryByRole("switch")).toBeNull();
  });

  it("模型端点能收哪些来源：有内置 Inbox 时置顶带徽章，其他无仓库项目默认折叠", async () => {
    const base = server();
    const impl = (async (url: string, init?: RequestInit) => {
      if (url === "/v1/projects") {
        return json(200, {
          projects: [
            { id: "p_repo", name: "绑定仓库项目", capture: true, capture_outbound: null, repos: [{ remote: "git@h:a/b.git" }] },
            { id: "p_misc", name: "零散无仓项目", capture: true, capture_outbound: null, repos: [] },
            { id: "prj_inbox", name: "Inbox", system_role: "inbox", capture: true, capture_outbound: null, repos: [] },
          ],
        });
      }
      return base.impl(url, init);
    }) as unknown as typeof fetch;
    mount(impl);
    const sec = await card("模型端点能收哪些来源");
    const inboxRow = await sec.findByRole("listitem", { name: "Inbox" });
    expect(inboxRow).toHaveTextContent("内置");
    expect(sec.getByRole("listitem", { name: "绑定仓库项目" })).toBeInTheDocument();
    expect(sec.getByText("其他未绑定仓库的项目（1）")).toBeInTheDocument();
  });
});
