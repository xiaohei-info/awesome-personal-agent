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
    ] });
    if (url === "/v1/items/itm_a/comments" && m === "POST") {
      const b = JSON.parse(String(init.body));
      posts.push(b);
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

  it("点回复写一条带 reply_to，写完出现在被回复的那条下面；顶部输入框写的不带", async () => {
    const s = server();
    mount(s.impl);
    const user = userEvent.setup();
    const stream = await screen.findByRole("list", { name: "讨论" });
    const last = stream.querySelector('[data-comment="p4"]') as HTMLElement;
    await user.click(within(last).getByRole("button", { name: "回复" }));
    await user.type(within(last).getByRole("textbox", { name: "回复 你" }), "收到");
    await user.click(within(last).getByRole("button", { name: "发送" }));
    await waitFor(() => expect(s.posts).toEqual([{ text: "收到", reply_to: "p4" }]));
    await waitFor(() => expect(within(stream.querySelector('[data-comment="p4"]') as HTMLElement).getByRole("list", { name: "对 你 的回复" })).toHaveTextContent("收到"));

    await user.type(screen.getAllByRole("textbox", { name: "发言" }).at(-1)!, "新的一条{Meta>}{Enter}{/Meta}");
    await waitFor(() => expect(s.posts).toEqual([{ text: "收到", reply_to: "p4" }, { text: "新的一条" }]));
  });
});
