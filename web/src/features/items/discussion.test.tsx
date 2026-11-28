import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { withSession } from "../../app/test-session";
import { feature } from "./index";

// 事项的讨论：发言按时间成流，回复挂在被回复的那条下；作者写「你」或 Agent 的名字（不露凭据 id）；
// 派出去的子事项在流里写它现在的阶段；回复写入带 reply_to。测试边界替身按 api/items、api/why 的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const at = (m: number) => new Date(Date.UTC(2030, 0, 2, 1, m)).toISOString();

function server() {
  const progress = [
    { id: "p1", text: "先查一下日志", actor: "owner", at: at(0) },
    { id: "p2", text: "日志里是缓存过期", actor: "cred_hermes", at: at(5), reply_to: "p1" },
    { id: "p3", text: "好，那就改配置", actor: "owner", at: at(9), reply_to: "p2" },
    { id: "p4", text: "另起一条：周五前上线", actor: "owner", at: at(12) },
  ];
  const posts: unknown[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const m = init.method ?? "GET";
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items") return json(200, { items: [] });
    if (url === "/v1/settings/tokens") return json(200, { credentials: [{ id: "cred_hermes", agent_id: "agt_1" }] });
    if (url === "/v1/agents") return json(200, { agents: [{ id: "agt_1", name: "小黑" }] });
    if (url === "/v1/items/itm_a" && m === "GET") return json(200, { item: { id: "itm_a", phase: "open", version: 3, kind: "task", title: "修登录超时" }, relations: [], progress, correlation_keys: [] });
    if (url === "/v1/items/itm_c") return json(200, { item: { id: "itm_c", phase: "waiting", version: 1, kind: "delegation", title: "子任务：改配置" }, relations: [], progress: [], correlation_keys: [] });
    if (url === "/v1/items/itm_a/trace") return json(200, { entries: [
      { at: at(10), who: { kind: "agent", name: "cred_hermes" }, kind: "delegated", args: { assignee: "exe_1" }, item: { id: "itm_c", title: "子任务：改配置" }, steps: [] },
      { at: at(12), who: { kind: "owner" }, kind: "progress", args: { id: "p4", text: "另起一条：周五前上线", mentions: [{ target: "exe_1", kind: "executor", state: "running", item: "itm_c" }, { target: "session:main", kind: "main", state: "delivered" }] }, steps: [] },
    ] });
    if (url === "/v1/executors") return json(200, { executors: [{ id: "exe_1", kind: "external-cli", label: "构建机 · Codex" }] });
    if (url === "/v1/items/itm_a/mentionable") return json(200, { candidates: [
      { id: "session:main", kind: "main", ok: true },
      { id: "exe_1", kind: "executor", label: "构建机 · Codex", host: "build-1", ok: false, reason: "busy" },
      { id: "exe_2", kind: "executor", label: "笔记本 · Claude", host: "laptop", remote: true, ok: true },
      { id: "exe_3", kind: "executor", host: "nas", ok: true },
    ] });
    if (url === "/v1/items/itm_a/comments" && m === "POST") {
      const b = JSON.parse(String(init.body));
      posts.push(b);
      if (b.mentions?.includes("exe_3")) return json(409, { code: "conflict", why: "exe_3 busy", next: "wait for it" });
      progress.push({ id: "p" + (progress.length + 1), text: b.text, actor: "owner", at: at(20), ...(b.reply_to && { reply_to: b.reply_to }) });
      return json(201, { comment: progress[progress.length - 1] });
    }
    return json(404, { code: "not_found", why: "no such route in this fake", next: "-" });
  }) as unknown as typeof fetch;
  return { impl, posts };
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={["/items/itm_a"]}><App features={[feature]} fetchImpl={withSession(f)} /></MemoryRouter>);
}

