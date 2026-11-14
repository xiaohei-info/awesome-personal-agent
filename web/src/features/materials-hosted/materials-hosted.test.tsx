import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as materials } from "../materials/index";
import { feature as projects } from "../projects/index";

// 测试边界替身：按 api/materials（/content 三态、/changes）、api/settings、api/projects 的契约应答。
// 允许的执行者校验、父事项来源是服务端规则——替身只复现可观察行为，页面只能如实展示。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix the request" });

const hostedMat = { id: "mat_h", project: "acc-work", kind: "design", title: "登录方案", location: "git@h:a/b.git:docs/design.md" };
const ownMat = { id: "mat_o", project: "acc-work", kind: "minutes", title: "周会纪要", current_rev: "r1" };
const guide = { id: "mat_g", project: "acc-work", kind: "guideline", title: "项目规范", current_rev: "g1" };
const otherMat = { id: "mat_x", project: "acc-open", kind: "minutes", title: "别的项目纪要", current_rev: "x1" };

function server(o: { content: Record<string, unknown>[]; allowed?: string[] }) {
  const calls: { method: string; path: string; body?: any; headers: Record<string, string> }[] = [];
  let reads = 0;
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, headers: (init.headers ?? {}) as Record<string, string> });
    const u = new URL(url, "http://x");
    const all = [hostedMat, ownMat, guide, otherMat];
    if (u.pathname === "/v1/projects") return json(200, { projects: [{ id: "acc-work", name: "acc-work", status: "active", allowed_executors: o.allowed ?? null }] });
    if (u.pathname === "/v1/projects/acc-work") return json(200, { id: "acc-work", name: "acc-work", status: "active", allowed_executors: null });
    if (u.pathname === "/v1/items") return json(200, { items: [] });
    if (u.pathname === "/v1/settings/executors") return json(200, { executors: [{ id: "work-host.cli", kind: "cli" }, { id: "third.cli", kind: "cli" }] });
    if (u.pathname === "/v1/materials") {
      const p = u.searchParams.get("project");
      return json(200, { materials: all.filter((m) => !p || m.project === p) });
    }
    if (u.pathname === "/v1/materials/mat_h") return json(200, { material: hostedMat, hosted: { repo: "git@h:a/b.git", path: "docs/design.md" } });
    if (u.pathname === "/v1/materials/mat_g") return json(200, { material: guide, revision: { id: "g1", content: "## 处理规则\n先问再做" } });
    if (u.pathname === "/v1/materials/mat_h/content") return json(200, o.content[Math.min(reads++, o.content.length - 1)]);
    if (u.pathname === "/v1/materials/mat_h/changes" && method === "POST") {
      if (!body.request) return err(400, "invalid_request", "request is required");
      if (o.allowed && !o.allowed.includes(body.assignee)) return err(403, "executor_not_allowed", "assignee is not an allowed executor for this project");
      return json(201, { item: "item_child", parent: "item_parent" });
    }
    return err(404, "not_found", "no such route");
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch, path: string) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[materials, projects]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const READY = { state: "ready", content: "# 默认分支正文", commit: "c0ffee", repo: "git@h:a/b.git", path: "docs/design.md" };
const openHosted = async () => {
  await userEvent.click(await screen.findByRole("button", { name: "登录方案" }));
  return within(await screen.findByRole("region", { name: "材料详情" }));
};

beforeEach(() => sessionStorage.clear());

describe("ACT-HST-03 只读展示默认分支内容并标明 commit", () => {
  it("先读取中，轮询到 ready 后展示正文与 commit，且没有编辑框", async () => {
    mount(server({ content: [{ state: "reading" }, READY] }).impl, "/materials");
    const d = await openHosted();
    expect(await d.findByText("读取中…")).toBeInTheDocument();
    await waitFor(() => expect(d.getByTestId("hosted-content")).toHaveTextContent("# 默认分支正文"), { timeout: 5000 });
    expect(d.getByText(/最近 commit c0ffee/)).toBeInTheDocument();
    expect(d.queryByLabelText("Markdown 内容")).not.toBeInTheDocument();
    expect(d.queryByText("读取中…")).not.toBeInTheDocument();
  });
});

describe("ACT-HST-04 读不到时降级", () => {
  it("只显示登记信息和暂不可读，页面不报错", async () => {
    mount(server({ content: [{ state: "unavailable", reason: "remote unreachable", repo: "git@h:a/b.git", path: "docs/design.md" }] }).impl, "/materials");
    const d = await openHosted();
    expect(await d.findByText(/暂不可读：remote unreachable/)).toBeInTheDocument();
    expect(d.getByText(/登记信息：git@h:a\/b.git · docs\/design.md/)).toBeInTheDocument();
    expect(d.queryByRole("alert")).not.toBeInTheDocument();
    expect(d.queryByTestId("hosted-content")).not.toBeInTheDocument();
  });

  it("读取接口真的出错时如实显示原因", async () => {
    const s = server({ content: [READY] });
    const bad = ((url: string, init?: RequestInit) => (url.endsWith("/content") ? Promise.resolve(err(500, "internal", "store is unavailable")) : s.impl(url, init))) as typeof fetch;
    mount(bad, "/materials");
    const d = await openHosted();
    expect(await d.findByRole("alert")).toHaveTextContent("store is unavailable");
  });
});

