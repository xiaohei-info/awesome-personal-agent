import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 替身只回答 GET/PUT /v1/settings/note-vaults：整体替换，重名 / 脚本类协议返回 400 且什么都不存。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
function server(initial: { name: string; open_url: string }[]) {
  let vaults = initial, version = 1;
  const puts: { vaults: { name: string; open_url: string }[] }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    if (url === "/v1/projects") return json(200, { projects: [] }); // 会话探针
    if (url !== "/v1/settings/note-vaults") return json(404, { code: "not_found", why: "no such route", next: "" });
    if ((init.method ?? "GET") === "GET") return json(200, { version, vaults });
    const body = JSON.parse(init.body as string);
    puts.push(body);
    if (body.vaults.some((v: { open_url: string }) => /^javascript:/.test(v.open_url))) return json(400, { code: "invalid_request", why: "open_url protocol is not allowed", next: "fix it; nothing was saved" });
    vaults = body.vaults;
    version++;
    return json(200, { version, vaults });
  }) as unknown as typeof fetch;
  return { impl, puts };
}
function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/note-vaults"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const CARD = "我的笔记库";
const card = async () => within(await screen.findByRole("region", { name: CARD }));
const save = async () => userEvent.click((await card()).getByRole("button", { name: `保存${CARD}` }));

beforeEach(() => sessionStorage.clear());

describe("设置 › 笔记库", () => {
  it("ACT-WVM-01: 每库一行可增删改；保存前先看到用示例路径渲染的打开地址；PUT 整体替换", async () => {
    const s = server([{ name: "notes", open_url: "app://open?v={vault}&f={path}" }, { name: "old", open_url: "https://x.example/{path}" }]);
    mount(s.impl);
    const c = await card();
    expect(c.getByLabelText("库1 名称")).toHaveValue("notes");
    expect(c.getByLabelText("库1 示例地址")).toHaveTextContent("app://open?v=notes&f=folder%2Fexample%20note.md");

    await userEvent.clear(c.getByLabelText("库1 打开方式"));
    await userEvent.type(c.getByLabelText("库1 打开方式"), "https://wiki.example/{{vault}/{{path}");
    expect(c.getByLabelText("库1 示例地址")).toHaveTextContent("https://wiki.example/notes/folder%2Fexample%20note.md");
    await userEvent.click(c.getByRole("button", { name: "删除库 2" }));
    await userEvent.click(c.getByRole("button", { name: "＋ 添加笔记库" }));
    await userEvent.type(c.getByLabelText("库2 名称"), " a b ");
    await userEvent.type(c.getByLabelText("库2 打开方式"), "app://{{vault}");
    expect(s.puts).toHaveLength(0); // 保存前没有写任何东西
    await save();
    await c.findByRole("status");
    expect(s.puts).toEqual([{ vaults: [{ name: "notes", open_url: "https://wiki.example/{vault}/{path}" }, { name: "a b", open_url: "app://{vault}" }] }]);
  });

  it("全部删掉也能保存：PUT 空数组", async () => {
    const s = server([{ name: "notes", open_url: "app://{vault}" }]);
    mount(s.impl);
    const c = await card();
    await userEvent.click(c.getByRole("button", { name: "删除库 1" }));
    expect(c.getByText("还没有登记笔记库")).toBeInTheDocument();
    await save();
    await c.findByRole("status");
    expect(s.puts).toEqual([{ vaults: [] }]);
  });

  it("本地拒绝空名与重名：不发请求，原因显示在卡片里", async () => {
    const s = server([{ name: "a", open_url: "app://a" }, { name: "a", open_url: "app://b" }]);
    mount(s.impl);
    const c = await card();
    await save();
    expect(await c.findByRole("alert")).toHaveTextContent("库名「a」重复");
    await userEvent.clear(c.getByLabelText("库1 名称"));
    await save();
    expect(await c.findByRole("alert")).toHaveTextContent("第 1 行的库名不能为空");
    expect(s.puts).toHaveLength(0);
  });

  it("服务端拒绝（脚本类协议）：显示原因，草稿保留", async () => {
    const s = server([]);
    mount(s.impl);
    const c = await card();
    await userEvent.click(c.getByRole("button", { name: "＋ 添加笔记库" }));
    await userEvent.type(c.getByLabelText("库1 名称"), "bad");
    await userEvent.type(c.getByLabelText("库1 打开方式"), "javascript:alert(1)");
    await save();
    expect(await c.findByRole("alert")).toHaveTextContent("open_url protocol is not allowed");
    expect(c.getByLabelText("库1 打开方式")).toHaveValue("javascript:alert(1)");
  });

  it("读取失败给原因和重试", async () => {
    mount((async (u: string) => (u === "/v1/projects" ? json(200, { projects: [] }) : json(500, { code: "internal", why: "boom", next: "retry" }))) as unknown as typeof fetch);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
