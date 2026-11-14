import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "../items/index";

// 测试边界替身：按 GET /v1/items/{id}/messages 的契约应答（Web 登录拿全部消息，forward_allowed 由服务端给出）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const MESSAGES = [
  { id: "m1", source: "gh", sender: "alice", sender_verified: true, subject: "s-m1", occurred_at: "2030-01-01T00:00:00Z", attach_state: "pending", forward_allowed: true },
  { id: "m2", source: "gh", sender: "mallory", sender_verified: false, subject: "s-m2", occurred_at: "2030-01-01T00:01:00Z", attach_state: "pending", forward_allowed: false },
  { id: "m3", source: "gh", subject: "s-m3", occurred_at: "2030-01-01T00:02:00Z", attach_state: "processed", forward_allowed: false },
];

function fetchFor(messages: unknown, status = 200) {
  return (async (url: string) => {
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items") return json(200, { items: [] });
    if (url.startsWith("/v1/why/")) return json(200, { steps: [] });
    if (url === "/v1/items/item_a/messages") return json(status, status === 200 ? { messages } : messages);
    if (url === "/v1/items/item_a") return json(200, { item: { id: "item_a", phase: "open", version: 1, kind: "task", title: "item-a" }, relations: [] });
    return json(404, { code: "not_found", why: "no such route", next: "check" });
  }) as unknown as typeof fetch;
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/items/item_a"]}>
      <App features={[feature]} fetchImpl={f} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("事项页挂靠消息（ACT-FWD-04）", () => {
  it("显示全部消息及发送者、核实状态、后台能否读到；读不到的标明并给出加入可读名单的入口", async () => {
    mount(fetchFor(MESSAGES));
    const list = within(await screen.findByRole("list", { name: "跟进消息列表" }));
    const rows = list.getAllByRole("listitem");
    expect(rows).toHaveLength(3);

    expect(within(rows[0]).getByLabelText("发送者")).toHaveTextContent("alice");
    expect(within(rows[0]).getByLabelText("核实状态")).toHaveTextContent("已核实");
    expect(within(rows[0]).getByLabelText("后台 agent 能否读到")).toHaveTextContent("后台 agent 可读");
    expect(within(rows[0]).queryByRole("link", { name: "加入可读名单" })).toBeNull();

    expect(within(rows[1]).getByLabelText("发送者")).toHaveTextContent("mallory");
    expect(within(rows[1]).getByLabelText("核实状态")).toHaveTextContent("未核实");
    expect(within(rows[1]).getByLabelText("后台 agent 能否读到")).toHaveTextContent("后台 agent 看不到");
    expect(within(rows[1]).getByLabelText("处理状态")).toHaveTextContent("待转给 agent");
    // 名单外：后台 agent 看不到（上一行），处理状态仍是待转（上上行）；给出去设置可读名单的入口
    expect(within(rows[1]).getByRole("link", { name: "加入可读名单" })).toHaveAttribute("href", "/settings/access");

    // 发送者缺失：显示为未知且未核实，不伪造发送者
    expect(within(rows[2]).getByLabelText("发送者")).toHaveTextContent("未知发送者");
    expect(within(rows[2]).getByLabelText("核实状态")).toHaveTextContent("未核实");
  });

  it("来源 link 只有 http(s) 才渲染成链接；javascript: 等不产生 href", async () => {
    const msgs = [
      { ...MESSAGES[0], id: "l1", subject: "ok", link: "https://example.com/pr/1" },
      { ...MESSAGES[0], id: "l2", subject: "evil", link: "javascript:alert(1)" },
      { ...MESSAGES[0], id: "l3", subject: "junk", link: "not a url" },
    ];
    mount(fetchFor(msgs));
    const rows = within(await screen.findByRole("list", { name: "跟进消息列表" })).getAllByRole("listitem");
    const ok = within(rows[0]).getByRole("link", { name: "来源" });
    expect(ok).toHaveAttribute("href", "https://example.com/pr/1");
    expect(ok).toHaveAttribute("rel", "noopener noreferrer");
    expect(within(rows[1]).queryByRole("link")).toBeNull();
    expect(within(rows[2]).queryByRole("link")).toBeNull();
    expect(document.querySelector('a[href^="javascript"]')).toBeNull();
  });

  it("没有挂靠消息时显示空状态", async () => {
    mount(fetchFor([]));
    expect(await screen.findByText("还没有跟进消息")).toBeInTheDocument();
  });

  it("读取失败时显示服务端原因，不显示空列表", async () => {
    mount(fetchFor({ code: "permission_denied", why: "credential cannot read messages", next: "log in" }, 403));
    expect(await screen.findByRole("alert")).toHaveTextContent("credential cannot read messages");
    expect(screen.queryByText("还没有跟进消息")).not.toBeInTheDocument();
  });
});