describe("ACT-HST-06 发起修改走委派", () => {
  it("填写修改内容、选执行者，生成子事项并带幂等键", async () => {
    const s = server({ content: [READY], allowed: ["work-host.cli"] });
    mount(s.impl, "/materials");
    const d = await openHosted();
    await userEvent.click(d.getByRole("button", { name: "交给执行者去仓库改" }));
    expect(await d.findByRole("alert")).toHaveTextContent("request is required");
    await userEvent.selectOptions(d.getByLabelText("执行者"), "work-host.cli");
    await userEvent.type(d.getByLabelText("修改要求"), "修正错别字");
    await userEvent.click(d.getByRole("button", { name: "交给执行者去仓库改" }));
    expect(await d.findByRole("link", { name: "item_child" })).toHaveAttribute("href", "/items/item_child");
    const post = s.calls.filter((c) => c.method === "POST" && c.path === "/v1/materials/mat_h/changes").at(-1)!;
    expect(post.body).toEqual({ assignee: "work-host.cli", request: "修正错别字" });
    expect(post.headers["Idempotency-Key"]).toBeTruthy();
    expect(post.headers["X-CSRF-Token"]).toBe("tok-1");
  });

  it("执行者不在项目允许范围内：显示服务端的拒绝，不显示成功", async () => {
    mount(server({ content: [READY], allowed: ["work-host.cli"] }).impl, "/materials");
    const d = await openHosted();
    await userEvent.selectOptions(d.getByLabelText("执行者"), "third.cli");
    await userEvent.type(d.getByLabelText("修改要求"), "x");
    await userEvent.click(d.getByRole("button", { name: "交给执行者去仓库改" }));
    expect(await d.findByRole("alert")).toHaveTextContent("not an allowed executor");
    expect(d.queryByRole("link", { name: "item_child" })).not.toBeInTheDocument();
  });

  it("被拒绝后改选允许的执行者重提：换新幂等键；结果未知（5xx）重试则沿用同一个键", async () => {
    const s = server({ content: [READY], allowed: ["work-host.cli"] });
    let fail5xx = true;
    const keys: string[] = [];
    const flaky = ((url: string, init?: RequestInit) => {
      if (url.endsWith("/changes")) keys.push((init!.headers as Record<string, string>)["Idempotency-Key"]);
      if (url.endsWith("/changes") && init?.method === "POST" && JSON.parse(init.body as string).assignee === "work-host.cli" && fail5xx) {
        fail5xx = false;
        return Promise.resolve(err(503, "internal", "try later"));
      }
      return s.impl(url, init);
    }) as typeof fetch;
    mount(flaky, "/materials");
    const d = await openHosted();
    await userEvent.type(d.getByLabelText("修改要求"), "x");
    await userEvent.selectOptions(d.getByLabelText("执行者"), "third.cli");
    await userEvent.click(d.getByRole("button", { name: "交给执行者去仓库改" }));
    await d.findByRole("alert");
    await userEvent.selectOptions(d.getByLabelText("执行者"), "work-host.cli");
    await userEvent.click(d.getByRole("button", { name: "交给执行者去仓库改" }));
    await waitFor(() => expect(d.getByRole("alert")).toHaveTextContent("try later"));
    await userEvent.click(d.getByRole("button", { name: "交给执行者去仓库改" }));
    expect(await d.findByRole("link", { name: "item_child" })).toBeInTheDocument();
    expect(keys).toHaveLength(3); // 403、503、成功
    expect(keys[1]).not.toBe(keys[0]); // 明确被拒后换新键
    expect(keys[2]).toBe(keys[1]); // 结果未知后重试沿用
  });
});

describe("ACT-HST-08 项目页集中查看", () => {
  it("材料视图只含本项目、托管型与自有型并列标明来源；规范单列；托管型只读且可展开", async () => {
    mount(server({ content: [READY] }).impl, "/projects/acc-work");
    const mats = within(await screen.findByRole("region", { name: "材料" }));
    expect(await mats.findByText(/登录方案/)).toBeInTheDocument();
    const lis = mats.getAllByRole("listitem");
    expect(lis.find((l) => l.textContent?.includes("登录方案"))).toHaveTextContent("在仓库里（只读）");
    expect(lis.find((l) => l.textContent?.includes("周会纪要"))).toHaveTextContent("在 APA 里");
    expect(mats.queryByText("别的项目纪要")).not.toBeInTheDocument();
    expect(mats.queryByText("项目规范")).not.toBeInTheDocument();
    const rules = within(screen.getByRole("region", { name: "规范" }));
    expect(await rules.findByText(/先问再做/)).toBeInTheDocument();
    await userEvent.click(mats.getByRole("button", { name: "登录方案" }));
    expect(await screen.findByTestId("hosted-content")).toHaveTextContent("# 默认分支正文");
    expect(screen.getByRole("region", { name: "仓库" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "允许的执行者" })).toBeInTheDocument();
  });
});
