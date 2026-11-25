import { withSession } from "../../app/test-session";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as materials } from "./index";
import { feature as projects } from "../projects/index";

// 测试边界替身：按 GET /materials、GET/PATCH /materials/{id}（附录 B.2、§12.8）应答。
// 授权、位置不可改、非链接型 409 是服务端规则（api/materials/link_test.go 已验）；替身只复现可观察行为。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface Mat { id: string; project: string; kind: string; title: string; tags?: string[]; form: string; link?: string; open_url?: string; current_rev?: string }

const vaultLink = (): Mat => ({ id: "mat_v", project: "acc-work", kind: "reference", title: "方案A", tags: ["a"], form: "link", link: "vault:notes/方案 A.md", open_url: "obsidian://open?vault=notes&file=%E6%96%B9%E6%A1%88%20A.md" });
const webLink = (): Mat => ({ id: "mat_w", project: "acc-work", kind: "reference", title: "外部文档", form: "link", link: "https://example.invalid/doc" });
const noOpen = (): Mat => ({ id: "mat_n", project: "acc-work", kind: "reference", title: "无打开方式", form: "link", link: "vault:gone/x.md" });
const owned = (): Mat => ({ id: "mat_o", project: "acc-work", kind: "minutes", title: "周会纪要", form: "owned", current_rev: "r1" });

function backend(mats: Mat[], patchFails = false) {
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    const u = new URL(url, "http://x");
    const q = u.pathname;
    if (q === "/v1/projects") return json(200, { projects: [{ id: "acc-work", name: "acc-work", status: "active" }] });
    if (q === "/v1/projects/acc-work") return json(200, { id: "acc-work", name: "acc-work", status: "active" });
    if (q === "/v1/items") return json(200, { items: [] });
    if (q === "/v1/materials") {
      const p = u.searchParams.get("project");
      return json(200, { materials: mats.filter((m) => !p || m.project === p) });
    }
    const m = q.match(/^\/v1\/materials\/([^/]+)$/);
    const mat = m && mats.find((x) => x.id === m[1]);
    if (!mat) return json(404, { code: "not_found", why: "material does not exist", next: "check the id" });
    if (method === "PATCH") {
      if (patchFails) return json(403, { code: "forbidden", why: "only the signed-in owner edits this", next: "sign in on the web" });
      Object.assign(mat, body);
      return json(200, { material: mat.id });
    }
    return json(200, { material: mat, relations: [] });
  }) as unknown as typeof fetch;
  return { impl, calls, mats };
}

function mount(fetchImpl: typeof fetch, path = "/materials") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[materials, projects]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("链接型材料显示（ACT-WVM-02）", () => {
  it("材料页：链接型标明形态、显示位置，库内的“打开”是 open_url，网址型是网址本身，库未登记打开方式时不给打开", async () => {
    mount(backend([vaultLink(), webLink(), noOpen(), owned()]).impl);
    await screen.findByRole("button", { name: "方案A" });
    const rows = screen.getAllByRole("listitem");
    const row = (title: string) => rows.find((l) => l.textContent?.includes(title))!;
    expect(row("方案A")).toHaveTextContent("链接型");
    expect(row("方案A")).toHaveTextContent("vault:notes/方案 A.md");
    expect(within(row("方案A")).getByRole("link", { name: "打开" })).toHaveAttribute("href", vaultLink().open_url);
    expect(within(row("外部文档")).getByRole("link", { name: "打开" })).toHaveAttribute("href", "https://example.invalid/doc");
    expect(within(row("无打开方式")).queryByRole("link", { name: "打开" })).toBeNull();
    expect(row("无打开方式")).toHaveTextContent("该库未登记打开方式");
    expect(row("周会纪要")).toHaveTextContent("在 APA 里");
    expect(within(row("周会纪要")).queryByRole("link", { name: "打开" })).toBeNull();
  });

  it("详情：位置只读（没有可编辑的位置输入）；改标题、kind、标签发 PATCH，不带位置与正文，带 CSRF，保存后列表刷新", async () => {
    const b = backend([vaultLink()]);
    mount(b.impl);
    await userEvent.click(await screen.findByRole("button", { name: "方案A" }));
    const d = within(await screen.findByRole("region", { name: "材料详情" }));
    expect(d.getByText("vault:notes/方案 A.md")).toBeInTheDocument();
    expect(d.queryByRole("textbox", { name: /位置/ })).toBeNull();
    expect(d.queryByLabelText("Markdown 内容")).toBeNull();
    const title = d.getByLabelText("标题");
    await userEvent.clear(title);
    await userEvent.type(title, "方案B");
    await userEvent.selectOptions(d.getByLabelText("类型"), "minutes");
    const tags = d.getByLabelText("标签（逗号分隔）");
    await userEvent.clear(tags);
    await userEvent.type(tags, "x, y ,");
    await userEvent.click(d.getByRole("button", { name: "保存" }));
    expect(await screen.findByRole("status")).toHaveTextContent("已保存");
    const patch = b.calls.find((c) => c.method === "PATCH")!;
    expect(patch.path).toBe("/v1/materials/mat_v");
    expect(patch.body).toEqual({ title: "方案B", kind: "minutes", tags: ["x", "y"] });
    expect(patch.csrf).toBe("tok-1");
    expect(await screen.findByRole("button", { name: "方案B" })).toBeInTheDocument();
  });

  it("服务端拒绝修改：显示原因，不假装保存", async () => {
    mount(backend([vaultLink()], true).impl);
    await userEvent.click(await screen.findByRole("button", { name: "方案A" }));
    const d = within(await screen.findByRole("region", { name: "材料详情" }));
    await userEvent.click(d.getByRole("button", { name: "保存" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("only the signed-in owner edits this");
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("项目页材料区：链接型标明形态、位置与“打开”，与自有型并列", async () => {
    mount(backend([vaultLink(), webLink(), owned()]).impl, "/projects/acc-work");
    const mats = within(await screen.findByRole("region", { name: "材料" }));
    const lis = await mats.findAllByRole("listitem");
    const row = (title: string) => lis.find((l) => l.textContent?.includes(title))!;
    expect(row("方案A")).toHaveTextContent("链接型");
    expect(row("方案A")).toHaveTextContent("vault:notes/方案 A.md");
    expect(within(row("方案A")).getByRole("link", { name: "打开" })).toHaveAttribute("href", vaultLink().open_url);
    expect(within(row("外部文档")).getByRole("link", { name: "打开" })).toHaveAttribute("href", "https://example.invalid/doc");
    expect(row("周会纪要")).toHaveTextContent("在 APA 里");
  });
});
