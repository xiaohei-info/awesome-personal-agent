import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 替身只回答 POST /v1/import、GET /v1/imports、GET /v1/audit/events、POST /v1/settings/grants，形状同附录 B.2。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const BATCHES = [{ id: "imp_big", source: "big", created: 520, existing: 0, invalid: 0, started_at: "2026-10-02T02:00:00Z", actor: "owner" }, { id: "imp_old", source: "legacy", created: 3, existing: 1, invalid: 0, started_at: "2026-10-01T02:00:00Z", actor: "owner", grant: "gr_1" }];

function server(opts: { failChunk?: number } = {}) {
  const posts: { url: string; lines: number; ct: string | null }[] = [];
  const grants: unknown[] = [];
  let calls = 0;
  const impl = (async (url: string, init: RequestInit = {}) => {
    const h = (init.headers ?? {}) as Record<string, string>;
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/imports") return json(200, { batches: BATCHES });
    if (url.startsWith("/v1/audit/events?")) {
      const q = new URL(url, "http://x").searchParams;
      if (q.get("imported") === "imp_big") { // 520 条，升序，limit 500，after=<seq> 取其后一页（B.2）
        const after = Number(q.get("after") ?? 0), lim = Number(q.get("limit"));
        const all = Array.from({ length: 520 }, (_, i) => ({ seq: i + 1, object: `itm_${i + 1}` }));
        return json(200, { events: all.filter((e) => e.seq > after).slice(0, lim) });
      }
      return json(200, { events: q.get("imported") === "imp_old" && q.get("type") === "item.created" ? [{ seq: 1, object: "itm_a" }, { seq: 2, object: "itm_b" }] : [] });
    }
    if (url.startsWith("/v1/import?")) {
      const q = new URL(url, "http://x").searchParams;
      const rows = (init.body as string).split("\n").filter(Boolean);
      posts.push({ url, lines: rows.length, ct: h["Content-Type"] });
      calls++;
      if (opts.failChunk === calls) return json(403, { code: "invalid_request", why: "disk is full", next: "retry" });
      const results = rows.map((r, i) => (r.includes("bad") ? { line: i + 1, external_id: "BAD", outcome: "invalid", code: "invalid_request", why: "history must start with created" } : { line: i + 1, outcome: "created" }));
      const created = results.filter((r) => r.outcome === "created").length;
      return json(200, { ...(q.get("dry_run") ? {} : { batch: "imp_new" }), results, created, existing: 0, invalid: results.length - created });
    }
    if (url === "/v1/settings/grants" && init.method === "POST") {
      grants.push(JSON.parse(init.body as string));
      return json(201, { id: "gr_9" });
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, posts, grants };
}
function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings/import"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const file = (lines: string[]) => new File([lines.join("\n")], "plan.jsonl", { type: "text/plain" });
async function choose(lines: string[], source = "legacy") {
  fireEvent.change(await screen.findByLabelText("计划文件"), { target: { files: [file(lines)] } });
  await userEvent.type(within(screen.getByRole("region", { name: "导入计划文件" })).getByLabelText("来源名"), source);
}
const rec = (n: number) => Array.from({ length: n }, (_, i) => `{"type":"item","external_id":"T-${i}"}`);

beforeEach(() => sessionStorage.clear());

describe("设置 › 导入", () => {
  it("ACT-WIM-01: 先预检（dry_run）显示新建、已存在、不合法及原因；确认后按 500 条分批写入，后批沿用同一批次号并显示进度", async () => {
    const s = server();
    mount(s.impl);
    await choose([...rec(599), '{"bad":1}']);
    await userEvent.click(screen.getByRole("button", { name: "预检" }));
    const pre = within(await screen.findByRole("region", { name: "预检结果" }));
    expect(pre.getByText(/会新建 599 条；已存在 0 条；不合法 1 条/)).toBeInTheDocument();
    expect(pre.getByText(/第 600 行（BAD）：history must start with created/)).toBeInTheDocument(); // 第二块第 100 行 = 全文件第 600 行
    expect(s.posts.every((p) => p.url.includes("dry_run=1") && !p.url.includes("batch="))).toBe(true); // 预检不带批次、不写入
    expect(s.posts.map((p) => p.lines)).toEqual([500, 100]);
    await userEvent.click(pre.getByRole("button", { name: "确认导入" }));
    expect(await screen.findByText("导入完成（imp_new），新建 599 条")).toBeInTheDocument();
    const writes = s.posts.filter((p) => !p.url.includes("dry_run"));
    expect(writes.map((p) => p.lines)).toEqual([500, 100]);
    expect(writes[0].url).not.toContain("batch=");
    expect(writes[1].url).toContain("batch=imp_new");
    expect(writes.every((p) => p.url.includes("source=legacy") && p.ct === "application/x-ndjson")).toBe(true);
  });

  it("写入中途被拒绝：原因原样显示，已写入的留着，从断点沿用批次号继续", async () => {
    const s = server({ failChunk: 4 }); // 预检 2 次，写入第 1 批成功、第 2 批失败
    mount(s.impl);
    await choose(rec(600));
    await userEvent.click(screen.getByRole("button", { name: "预检" }));
    await userEvent.click(await screen.findByRole("button", { name: "确认导入" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("disk is full");
    expect(screen.getByRole("status")).toHaveTextContent("已写入 1 / 2 批");
    await userEvent.click(screen.getByRole("button", { name: "从断点继续" }));
    expect(await screen.findByText("导入完成（imp_new），新建 600 条")).toBeInTheDocument();
    const writes = s.posts.filter((p) => !p.url.includes("dry_run"));
    expect(writes.map((p) => p.lines)).toEqual([500, 100, 100]);
    expect(writes[2].url).toContain("batch=imp_new");
  });

  it("来源名不合法或文件为空时不发请求", async () => {
    const s = server();
    mount(s.impl);
    await choose(rec(1), "Bad Name");
    await userEvent.click(screen.getByRole("button", { name: "预检" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("来源名只能用小写字母");
    expect(s.posts).toHaveLength(0);
  });

  it("ACT-WIM-02: 下方列出历次批次（来源名、条数、时间、操作者、授权单），点开看这一批写了哪些事项", async () => {
    mount(server().impl);
    const row = within(await screen.findByRole("group", { name: "imp_old" }));
    expect(row.getByText(/legacy/)).toBeInTheDocument();
    expect(row.getByText(/新建: 3 · 已存在: 1 · 不合法: 0/)).toBeInTheDocument();
    expect(row.getByText(/操作者: owner · 授权: gr_1/)).toBeInTheDocument();
    await userEvent.click(row.getByRole("button", { name: "查看批次 imp_old" }));
    const list = await screen.findByRole("list", { name: "批次 imp_old 写入的事项" });
    expect(within(list).getByRole("link", { name: "itm_a" })).toHaveAttribute("href", "/items/itm_a");
    expect(within(list).getByRole("link", { name: "itm_b" })).toBeInTheDocument();
  });

  it("ACT-WIM-02: 一个批次超过 500 条事项时翻页取全，不截断", async () => {
    mount(server().impl);
    const row = within(await screen.findByRole("group", { name: "imp_big" }));
    await userEvent.click(row.getByRole("button", { name: "查看批次 imp_big" }));
    const list = await screen.findByRole("list", { name: "批次 imp_big 写入的事项" });
    await waitFor(() => expect(within(list).getAllByRole("link")).toHaveLength(520));
    expect(within(list).getByRole("link", { name: "itm_501" })).toBeInTheDocument();
    expect(within(list).getByRole("link", { name: "itm_520" })).toBeInTheDocument();
  });

  it("ACT-WIM-02: 签发导入用的授权：只含 import.apply，带来源名、条数上限与有效期（≤7 天）", async () => {
    const s = server();
    mount(s.impl);
    const form = within(await screen.findByRole("form", { name: "签发导入用的授权" }));
    await userEvent.type(form.getByLabelText("给哪个 Agent"), "codex");
    await userEvent.type(form.getByLabelText("来源名"), "legacy");
    await userEvent.type(form.getByLabelText("条数上限"), "1000");
    const before = Date.now();
    await userEvent.click(form.getByRole("button", { name: "签发" }));
    expect(await form.findByText("已签发授权 gr_9")).toBeInTheDocument();
    const g = s.grants[0] as { grantee: unknown; actions: string[]; import: unknown; expires_at: string };
    expect(g.actions).toEqual(["import.apply"]);
    expect(g.grantee).toEqual({ agent: "codex" });
    expect(g.import).toEqual({ source: "legacy", max_records: 1000 });
    expect(Date.parse(g.expires_at) - before).toBeGreaterThan(23 * 3600_000);
    expect(Date.parse(g.expires_at) - before).toBeLessThanOrEqual(24 * 3600_000 + 5000);
  });

  it("授权单的有效期超过 168 小时时不发请求", async () => {
    const s = server();
    mount(s.impl);
    const form = within(await screen.findByRole("form", { name: "签发导入用的授权" }));
    await userEvent.type(form.getByLabelText("给哪个 Agent"), "codex");
    await userEvent.type(form.getByLabelText("来源名"), "legacy");
    await userEvent.type(form.getByLabelText("条数上限"), "10");
    await userEvent.clear(form.getByLabelText("有效小时数"));
    await userEvent.type(form.getByLabelText("有效小时数"), "200");
    await userEvent.click(form.getByRole("button", { name: "签发" }));
    expect(await form.findByRole("alert")).toHaveTextContent("最长 168 小时");
    expect(s.grants).toHaveLength(0);
  });
});
