import { cleanup as cleanupAll, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { browserZone } from "./schedule";

// 测试边界替身：按 GET /v1/items?kind=brief、GET /v1/instances、GET /v1/packs 与 POST /v1/instances 契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const items = [
  { id: "b1", kind: "brief", title: "周一简报", created_at: "2030-01-01T00:00:00Z", origin: { source: "instance:ins_1" }, spec: { period: "2030-W01", sections: [{ project: "home", body: "家里进展" }, { project: "work", body: "工作进展" }] } },
  { id: "b2", kind: "brief", title: "周复盘", created_at: "2030-01-07T00:00:00Z", spec: { period: "2030-W02-review", sections: [{ project: "work", body: "复盘工作" }] } },
  { id: "b4", kind: "brief", title: "新简报", created_at: "2030-01-09T00:00:00Z", spec: { period: "2030-W03", sections: [{ title: "日程", body: "今天 10:00 站会" }, { project: "home", body: "家里新进展" }, { title: "知会", body: "无" }] } },
  { id: "b3", kind: "brief", title: "空简报", created_at: "2030-01-08T00:00:00Z", spec: {} },
];
const DEF = "每个项目一节，写进展。";
const packs = [{ name: "brief", instance_params: {}, default_instruction: DEF }, { name: "review", instance_params: {} }, { name: "follow-up", instance_params: {} }];
const routines = [{ id: "ins_r", pack: "routine", params: { schedule: "CRON_TZ=UTC 0 9 * * 1", mode: "create_item", title: "{date} 周报" }, enabled: true }];
const instances = [{ id: "ins_1", pack: "brief", params: { schedule: "30 8 * * 1-5" }, enabled: true }, { id: "ins_2", pack: "follow-up", params: {}, enabled: true }];

interface Calls { posts: { body: unknown; csrf: string | null }[]; urls: string[]; patches?: { url: string; body: unknown }[]; puts?: any[] }
function server(o: { delivery?: unknown; patchFail?: boolean; fail?: boolean; items?: unknown[]; reject?: boolean; routine?: boolean; host?: string; caps?: Record<string, string>; ready?: boolean; instances?: unknown[] } = {}, calls: Calls = { posts: [], urls: [] }) {
  const f = (async (url: string, init?: RequestInit) => {
    calls.urls.push(url);
    if (url === "/v1/projects") return json(200, { projects: [{ id: "home", name: "家务" }, { id: "work", name: "工作" }] });
    if (o.fail) return json(500, { code: "internal", why: "ledger unavailable", next: "retry" });
    if (url === "/v1/items?kind=brief") return json(200, { items: o.items ?? items });
    const one = url.match(/^\/v1\/items\/([^/?]+)$/);
    if (one) {
      const it = (o.items ?? items).find((x) => (x as { id: string }).id === decodeURIComponent(one[1]));
      return it ? json(200, { item: it, relations: [] }) : json(404, { code: "not_found", why: "no such item", next: "" });
    }
    if (url === "/v1/settings/policy" && o.delivery && init?.method === "PUT") {
      const body = JSON.parse(init.body as string);
      (calls.puts ??= []).push(body);
      return json(200, { version: 4, disturb: { ...body.disturb, delivery: body.disturb.delivery ?? o.delivery } });
    }
    if (url === "/v1/settings/policy" && o.delivery) return json(200, { version: 3, disturb: { daily_cap: 20, mutes: [], delivery: o.delivery } });
    if (url === "/v1/packs") return json(200, { packs: o.routine ? [...packs, { name: "routine", instance_params: {} }] : packs });
    if (url.startsWith("/v1/agents?")) return json(200, { agents: [], selected: { agent_name: o.host, permission_cap: o.caps?.[o.host ?? ""] ?? "read" }, reason: "" });
    const ip = url.match(/^\/v1\/instances\/([^/]+)$/);
    if (ip && init?.method === "PATCH") {
      const body = JSON.parse(init.body as string);
      if (o.patchFail) return json(400, { code: "invalid_request", why: "instruction is too long", next: "shorten it" });
      (calls.patches ??= []).push({ url, body });
      const cur = [...instances, ...routines].find((x) => x.id === decodeURIComponent(ip[1]))!;
      return json(200, { ...cur, ...body, params: body.params ?? cur.params, enabled: body.enabled ?? cur.enabled });
    }
    if (/^\/v1\/instances\/[^/]+\/run$/.test(url) && init?.method === "POST") {
      calls.posts.push({ body: url, csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] ?? null });
      return json(202, instances[0]);
    }
    if (url === "/v1/instances" && init?.method === "POST") {
      calls.posts.push({ body: JSON.parse(init.body as string), csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] ?? null });
      if (o.reject) return json(400, { code: "invalid_request", why: "schedule is not a valid cron expression", next: "fix the request and retry" });
      const b = JSON.parse(init.body as string);
      return json(201, { id: "ins_3", pack: b.pack, ...(b.project ? { project: b.project } : {}), params: b.params, enabled: true });
    }
    if (url === "/v1/instances") return json(200, { instances: o.instances ?? (o.routine ? [...instances, ...routines] : instances), agent_ready: !!o.ready });
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return f;
}
function mount(f: typeof fetch, path = "/briefs/schedule") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[path]}><App features={[feature]} fetchImpl={f} /></MemoryRouter>);
}

