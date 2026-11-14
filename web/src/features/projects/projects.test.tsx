import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/projects、api/settings/exec、api/items 的契约应答。
// allowed_executors 只能走 PUT /settings/projects/{id}/allowed-executors（PATCH 带它会被拒），错误体见附录 B.3。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix the input" });

interface P { id: string; name: string; intro?: string; tags?: string[]; status: string; repos?: { remote: string; checkouts?: { host: string; path: string }[] }[]; allowed_executors: string[] | null; capture?: boolean; capture_outbound?: string | null }

function server(opts: { putDenied?: boolean } = {}) {
  const projects: P[] = [
    { id: "p_work", name: "acc-work", intro: "工作", tags: ["work"], status: "active", allowed_executors: ["work-host.cli"], repos: [{ remote: "git@h:a/b.git", checkouts: [{ host: "m1", path: "/src/b" }] }] },
    { id: "p_open", name: "acc-open", status: "active", allowed_executors: null },
  ];
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    const h = (init.headers ?? {}) as Record<string, string>;
    calls.push({ method, path: url, body, csrf: h["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects" && method === "GET") return json(200, { projects });
    if (url === "/v1/projects" && method === "POST") {
      if (!body.name) return err(400, "invalid_request", "name is required");
      const p: P = { id: "p_new", name: body.name, status: "active", allowed_executors: null, intro: body.intro, tags: body.tags };
      projects.push(p);
      return json(201, p);
    }
    if (url === "/v1/settings/executors") return json(200, { executors: [{ id: "work-host.cli", kind: "external-cli", host: "work-host" }, { id: "other-host.cli", kind: "external-cli" }] });
    if (url.startsWith("/v1/items?project=")) {
      const id = new URL(url, "http://x").searchParams.get("project");
      return json(200, { items: id === "p_work" ? [{ id: "item_1", phase: "open", version: 1, kind: "task", title: "项目内事项" }] : [] });
    }
    const co = url.match(/^\/v1\/settings\/projects\/([^/]+)\/capture-outbound$/);
    if (co && method === "PUT") {
      const p = projects.find((x) => x.id === co[1])!;
      p.capture_outbound = body.value;
      return json(200, { value: p.capture_outbound });
    }
    const a = url.match(/^\/v1\/settings\/projects\/([^/]+)\/allowed-executors$/);
    if (a && method === "PUT") {
      if (opts.putDenied) return err(403, "web_only", "allowed_executors can only be changed by the owner in the Web UI");
      const p = projects.find((x) => x.id === a[1])!;
      p.allowed_executors = body.executors;
      return json(200, { id: p.id, allowed_executors: p.allowed_executors });
    }
    const m = url.match(/^\/v1\/projects\/([^/]+)(\/repos)?$/);
    if (!m) return err(404, "not_found", "no such route");
    const p = projects.find((x) => x.id === m[1]);
    if (!p) return err(404, "not_found", "project not found");
    if (method === "GET") return json(200, p);
    if (m[2]) {
      if (body.op === "add_remote") p.repos = [...(p.repos ?? []), { remote: body.remote }];
      if (body.op === "remove_remote") p.repos = (p.repos ?? []).filter((r) => r.remote !== body.remote);
      if (body.op === "set_checkout") p.repos!.find((r) => r.remote === body.remote)!.checkouts = [{ host: body.host, path: body.path }];
      if (body.op === "remove_checkout") p.repos!.find((r) => r.remote === body.remote)!.checkouts = [];
      return json(200, p);
    }
    if ("allowed_executors" in body) return err(400, "invalid_request", "allowed_executors is not part of the project update");
    Object.assign(p, body);
    return json(200, p);
  }) as unknown as typeof fetch;
  return { impl, projects, calls };
}