describe("事项的讨论", () => {
  it("回复挂在被回复的那条下（楼中楼），作者写你或 Agent 的名字，子事项旁写它现在的阶段", async () => {
    mount(server().impl);
    const stream = await screen.findByRole("list", { name: "讨论" });
    const top = [...stream.children] as HTMLElement[];
    // 顶层：第一条发言、派活那行、另起的一条；回复不在顶层。
    expect(top.map((li) => li.className)).toEqual(["comment", "stream-event", "comment"]);
    const first = top[0];
    expect(within(first).getByRole("list", { name: "对 你 的回复" })).toHaveTextContent("日志里是缓存过期");
    const agentReply = first.querySelector('[data-comment="p2"]') as HTMLElement;
    await waitFor(() => expect(within(agentReply).getByText("小黑")).toBeInTheDocument());
    expect(agentReply).not.toHaveTextContent("cred_hermes");
    expect(within(agentReply).getByRole("list", { name: "对 小黑 的回复" })).toHaveTextContent("好，那就改配置");
    expect(await within(top[1]).findByText("等待中")).toBeInTheDocument();
  });

  it("@：输入 @ 弹出主会话与执行者（含别的机器上的），在跑的置灰写原因；键盘选中成小标签，发送带 mentions；发言下写 @ 了谁、各自怎样", async () => {
    const s = server();
    mount(s.impl);
    const user = userEvent.setup();
    const stream = await screen.findByRole("list", { name: "讨论" });
    // 已有的 @ 状态：执行者在跑（链到它的子事项）、主会话已送达；不露 id
    const states = within(stream.querySelector('[data-comment="p4"]') as HTMLElement).getByRole("list", { name: "@ 了谁" });
    await waitFor(() => expect(states).toHaveTextContent("@构建机 · Codex · 在跑"));
    expect(states).toHaveTextContent("@主 Agent · 已送达");
    expect(within(states).getByRole("link")).toHaveAttribute("href", "/items/itm_c");
    expect(states).not.toHaveTextContent("exe_1");

    const box = screen.getAllByRole("combobox", { name: "发言" }).at(-1)!;
    await user.type(box, "请看一下 @");
    const pop = await screen.findByRole("listbox", { name: "@ 谁" });
    await waitFor(() => expect(within(pop).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "主主 Agent", "构构建机 · Codex这件事上还在跑", "笔笔记本 · Claude在 laptop", "未未命名执行者nas",
    ]));
    expect(within(pop).getAllByRole("option")[1]).toHaveAttribute("aria-disabled", "true");
    await user.keyboard("{ArrowDown}{ArrowDown}{Enter}"); // 第 2 项置灰、选不上；落到第 3 项
    expect(screen.queryByRole("listbox", { name: "@ 谁" })).toBeNull();
    expect(screen.getByRole("list", { name: "这条要 @ 的" })).toHaveTextContent("@笔记本 · Claude");
    expect(box).toHaveValue("请看一下 ");
    await user.type(box, "@笔");
    expect(within(await screen.findByRole("listbox", { name: "@ 谁" })).queryAllByRole("option")).toHaveLength(0); // 已选的不再列
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "发送并叫 1 个来做" }));
    await waitFor(() => expect(s.posts.at(-1)).toEqual({ text: "请看一下 @笔", mentions: ["exe_2"] }));
  });

  it("@ 的对象叫不动（服务端 409）：整条不生效，原因写在输入框下，文字与小标签都留着", async () => {
    const s = server();
    mount(s.impl);
    const user = userEvent.setup();
    await screen.findByRole("list", { name: "讨论" });
    const box = screen.getAllByRole("combobox", { name: "发言" }).at(-1)!;
    await user.type(box, "@nas");
    await user.keyboard("{Enter}");
    await user.type(box, "跑一下");
    await user.click(screen.getByRole("button", { name: "发送并叫 1 个来做" }));
    expect((await screen.findAllByText(/exe_3 busy/)).length).toBeGreaterThan(0);
    expect(box).toHaveValue("跑一下");
    expect(screen.getByRole("list", { name: "这条要 @ 的" })).toHaveTextContent("@未命名执行者");
  });

  it("点回复写一条带 reply_to，写完出现在被回复的那条下面；顶部输入框写的不带", async () => {
    const s = server();
    mount(s.impl);
    const user = userEvent.setup();
    const stream = await screen.findByRole("list", { name: "讨论" });
    const last = stream.querySelector('[data-comment="p4"]') as HTMLElement;
    await user.click(within(last).getByRole("button", { name: "回复" }));
    await user.type(within(last).getByRole("combobox", { name: "回复 你" }), "收到");
    await user.click(within(last).getByRole("button", { name: "发送" }));
    await waitFor(() => expect(s.posts).toEqual([{ text: "收到", reply_to: "p4" }]));
    await waitFor(() => expect(within(stream.querySelector('[data-comment="p4"]') as HTMLElement).getByRole("list", { name: "对 你 的回复" })).toHaveTextContent("收到"));

    await user.type(screen.getAllByRole("combobox", { name: "发言" }).at(-1)!, "新的一条{Meta>}{Enter}{/Meta}");
    await waitFor(() => expect(s.posts).toEqual([{ text: "收到", reply_to: "p4" }, { text: "新的一条" }]));
  });
});