beforeEach(() => sessionStorage.clear());

const LONG = { id: "b9", kind: "brief", title: "晚报", created_at: "2030-02-01T12:00:00Z", origin: { source: "instance:ins_1" }, spec: { period: "2030-W05", sections: [
  { title: "要点", body: "# 要点\n## 小标题\n第一行\n第二行\n\n- 事项一\n  - 子项\n- 事项二\n\n1. 先做\n2. 再做\n\n| 名称 | 状态 |\n| --- | --- |\n| 报告 | **完成** |\n\n```\nnpm test\n```\n\n> 引用一句\n\n看 [文档](https://example.org/a) 与 https://example.org/b.\n\n- [x] 已完成项\n- [ ] 待办项\n\n1. 步骤一\n\n   步骤一的第二段\n\n       缩进代码行\n\n2. 步骤二\n\n**粗体里的 [嵌套链接](https://example.org/n) 与 *斜体***" },
  { title: "危险", body: "<script>alert(1)</script>\n\n[点我](javascript:alert(1)) [数据](data:text/html;base64,AAAA) [相对](/judgments/x)\n\n![x](https://example.org/p.png) ![y](javascript:alert(2))" },
] } };
const old = (n: number) => ({ id: `o${String(n).padStart(2, "0")}`, kind: "brief", title: `旧简报${n}`, created_at: `2029-12-${String(n).padStart(2, "0")}T08:00:00Z`, spec: { sections: [{ title: "回顾", body: `内容${n}` }] } });