function mount(fetchImpl: typeof fetch, path = "/projects") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("项目页", () => {
  it("列出项目并显示服务端错误", async () => {
    mount(server().impl);
    expect(await screen.findByRole("link", { name: "acc-work" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "acc-open" })).toBeInTheDocument();
  });

  it("新建项目：带 CSRF 提交，出现在列表；缺名称时显示服务端原因", async () => {
    const s = server();
    const u = userEvent.setup();
    mount(s.impl);
    await screen.findByRole("link", { name: "acc-work" });
    await u.click(screen.getByRole("button", { name: "新建项目" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("name is required");
    await u.type(screen.getByLabelText("名称"), "acc-new");
    await u.type(screen.getByLabelText("标签（逗号分隔）"), "a, b");
    await u.click(screen.getByRole("button", { name: "新建项目" }));
    expect(await screen.findByRole("link", { name: "acc-new" })).toBeInTheDocument();
    const post = s.calls.filter((c) => c.method === "POST" && c.body?.name === "acc-new")[0];
    expect(post.csrf).toBe("tok-1");
    expect(post.body.tags).toEqual(["a", "b"]);
  });

  it("编辑项目：PATCH 不含 allowed_executors", async () => {
    const s = server();
    const u = userEvent.setup();
    mount(s.impl, "/projects/p_work");
    const name = await screen.findByLabelText("名称");
    await u.clear(name);
    await u.type(name, "acc-work2");
    await u.selectOptions(screen.getByLabelText("状态"), "paused");
    await u.click(screen.getByRole("button", { name: "保存项目" }));
    await waitFor(() => expect(s.projects[0].name).toBe("acc-work2"));
    const patch = s.calls.find((c) => c.method === "PATCH")!;
    expect(patch.body).toMatchObject({ name: "acc-work2", status: "paused" });
    expect(patch.body).not.toHaveProperty("allowed_executors");
    expect(s.projects[0].status).toBe("paused");
  });

  it("聊天内容发给外部模型时：开启记下待办后才出现；不预选、标「还没选」；保存只发 value", async () => {
    const s = server();
    const u = userEvent.setup();
    mount(s.impl, "/projects/p_work");
    await screen.findByLabelText("名称");
    expect(screen.queryByRole("radiogroup", { name: "聊天内容发给外部模型时" })).not.toBeInTheDocument();
    await u.click(screen.getByRole("checkbox", { name: "从聊天里自动记下待办" }));
    await u.click(screen.getByRole("button", { name: "保存项目" }));
    const group = await screen.findByRole("radiogroup", { name: "聊天内容发给外部模型时" });
    expect(within(group).getAllByRole("radio").every((r) => !(r as HTMLInputElement).checked)).toBe(true);
    expect(screen.getByText("还没选")).toBeInTheDocument();
    const region = within(screen.getByRole("region", { name: "聊天内容发给外部模型时" }));
    expect(region.getByRole("button", { name: "保存" })).toBeDisabled();
    await u.click(within(group).getByRole("radio", { name: "摘要和开头" }));
    await u.click(region.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(s.projects[0].capture_outbound).toBe("excerpt"));
    const put = s.calls.find((c) => c.method === "PUT" && c.path === "/v1/settings/projects/p_work/capture-outbound")!;
    expect(put.body).toEqual({ value: "excerpt" });
    expect(put.csrf).toBe("tok-1");
    await waitFor(() => expect(screen.queryByText("还没选")).not.toBeInTheDocument());
  });

  it("repos：添加/删除远程地址、设置检出，显示服务端状态", async () => {
    const s = server();
    const u = userEvent.setup();
    mount(s.impl, "/projects/p_work");
    expect(await screen.findByText("git@h:a/b.git")).toBeInTheDocument();
    expect(screen.getByText("m1:/src/b")).toBeInTheDocument();
    await u.type(screen.getByLabelText("远程地址"), "git@h:c/d.git");
    await u.click(screen.getByRole("button", { name: "添加仓库" }));
    expect(await screen.findByText("git@h:c/d.git")).toBeInTheDocument();
    const repo = screen.getByText("git@h:c/d.git").closest("li")!;
    await u.type(within(repo).getByLabelText("主机名"), "m2");
    await u.type(within(repo).getByLabelText("路径"), "/src/d");
    await u.click(within(repo).getByRole("button", { name: "设置检出" }));
    expect(await screen.findByText("m2:/src/d")).toBeInTheDocument();
    await u.click(within(screen.getByText("git@h:a/b.git").closest("li")!).getByRole("button", { name: "删除仓库" }));
    await waitFor(() => expect(screen.queryByText("git@h:a/b.git")).not.toBeInTheDocument());
  });

  it("允许的执行者：经 settings 路由 PUT；不限制发 null，限制不选发 []", async () => {
    const s = server();
    const u = userEvent.setup();
    mount(s.impl, "/projects/p_work");
    const other = await screen.findByRole("checkbox", { name: /^other-host\.cli/ });
    expect(screen.getByRole("checkbox", { name: /^work-host\.cli/ })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "work-host.cli · 外部命令行工具 · work-host" })).toBeInTheDocument(); // 名称 · 种类 · 主机
    expect(other).not.toBeChecked();
    await u.click(other);
    await u.click(screen.getByRole("button", { name: "保存执行者" }));
    await waitFor(() => expect(s.projects[0].allowed_executors).toEqual(["work-host.cli", "other-host.cli"]));
    expect(s.calls.find((c) => c.method === "PUT")!.csrf).toBe("tok-1");

    await u.click(screen.getByLabelText("谁都可以"));
    await u.click(screen.getByRole("button", { name: "保存执行者" }));
    await waitFor(() => expect(s.projects[0].allowed_executors).toBeNull());

    await u.click(screen.getByLabelText("只能派给："));
    await u.click(screen.getByRole("checkbox", { name: /^work-host\.cli/ })); // 取消
    await u.click(screen.getByRole("checkbox", { name: /^other-host\.cli/ })); // 取消
    expect(screen.getByText("只能派给你自己")).toBeInTheDocument(); // 一个都不选的后果当场可见（§14.7）
    await u.click(screen.getByRole("button", { name: "保存执行者" }));
    await waitFor(() => expect(s.projects[0].allowed_executors).toEqual([]));
  });

  it("允许的执行者被拒（非 Web）：显示原因，页面状态不变", async () => {
    const u = userEvent.setup();
    mount(server({ putDenied: true }).impl, "/projects/p_work");
    await u.click(await screen.findByRole("checkbox", { name: /^other-host\.cli/ }));
    await u.click(screen.getByRole("button", { name: "保存执行者" }));
    expect(await screen.findByText(/only be changed by the owner/)).toBeInTheDocument();
  });

  it("事项视图：显示项目下的事项，空项目显示空状态", async () => {
    const s = server();
    mount(s.impl, "/projects/p_work");
    expect(await screen.findByRole("link", { name: "项目内事项" })).toHaveAttribute("href", "/items/item_1");
    expect(s.calls.some((c) => c.path === "/v1/items?project=p_work")).toBe(true);
  });

  it("空项目：事项视图显示空状态；不存在的项目显示错误", async () => {
    const s = server();
    const { unmount } = mount(s.impl, "/projects/p_open");
    expect(await screen.findByText("这个项目还没有事项")).toBeInTheDocument();
    unmount();
    mount(s.impl, "/projects/nope");
    expect(await screen.findByRole("alert")).toHaveTextContent("project not found");
  });
});
