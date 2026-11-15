import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 「交给 Agent」：本质是给任务事项加/去掉 agent-ok 标签（PATCH tags，带 If-Match），界面不露出标签名。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const RULE = "background-run/event_rules/handle-labelled";

function server(o: { mode?: string; declared?: boolean; items?: any[] } = {}) {
  const items = o.items ?? [{ id: "i1", version: 1, kind: "task", phase: "proposed", title: "修复登录", tags: ["mine"] }];
  const patches: { body: any; ifMatch: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const h = (init.headers ?? {}) as Record<string, string>;
    if (url === "/v1/rules") return json(200, { rules: o.mode ? [{ key: RULE, pack: "background-run", group: "event_rules", id: "handle-labelled", mode: o.mode, hash: "h" }] : [] });
    if (url === "/v1/settings/commands") return json(200, { declarations: o.declared ? [{ harness: "hx", declaration: { headless: { access: {} } } }] : [] });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items" || url.startsWith("/v1/items?")) return json(200, { items });
    if (/\/messages$/.test(url)) return json(200, { messages: [] });
    if (url.startsWith("/v1/why/")) return json(200, { steps: [] });
    const it = items.find((i) => url === "/v1/items/" + i.id);
    if (!it) return json(404, { code: "not_found", why: "no item", next: "" });
    if (method === "PATCH") {
      const body = JSON.parse(init.body as string);
      patches.push({ body, ifMatch: h["If-Match"] ?? null });
      Object.assign(it, { tags: body.tags, version: it.version + 1 });
      return json(200, { id: it.id, version: it.version });
    }
    return json(200, { item: it, relations: [] });
  }) as unknown as typeof fetch;
  return { impl, patches, items };
}

function mount(s: ReturnType<typeof server>, path = "/items/i1") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[path]}><App features={[feature]} fetchImpl={s.impl} /></MemoryRouter>);
}
const box = () => screen.findByRole("checkbox", { name: "交给 Agent 处理" });

describe("事项 › 交给 Agent", () => {
  beforeEach(() => sessionStorage.clear());

  it("开：只加 agent-ok、保留别的标签，带 If-Match 与一句原因；详情不露出标签名；关：只去掉它", async () => {
    const s = server({ mode: "auto", declared: true });
    mount(s);
    await userEvent.click(await box());
    await waitFor(() => expect(s.patches).toHaveLength(1));
    expect(s.patches[0]).toEqual({ body: { tags: ["mine", "agent-ok"], reason: "交给 Agent 处理" }, ifMatch: "1" });
    expect(await screen.findByRole("checkbox", { name: "交给 Agent 处理", checked: true })).toBeInTheDocument();
    expect(screen.getByText("标签：mine")).toBeInTheDocument();
    expect(screen.queryByText(/agent-ok/)).not.toBeInTheDocument();
    expect(screen.queryByText(/试运行|开工/)).not.toBeInTheDocument(); // 前提都满足：不多说

    await userEvent.click(screen.getByRole("checkbox", { name: "交给 Agent 处理" }));
    await waitFor(() => expect(s.patches).toHaveLength(2));
    expect(s.patches[1]).toEqual({ body: { tags: ["mine"], reason: "不再交给 Agent" }, ifMatch: "2" });
  });

  it("已结束的事项没有这个控件", async () => {
    const s = server({ items: [{ id: "i1", version: 1, kind: "task", phase: "done", title: "完成了" }] });
    mount(s);
    await screen.findByText("完成了");
    expect(screen.queryByRole("checkbox", { name: "交给 Agent 处理" })).not.toBeInTheDocument();
  });

  it("不是任务的事项没有这个控件", async () => {
    const s = server({ items: [{ id: "i2", version: 1, kind: "note", phase: "open", title: "笔记" }] });
    mount(s, "/items/i2");
    await screen.findByText("笔记");
    expect(screen.queryByRole("checkbox", { name: "交给 Agent 处理" })).not.toBeInTheDocument();
  });

  it.each([
    ["规则关着", { mode: "off", declared: true }, "自动开工没开", "/rules"],
    ["试运行", { mode: "observe", declared: true }, "试运行中，只记录", "/rules"],
    ["Agent 还不能在后台运行", { mode: "auto", declared: false }, "会在允许 Agent 后台运行后开工", "/settings/agents"],
  ])("开着但前提没满足（%s）：旁边一句状态，链到对应设置", async (_n, o, text, href) => {
    const s = server({ ...o, items: [{ id: "i1", version: 1, kind: "task", phase: "proposed", title: "修复登录", tags: ["agent-ok"] }] });
    mount(s);
    const link = await screen.findByRole("link", { name: text + " ›" });
    expect(link).toHaveAttribute("href", href);
  });

  it("已经在进行中的事项：说明不会补开工，没有链接", async () => {
    const s = server({ mode: "auto", declared: true, items: [{ id: "i1", version: 1, kind: "task", phase: "open", title: "修复登录", tags: ["agent-ok"] }] });
    mount(s);
    expect(await screen.findByText("已在进行中，不会自动开工")).toBeInTheDocument();
  });

  it("没交出去时不显示状态；列表和标签筛选里 agent-ok 显示为「交给 Agent」", async () => {
    const s = server({ mode: "off", items: [
      { id: "i1", version: 1, kind: "task", phase: "proposed", title: "修复登录", tags: ["mine"] },
      { id: "i2", version: 1, kind: "task", phase: "open", title: "改文档", tags: ["agent-ok", "docs"] },
    ] });
    mount(s, "/items");
    await screen.findByText("改文档");
    expect(screen.getByText("交给 Agent #docs")).toBeInTheDocument();
    expect(screen.queryByText(/agent-ok/)).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "交给 Agent" })).toHaveValue("agent-ok");
    await userEvent.selectOptions(screen.getByLabelText("标签筛选"), "agent-ok");
    expect(screen.queryByText("修复登录")).not.toBeInTheDocument();
    expect(screen.getByText("改文档")).toBeInTheDocument();
  });
});