describe("简报页", () => {
  it("正文按 Markdown 渲染：标题下移、段落换行、嵌套列表、表格、代码、引用、链接", async () => {
    mount(server({ items: [LONG, ...items] }), "/briefs");
    const a = within(await screen.findByLabelText("简报 晚报"));
    expect(a.getByRole("heading", { level: 3, name: "要点" })).toBeInTheDocument();
    expect(a.getAllByRole("heading", { name: "要点" })).toHaveLength(1); // 正文开头重复的同名标题不再画一遍
    expect(a.getByRole("heading", { level: 5, name: "小标题" })).toBeInTheDocument(); // 正文 ## 落在节标题之下
    expect(a.getByText(/第一行/).innerHTML).toContain("<br>"); // 单个换行保留
    expect(within(a.getAllByRole("list")[0]).getAllByRole("listitem")[0]).toHaveTextContent("事项一 子项");
    expect(a.getByText("子项").closest("ul")!.parentElement!.tagName).toBe("LI");
    expect(a.getAllByRole("list")[2].tagName).toBe("OL"); // [0] 无序列表 [1] 其子列表
    expect(a.getByRole("columnheader", { name: "状态" })).toBeInTheDocument();
    expect(a.getByRole("cell", { name: "完成" }).querySelector("strong")).not.toBeNull();
    expect(a.getByText("npm test").tagName).toBe("CODE");
    expect(a.getByText("引用一句").closest("blockquote")).not.toBeNull();
    expect(a.getByRole("link", { name: "文档" })).toHaveAttribute("href", "https://example.org/a");
    expect(a.getByRole("link", { name: "https://example.org/b" })).toHaveAttribute("href", "https://example.org/b"); // 句号不进链接
  });

  it("成熟解析库覆盖的写法：任务清单、列表项内多段、缩进代码、嵌套强调与链接", async () => {
    mount(server({ items: [LONG] }), "/briefs");
    const a = within(await screen.findByLabelText("简报 晚报"));
    const boxes = a.getAllByRole("checkbox");
    expect(boxes.map((b) => (b as HTMLInputElement).checked)).toEqual([true, false]);
    expect(boxes[0]).toBeDisabled();
    const step = a.getByText("步骤一").closest("li")!;
    expect(step.querySelectorAll("p")).toHaveLength(2); // 同一项里两段
    expect(within(step).getByText("步骤一的第二段")).toBeInTheDocument();
    expect(step.querySelector("pre code")).toHaveTextContent("缩进代码行");
    const strong = a.getByRole("link", { name: "嵌套链接" }).closest("strong")!;
    expect(strong.querySelector("em")).toHaveTextContent("斜体");
    expect(strong.querySelector("a")).toHaveAttribute("rel", expect.stringContaining("noopener"));
  });

  it("HTML 与脚本只显示为文本，危险链接不可点，外部图片点开前不请求", async () => {
    const { container } = mount(server({ items: [LONG] }), "/briefs");
    const a = within(await screen.findByLabelText("简报 晚报"));
    expect(a.getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument();
    expect(container.querySelector("script")).toBeNull();
    expect(a.queryByRole("link", { name: "点我" })).toBeNull();
    expect(a.getByText(/点我 数据 相对/)).toBeInTheDocument(); // 不放行的协议与相对路径只显示文字
    expect(a.queryByRole("link", { name: /数据|相对/ })).toBeNull();
    expect(container.querySelector("a[href^='javascript'], a[href^='data']")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(a.getByRole("button", { name: "加载图片：x" })).toBeInTheDocument();
    expect(a.queryByRole("button", { name: "加载图片：y" })).toBeNull(); // 非 http(s) 图片不给点
    await userEvent.click(a.getByRole("button", { name: "加载图片：x" }));
    expect(container.querySelector("img")).toHaveAttribute("src", "https://example.org/p.png");
  });

  it("最新一份整份展开，旧的按时间倒排列在历史里，不重复展开；带来源安排便于区分早晚", async () => {
    mount(server({ items: [LONG, ...items] }), "/briefs");
    const latest = within(await screen.findByRole("region", { name: "最新一份" }));
    expect(latest.getByLabelText("简报 晚报")).toBeInTheDocument();
    const hist = within(await screen.findByRole("list", { name: "历史简报列表" }));
    const names = hist.getAllByRole("link").filter((l) => !l.getAttribute("href")!.endsWith("/schedule")).map((l) => l.textContent);
    expect(names).toEqual(["新简报", "空简报", "周复盘", "周一简报"]); // 按 created_at 从新到旧，与返回顺序无关
    expect(hist.getByRole("link", { name: /简报 · 工作日 08:30/ })).toHaveAttribute("href", "/briefs/schedule"); // 周一简报来自 ins_1，来源可点进安排
    expect(screen.queryByText("家里进展")).not.toBeInTheDocument(); // 历史只列标题与时间
  });

  it("历史简报可打开完整内容，刷新（按 id 重读）后仍在；打不开的 id 显示原因", async () => {
    const { unmount } = mount(server({ items: [LONG, ...items] }), "/briefs/b1");
    const a = within(await screen.findByLabelText("简报 周一简报"));
    expect(await a.findByRole("heading", { name: "家务" })).toBeInTheDocument();
    expect(a.getByText("家里进展")).toBeInTheDocument();
    expect(a.getByText("工作进展")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← 返回简报" })).toHaveAttribute("href", "/briefs");
    unmount();
    mount(server({ items: [] }), "/briefs/nope");
    expect(await screen.findByRole("alert")).toHaveTextContent("no such item");
  });

  it("历史分页：每页 20 条，翻页与按页码进入都能找到旧简报", async () => {
    const many = Array.from({ length: 25 }, (_, n) => old(n + 1)); // 12-01…12-25，加一份最新的
    mount(server({ items: [LONG, ...many] }), "/briefs");
    const hist = within(await screen.findByRole("list", { name: "历史简报列表" }));
    expect(hist.getAllByRole("link")).toHaveLength(20);
    expect(hist.getAllByRole("link")[0]).toHaveTextContent("旧简报25");
    expect(screen.getByText("第 1 / 2 页")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "下一页" }));
    expect(within(await screen.findByRole("list", { name: "历史简报列表" })).getAllByRole("link").map((l) => l.textContent)).toEqual(["旧简报5", "旧简报4", "旧简报3", "旧简报2", "旧简报1"]);
    expect(screen.getByRole("button", { name: "下一页" })).toBeDisabled();
  });

  it("旧简报缺少 created_at 与来源时仍可读、可打开", async () => {
    mount(server({ items: [{ id: "x1", kind: "brief", title: "很早的简报", spec: { sections: [{ project: "home", body: "旧写法" }] } }, items[0]] }), "/briefs");
    expect(await screen.findByLabelText("简报 周一简报")).toBeInTheDocument();
    const hist = within(await screen.findByRole("list", { name: "历史简报列表" }));
    expect(hist.getByText("生成时间未记录")).toBeInTheDocument();
    await userEvent.click(hist.getByRole("link", { name: "很早的简报" }));
    expect(await screen.findByText("旧写法")).toBeInTheDocument();
  });

  it("没有分节内容的简报显示空状态", async () => {
    mount(server({ items: [items[3]] }), "/briefs");
    expect(within(await screen.findByLabelText("简报 空简报")).getByText("暂无分节内容")).toBeInTheDocument();
  });

  it("没有任何简报时显示空状态", async () => {
    mount(server({ items: [] }), "/briefs");
    expect(await screen.findByText("还没有简报")).toBeInTheDocument();
  });

  it("简报页把「已生成的」与「定时安排」分开：安排页不混着简报正文，简报页只给一个去改安排的链接（界面设计 §3.1）", async () => {
    const v = mount(server());
    expect(await screen.findByRole("group", { name: "简报" })).toBeInTheDocument();
    expect(screen.queryByLabelText("简报 周一简报")).toBeNull();
    v.unmount();
    mount(server(), "/briefs");
    expect(await screen.findByRole("link", { name: "改定时安排 ›" })).toHaveAttribute("href", "/briefs/schedule");
  });

  it("flow-brief: 只列出简报类能力包的实例；每个一张卡，收起时一行摘要，不露 cron 串", async () => {
    mount(server());
    const l = within(await screen.findByLabelText("已安排的"));
    const card = await l.findByRole("group", { name: "简报" });
    expect(l.queryByRole("group", { name: "跟进提醒" })).toBeNull();
    expect(card).not.toHaveAttribute("open");
    expect(within(card).getByText(/工作日 08:30 · 开/)).toBeInTheDocument();
    expect(screen.queryByText("30 8 * * 1-5")).not.toBeInTheDocument();
  });

  it("每张卡上改这份结果怎么送、没出来时（唯一可编辑处）：没存实例行时按类型行显示生效值；改了只写这个实例的行，其余原样带回；取不到矩阵时不显示", async () => {
    const delivery = { default_channel: "channel:ch-1", rows: { "task_result:brief": { agent: "main", user: "off", else: "next_chat" }, "task_result:review": { agent: "off", user: "default" }, "task_failed:brief": { agent: "off", user: "default" } } };
    const calls: Calls = { posts: [], urls: [] };
    mount(server({ delivery, instances: [...instances, { id: "ins_v", pack: "review", params: { schedule: "30 17 * * 5" }, enabled: true }] }, calls));
    const brief = within(await screen.findByRole("group", { name: "简报" }));
    const result = within(await brief.findByRole("radiogroup", { name: "结果怎么送" }));
    expect(result.getByRole("radio", { name: "在对话里马上告诉我" })).toBeChecked();
    expect(within(brief.getByRole("radiogroup", { name: "没出来时" })).getByRole("radio", { name: /^推到手机/ })).toBeChecked();
    expect(within(screen.getByRole("group", { name: "复盘" }).querySelector('[data-reminder="instance:ins_v"]') as HTMLElement).getByRole("radio", { name: /^推到手机/ })).toBeChecked();
    expect(within(brief.getByRole("radiogroup", { name: "没出来时" })).queryByRole("radio", { name: "下次聊天时告诉我" })).toBeNull(); // 告警类不给"下次聊天时"
    await userEvent.click(result.getByRole("radio", { name: "下次聊天时告诉我" }));
    await waitFor(() => expect(calls.puts).toHaveLength(1));
    const rows = calls.puts![0].disturb.delivery.rows;
    expect(rows["instance:ins_1"]).toEqual({ agent: "next_chat", user: "off" });
    expect(rows["task_result:brief"]).toEqual(delivery.rows["task_result:brief"]);
    expect(Object.keys(rows).sort()).toEqual(["instance:ins_1", "task_failed:brief", "task_result:brief", "task_result:review"]);
    expect(calls.puts![0].disturb).not.toHaveProperty("delivery_saved");
    cleanupAll();
    mount(server());
    await screen.findByText(/工作日 08:30/);
    expect(screen.queryByRole("radiogroup", { name: "结果怎么送" })).toBeNull();
  });

  it("Agent 还不能后台运行时卡片写明等它接入并链到设置；「立即跑一次」不可点", async () => {
    mount(server());
    const card = within(await screen.findByRole("group", { name: "简报" }));
    expect(card.getByText("等 Agent 接入后开始")).toBeInTheDocument();
    expect(card.getByRole("link", { name: "去设置" })).toHaveAttribute("href", "/settings/agents");
    expect(card.getByRole("button", { name: "立即跑一次" })).toBeDisabled();
  });

  it("展开改「让 Agent 写什么」：没改时显示默认并标「默认」；改了标「已改」并带 instruction，恢复默认后不带；定时没动原样带回", async () => {
    const calls: Calls = { posts: [], urls: [], patches: [] };
    mount(server({ ready: true }, calls));
    const card = within(await screen.findByRole("group", { name: "简报" }));
    await userEvent.click(card.getByText("简报"));
    const box = card.getByRole("textbox", { name: /让 Agent 写什么/ });
    expect(box).toHaveValue(DEF);
    expect(card.getByText("默认")).toBeInTheDocument();
    expect(card.queryByRole("button", { name: "恢复默认" })).toBeNull();
    await userEvent.clear(box);
    await userEvent.type(box, "只写风险");
    expect(card.queryByText("默认")).toBeNull();
    expect(card.getByText("已改")).toBeInTheDocument();
    await userEvent.click(card.getByRole("button", { name: "保存" }));
    expect(calls.patches!.at(-1)).toEqual({ url: "/v1/instances/ins_1", body: { params: { schedule: "30 8 * * 1-5", instruction: "只写风险" } } });
    expect(await card.findByText("已保存，之后的运行都按这份")).toBeInTheDocument();
    await userEvent.click(card.getByRole("button", { name: "恢复默认" }));
    expect(box).toHaveValue(DEF);
    await userEvent.selectOptions(card.getByLabelText("哪几天"), "每周五");
    await userEvent.click(card.getByRole("button", { name: "保存" }));
    expect(calls.patches!.at(-1)).toEqual({ url: "/v1/instances/ins_1", body: { params: { schedule: "30 8 * * 5" } } });
  });

  it("有没保存的改动时不能立即跑一次（免得跑旧配置）；保存成功后改动标记清掉、可以跑；保存失败不显示已保存", async () => {
    const calls: Calls = { posts: [], urls: [], patches: [] };
    mount(server({ ready: true, patchFail: true }, calls));
    const card = within(await screen.findByRole("group", { name: "简报" }));
    await userEvent.click(card.getByText("简报"));
    const run = card.getByRole("button", { name: "立即跑一次" });
    expect(run).toBeEnabled();
    await userEvent.type(card.getByRole("textbox", { name: /让 Agent 写什么/ }), "再写风险");
    expect(run).toBeDisabled();
    expect(card.getByText("先保存")).toBeInTheDocument();
    await userEvent.click(card.getByRole("button", { name: "保存" }));
    expect((await card.findAllByText(/instruction is too long/)).length).toBeGreaterThan(0);
    expect(card.queryByText("已保存，之后的运行都按这份")).toBeNull();
    expect(run).toBeDisabled();
    expect(calls.posts).toEqual([]);
  });

  it("保存成功后以服务端返回的实例为准：改动标记清掉，立即跑一次用的就是刚存的配置", async () => {
    const calls: Calls = { posts: [], urls: [], patches: [] };
    mount(server({ ready: true }, calls));
    const card = within(await screen.findByRole("group", { name: "简报" }));
    await userEvent.click(card.getByText("简报"));
    await userEvent.selectOptions(card.getByLabelText("哪几天"), "每周五");
    expect(card.getByRole("button", { name: "立即跑一次" })).toBeDisabled();
    await userEvent.click(card.getByRole("button", { name: "保存" }));
    expect(await card.findByText("已保存，之后的运行都按这份")).toBeInTheDocument();
    expect(card.queryByText("先保存")).toBeNull();
    await userEvent.click(card.getByRole("button", { name: "立即跑一次" }));
    expect(calls.patches!.at(-1)!.body).toEqual({ params: { schedule: "30 8 * * 5" } });
    expect(calls.posts).toEqual([{ body: "/v1/instances/ins_1/run", csrf: "tok-1" }]);
  });

  it("读简报时点来源进入对应安排，卡片已展开，能直接改内容要求", async () => {
    mount(server({ items: [LONG, ...items] }), "/briefs");
    const latest = within(await screen.findByRole("region", { name: "最新一份" }));
    await userEvent.click(latest.getByRole("link", { name: /简报 · 工作日 08:30/ }));
    const card = await screen.findByRole("group", { name: "简报" });
    expect(card).toHaveAttribute("open");
    expect(within(card).getByRole("textbox", { name: /让 Agent 写什么/ })).toBeVisible();
  });

  it("Agent 可用时可以立即跑一次；卡片链到这个安排最近一次的简报", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({ ready: true }, calls));
    const card = within(await screen.findByRole("group", { name: "简报" }));
    expect(card.queryByText("等 Agent 接入后开始")).toBeNull();
    expect(card.getByRole("link", { name: /最近一次/ })).toHaveAttribute("href", "/briefs/b1");
    await userEvent.click(card.getByRole("button", { name: "立即跑一次" }));
    expect(calls.posts).toEqual([{ body: "/v1/instances/ins_1/run", csrf: "tok-1" }]);
    expect(await card.findByText("已开始")).toBeInTheDocument();
  });

  it("读不回的定时只显示「自定义时间」", async () => {
    mount(server({ instances: [{ id: "ins_9", pack: "review", params: { schedule: "*/5 * * * *" }, enabled: false }] }));
    const card = within(await screen.findByRole("group", { name: "复盘" }));
    expect(card.getByText(/自定义时间 · 关/)).toBeInTheDocument();
    expect(screen.queryByText("*/5 * * * *")).not.toBeInTheDocument();
  });

  it("flow-brief: 再安排一个：POST /v1/instances 带包、定时与项目，成功后列出新实例", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({}, calls));
    const form = within(await screen.findByRole("form", { name: "新的定时简报" }));
    await userEvent.selectOptions(form.getByLabelText("做什么"), "review");
    await userEvent.selectOptions(form.getByLabelText("哪几天"), "每周五");
    const at = form.getByLabelText("几点");
    await userEvent.clear(at);
    await userEvent.type(at, "09:00");
    await userEvent.click(await form.findByLabelText("家务"));
    await userEvent.click(form.getByLabelText("工作"));
    await userEvent.click(form.getByRole("button", { name: "安排" }));
    // 定时带上这台设备的时区，避免 APA 主机时区不同时错开（§6.8）
    const zone = browserZone();
    expect(calls.posts).toEqual([{ body: { pack: "review", params: { schedule: `CRON_TZ=${zone} 0 9 * * 5`, projects: ["home", "work"] } }, csrf: "tok-1" }]);
    expect(await screen.findByRole("group", { name: "复盘" })).toBeInTheDocument();
  });

  it("建实例被服务端拒绝时显示原因，不伪造新实例", async () => {
    mount(server({ reject: true }));
    const form = within(await screen.findByRole("form", { name: "新的定时简报" }));
    await userEvent.type(form.getByLabelText("cron 或时长"), "bad"); // 高级里直接写的定时原样提交
    await userEvent.click(form.getByRole("button", { name: "安排" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("schedule is not a valid cron expression");
    expect(screen.queryByText(/ins_3/)).not.toBeInTheDocument();
  });

  it("还没有简报也没安排时，给出「每天早上给我一份」：只预填，本人再点「安排」才创建", async () => {
    const calls: Calls = { posts: [], urls: [] };
    const f = (async (url: string, init?: RequestInit) =>
      url === "/v1/items?kind=brief" ? json(200, { items: [] }) : url === "/v1/instances" && init?.method !== "POST" ? json(200, { instances: [] }) : server({}, calls)(url, init)) as unknown as typeof fetch;
    mount(f, "/briefs");
    await userEvent.click(await screen.findByRole("link", { name: "每天早上给我一份" }));
    expect(calls.posts).toEqual([]);
    expect(screen.getByLabelText("哪几天")).toHaveValue("*");
    expect(screen.getByLabelText("几点")).toHaveValue("08:00");
    await userEvent.click(screen.getByRole("button", { name: "安排" }));
    expect(calls.posts).toEqual([{ body: { pack: "brief", params: { schedule: `CRON_TZ=${browserZone()} 0 8 * * *` } }, csrf: "tok-1" }]);
  });

  it("没选时间时不发请求", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({}, calls));
    const form = within(await screen.findByRole("form", { name: "新的定时简报" }));
    await userEvent.clear(form.getByLabelText("几点"));
    await userEvent.click(form.getByRole("button", { name: "安排" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("请选好时间");
    expect(calls.posts).toEqual([]);
  });

  it("启停：每个实例一个开关，只发 enabled，界面按服务端返回更新", async () => {
    const calls: Calls = { posts: [], urls: [], patches: [] };
    mount(server({ routine: true }, calls));
    const card = within(await screen.findByRole("group", { name: "简报" }));
    await userEvent.click(card.getByRole("button", { name: "停用" }));
    expect(calls.patches).toEqual([{ url: "/v1/instances/ins_1", body: { enabled: false } }]);
    expect(await card.findByText(/工作日 08:30 · 关/)).toBeInTheDocument();
    expect(card.getByRole("button", { name: "启用" })).toBeInTheDocument();
    const r = within(screen.getByRole("list", { name: "已安排的例行任务" }));
    await userEvent.click(r.getByRole("button", { name: "启停 〈日期〉 周报" }));
    expect(calls.patches!.at(-1)).toEqual({ url: "/v1/instances/ins_r", body: { enabled: false } });
  });

  it("例行任务：默认只记一条事项，POST 只带 schedule、mode 与填了的标题、项目", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({ routine: true }, calls));
    const form = within(await screen.findByRole("form", { name: "新的例行任务" }));
    expect(form.queryByLabelText("要它做的事")).not.toBeInTheDocument();
    await userEvent.type(form.getByLabelText("标题（{date} 会换成当天日期）"), "交水电费");
    await userEvent.selectOptions(form.getByLabelText("哪几天做"), "1");
    await userEvent.selectOptions(await form.findByRole("combobox", { name: "归到哪个项目" }), "home");
    await userEvent.click(form.getByRole("button", { name: "安排例行任务" }));
    expect(calls.posts).toEqual([{ body: { pack: "routine", project: "home", params: { schedule: `CRON_TZ=${browserZone()} 0 9 * * 1`, mode: "create_item", title: "交水电费" } }, csrf: "tok-1" }]);
    expect(await within(screen.getByRole("list", { name: "已安排的例行任务" })).findByText(/交水电费 · 每周一 09:00 · 家务/)).toBeInTheDocument();
  });

  it("例行任务让 Agent 去做：任务说明必填；带上权限档与结果去处；超出默认宿主上限时提示后果", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({ routine: true, host: "hermes", caps: { hermes: "edit" } }, calls));
    const form = within(await screen.findByRole("form", { name: "新的例行任务" }));
    await userEvent.click(form.getByRole("radio", { name: "让 Agent 去做" }));
    await userEvent.click(form.getByRole("button", { name: "安排例行任务" }));
    expect(await form.findByRole("alert")).toHaveTextContent("请写清要它做的事");
    expect(calls.posts).toEqual([]);
    await userEvent.type(form.getByLabelText("要它做的事"), "按本周完成的事写周报草稿");
    await userEvent.selectOptions(form.getByLabelText("Agent 能用什么"), "full");
    // 上限只读、写明是谁的上限；超出时说后果并给「去改上限」，落到该 Agent 的页面（界面设计 §4）
    expect(form.getByText("hermes 的上限：可改文件")).toBeInTheDocument();
    expect(await form.findByText(/超出 hermes 的上限（可改文件），到点会停下来问你/)).toBeInTheDocument();
    expect(form.getByRole("link", { name: "去改上限" })).toHaveAttribute("href", expect.stringMatching(/^\/settings\/agents\//));
    await userEvent.selectOptions(form.getByLabelText("Agent 能用什么"), "edit");
    expect(form.queryByText(/超出 hermes 的上限/)).not.toBeInTheDocument();
    await userEvent.selectOptions(form.getByLabelText("结果写到哪"), "item_and_material");
    await userEvent.click(form.getByRole("button", { name: "安排例行任务" }));
    expect(calls.posts).toEqual([{ body: { pack: "routine", params: { schedule: `CRON_TZ=${browserZone()} 0 9 * * 1`, mode: "run", instruction: "按本周完成的事写周报草稿", access: "edit", output: "item_and_material" } }, csrf: "tok-1" }]);
  });

  it("没设默认宿主时，选「让 Agent 去做」提示到点会停下来问你，并给去设置的入口", async () => {
    mount(server({ routine: true }));
    const form = within(await screen.findByRole("form", { name: "新的例行任务" }));
    await userEvent.click(form.getByRole("radio", { name: "让 Agent 去做" }));
    expect(await form.findByText(/还没设默认 Agent/)).toBeInTheDocument();
    expect(form.getAllByRole("link", { name: "去设置" })[0]).toHaveAttribute("href", "/settings/agents#roles");
  });

  it("修改例行任务：按实例预填；定时没动就原样带回，改了几点仍用原来的时区；PATCH 带 project 与整份 params", async () => {
    const calls: Calls = { posts: [], urls: [], patches: [] };
    mount(server({ routine: true }, calls));
    const r = within(await screen.findByRole("list", { name: "已安排的例行任务" }));
    await userEvent.click(r.getByRole("button", { name: "修改 〈日期〉 周报" }));
    const form = within(await screen.findByRole("form", { name: "修改例行任务" }));
    expect(form.getByLabelText("标题（{date} 会换成当天日期）")).toHaveValue("{date} 周报");
    expect(form.getByLabelText("哪几天做")).toHaveValue("1");
    expect(form.getByLabelText("几点做")).toHaveValue("09:00");
    await userEvent.clear(form.getByLabelText("标题（{date} 会换成当天日期）"));
    await userEvent.type(form.getByLabelText("标题（{date} 会换成当天日期）"), "月报");
    await userEvent.click(form.getByRole("button", { name: "保存" }));
    expect(calls.patches!.at(-1)).toEqual({ url: "/v1/instances/ins_r", body: { project: "", params: { schedule: "CRON_TZ=UTC 0 9 * * 1", mode: "create_item", title: "月报" } } });
    expect(await r.findByText(/月报 · 每周一 09:00/)).toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "修改例行任务" })).toBeNull();

    await userEvent.click(r.getByRole("button", { name: "修改 月报" }));
    const again = within(await screen.findByRole("form", { name: "修改例行任务" }));
    await userEvent.selectOptions(again.getByLabelText("哪几天做"), "1-5");
    await userEvent.click(again.getByRole("button", { name: "保存" }));
    expect(calls.patches!.at(-1)!.body).toMatchObject({ params: { schedule: "CRON_TZ=UTC 0 9 * * 1-5" } });
  });

  it("服务端失败时显示真实原因", async () => {
    mount(server({ fail: true }));
    expect(await screen.findByRole("alert")).toHaveTextContent("ledger unavailable");
  });
});
