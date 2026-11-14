import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/settings/purge 的契约应答（POST /v1/settings/purge {object}；写请求带 CSRF）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function server(o: { deny?: boolean } = {}) {
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url.startsWith("/v1/items?")) return json(200, { items: new URL(url, "http://x").searchParams.get("q") === "续保" ? [{ id: "it-1", title: "续保材料确认" }] : [] });
    if (url.startsWith("/v1/materials/search?")) return json(200, { hits: [] });
    if (url === "/v1/settings/purge" && method === "POST") {
      if (o.deny) return json(403, { code: "web_only", why: "purge can only be used by the owner in the Web UI", next: "open the Web UI" });
      if (body.object === "mat-1") return json(200, { object: "mat-1", kind: "material", events: 3, blobs_removed: ["aa11"], blobs_retained: ["bb22"] });
      if (body.object === "it-1") return json(200, { object: "it-1", kind: "item", events: 5 });
      return json(404, { code: "not_found", why: "object not found", next: "check the id" });
    }
    return json(404, { code: "not_found", why: "no such route", next: "-" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/purge"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const posts = (s: ReturnType<typeof server>) => s.calls.filter((c) => c.method === "POST" && c.path === "/v1/settings/purge");

beforeEach(() => sessionStorage.clear());

describe("物理删除（flow-purge / ACT-PRG-01）", () => {
  it("确认前明确提示不可恢复且不发请求；取消后仍不发请求", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.type(await screen.findByLabelText("要删除的事项或材料（标题或编号）"), "it-1");
    await userEvent.click(screen.getByRole("button", { name: "删除…" }));
    expect(screen.getByRole("alertdialog")).toHaveTextContent("无法找回");
    expect(screen.getByRole("alertdialog")).toHaveTextContent("it-1");
    expect(posts(s)).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "取消" }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(posts(s)).toHaveLength(0);
  });

  it("确认后 POST {object} 带 CSRF，如实展示墓碑条数与 blob 删除/保留", async () => {
    const s = server();
    mount(s.impl);
    await userEvent.type(await screen.findByLabelText("要删除的事项或材料（标题或编号）"), "  mat-1 ");
    await userEvent.click(screen.getByRole("button", { name: "删除…" }));
    await userEvent.click(screen.getByRole("button", { name: "确认永久删除" }));
    const res = await screen.findByRole("status");
    expect(res).toHaveTextContent("mat-1");
    expect(res).toHaveTextContent("3");
    expect(res).toHaveTextContent("aa11");
    expect(res).toHaveTextContent("bb22");
    const w = posts(s)[0];
    expect(w.body).toEqual({ object: "mat-1" });
    expect(w.csrf).toBe("tok-1");
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(screen.getByLabelText("要删除的事项或材料（标题或编号）")).toHaveValue("");
  });

  it("按标题找到对象再删：确认框写标题，请求仍按编号发送；找不到时说明还可以按编号删", async () => {
    const s = server();
    mount(s.impl);
    const box = await screen.findByLabelText("要删除的事项或材料（标题或编号）");
    await userEvent.type(box, "没有这个");
    await userEvent.click(screen.getByRole("button", { name: "查找" }));
    expect(await screen.findByText("没找到，可直接按编号删除")).toBeInTheDocument();
    await userEvent.clear(box);
    await userEvent.type(box, "续保");
    await userEvent.click(screen.getByRole("button", { name: "查找" }));
    await userEvent.click(await screen.findByRole("radio", { name: /续保材料确认/ }));
    await userEvent.click(screen.getByRole("button", { name: "删除…" }));
    expect(screen.getByRole("alertdialog")).toHaveTextContent("永久删除「续保材料确认」，无法找回");
    await userEvent.click(screen.getByRole("button", { name: "确认永久删除" }));
    expect(await screen.findByRole("status")).toHaveTextContent("已永久删除「续保材料确认」");
    expect(posts(s)[0].body).toEqual({ object: "it-1" });
  });

  it("空 id 不能发起删除", async () => {
    mount(server().impl);
    expect(await screen.findByRole("button", { name: "删除…" })).toBeDisabled();
  });

  it("服务端拒绝（web_only / 不存在）时展示 why，不显示成功", async () => {
    const s = server({ deny: true });
    mount(s.impl);
    await userEvent.type(await screen.findByLabelText("要删除的事项或材料（标题或编号）"), "it-1");
    await userEvent.click(screen.getByRole("button", { name: "删除…" }));
    await userEvent.click(screen.getByRole("button", { name: "确认永久删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("only be used by the owner");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    const s2 = server();
    document.body.innerHTML = "";
    mount(s2.impl);
    await userEvent.type(await screen.findByLabelText("要删除的事项或材料（标题或编号）"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "删除…" }));
    await userEvent.click(screen.getByRole("button", { name: "确认永久删除" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("object not found");
  });
});
