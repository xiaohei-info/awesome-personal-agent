import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/runs（GET /runs?state=&category=&before=&limit=、升序、只留最新 limit 条）的契约应答，字段对应设计 §6.11。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const bg = { id: "r1", category: "background", runner: "main_session", launch: "headless", target: "i1", harness: "hermes", session_id: "sess-abc", access: "edit", basis: { policy_version: 7, rule_id: "handle-labelled" }, chain_depth: 1, state: "running", started_at: "2030-01-01T00:00:00Z", timeout: 600 };
const q1 = { id: "r2", category: "task", target: "i2", chain_depth: 0, state: "queued" };
const failed = { id: "r3", category: "source", target: "src1", chain_depth: 0, state: "failed", exit_code: 3, started_at: "2030-01-01T00:00:00Z", finished_at: "2030-01-01T00:01:00Z" };
const pull = (n: number, target = "src1", state = "succeeded") => ({ id: `p${n}`, category: "source", target, chain_depth: 0, state, exit_code: 0, started_at: "2030-01-02T00:00:00Z", finished_at: "2030-01-02T00:00:05Z" });
const timedOut = { id: "r4", category: "source", target: "src2", chain_depth: 0, state: "timeout", started_at: "2030-01-03T00:00:00Z", finished_at: "2030-01-03T00:10:00Z", timeout: 600 };

function server(all: object[]) {
  const calls: string[] = [];
  const impl = (async (url: string) => {
    calls.push(url);
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src1", name: "Gmail" }, { id: "src2", name: "日历" }] });
    if (url === "/v1/items/i1") return json(200, { item: { id: "i1", title: "整理周报" } });
    if (url === "/v1/items/i2") return json(200, { item: { id: "i2", title: "写周报" } });
    if (url.startsWith("/v1/runs?")) {
      const p = new URLSearchParams(url.split("?")[1]);
      const set = (k: string) => (p.get(k) ? p.get(k)!.split(",") : null);
      const states = set("state"), cats = set("category"), before = p.get("before");
      let list = all as { id: string; state: string; category: string }[];
      if (before) list = list.slice(0, list.findIndex((r) => r.id === before));
      list = list.filter((r) => (!states || states.includes(r.state)) && (!cats || cats.includes(r.category)));
      return json(200, { runs: list.slice(-Number(p.get("limit"))) });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const lines = () => within(screen.getByRole("list", { name: "运行日志" })).getAllByRole("button").map((b) => b.textContent ?? "");

beforeEach(() => sessionStorage.clear());
afterEach(() => localStorage.removeItem("apa.raw"));

describe("诊断 › 运行日志（flow-bg-main-session、flow-outcall-limits）", () => {
  it("一行一条、最新在上：用用户的话说做了什么，结果带状态，用时在行尾；没有「详情」按钮", async () => {
    mount(server([failed, q1, bg]).impl);
    expect(await screen.findByRole("heading", { name: "运行日志" })).toBeInTheDocument();
    await screen.findByText("拉取 Gmail");
    const rows = lines();
    expect(rows).toHaveLength(3);
    expect(rows[0]).toContain("Agent 后台运行：整理周报");
    expect(rows[0]).toContain("运行中");
    expect(rows[1]).toContain("执行任务：写周报");
    expect(rows[1]).toContain("排队中");
    expect(rows[1]).not.toMatch(/\d+s|\d+m/); // 缺省字段不伪造用时
    expect(rows[2]).toContain("拉取 Gmail");
    expect(rows[2]).toContain("失败");
    expect(rows[2]).toContain("1m");
    expect(screen.queryByRole("button", { name: "详情" })).not.toBeInTheDocument();
  });

  it("点一行就地展开：规则、会话、权限、连锁层数、退出码、时限；策略版本与 id 在原始值里；再点收起", async () => {
    localStorage.setItem("apa.raw", "1");
    mount(server([failed, bg]).impl);
    const row = await screen.findByRole("button", { name: /Agent 后台运行：整理周报/ });
    expect(row).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("会话 sess-abc")).not.toBeInTheDocument();
    await userEvent.click(row);
    expect(row).toHaveAttribute("aria-expanded", "true");
    for (const part of ["由规则「handle-labelled」发起", "会话 sess-abc", "权限：可改文件", "连锁第 1 层", "时限 600 秒", "policy v7", "r1"]) {
      expect(await screen.findByText(new RegExp(part))).toBeInTheDocument();
    }
    expect(screen.getByRole("link", { name: "查看事项" })).toHaveAttribute("href", "/items/i1");
    await userEvent.click(row);
    expect(screen.queryByText("会话 sess-abc")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /拉取 Gmail/ }));
    expect(await screen.findByText("退出码 3")).toBeInTheDocument();
  });

  it("不开原始值时 id 不出现", async () => {
    mount(server([bg]).impl);
    await userEvent.click(await screen.findByRole("button", { name: /Agent 后台运行：/ }));
    await screen.findByText("会话 sess-abc");
    expect(document.body).not.toHaveTextContent(/policy v7|hermes/);
  });

  it("超时用单独的颜色，与失败、成功区分", async () => {
    mount(server([timedOut, failed, pull(1, "src2")]).impl);
    await screen.findAllByText("拉取 日历");
    const badge = (text: string) => screen.getByText(text, { selector: "strong" });
    expect(badge("超时")).toHaveClass("warn");
    expect(badge("失败")).toHaveClass("bad");
    expect(badge("成功")).toHaveClass("ok");
  });

  it("类型与「只看失败」走服务端 ?category=、?state=，且没有重试按钮", async () => {
    const s = server([failed, q1, bg, timedOut, pull(1)]);
    mount(s.impl);
    await screen.findByText("Agent 后台运行：整理周报");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "类型" }), "Agent 任务");
    await screen.findByText("执行任务：写周报");
    expect(lines()).toHaveLength(2);
    expect(s.calls).toContain("/v1/runs?limit=100&category=task%2Cbackground");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "类型" }), "来源拉取");
    await userEvent.click(screen.getByRole("checkbox", { name: "只看失败" }));
    await screen.findByText("拉取 日历");
    expect(s.calls).toContain("/v1/runs?limit=100&category=source&state=failed%2Ctimeout");
    expect(lines()).toHaveLength(2); // 失败 + 超时，成功的拉取不在
    expect(screen.queryByRole("button", { name: /重试(?!加载)/ })).not.toBeInTheDocument();
  });

  it("同一来源连续成功的拉取折成一行，点开看明细；失败单独一行并截断折叠", async () => {
    mount(server([pull(1), pull(2), pull(3, "src1", "failed"), pull(4), pull(5), pull(6, "src2")]).impl);
    await screen.findByText("拉取 日历");
    const rows = lines();
    expect(rows).toHaveLength(4);
    expect(rows[1]).toContain("拉取 Gmail · 成功 ×2");
    expect(rows[2]).toContain("失败");
    expect(rows[3]).toContain("拉取 Gmail · 成功 ×2");
    await userEvent.click(screen.getAllByRole("button", { name: /成功 ×2/ })[0]);
    const kids = screen.getAllByRole("listitem").filter((li) => li.closest("ul.log-children"));
    expect(kids).toHaveLength(2);
    await userEvent.click(within(kids[0]).getByRole("button"));
    expect(await screen.findByText(/开始/)).toBeInTheDocument();
  });

  it("底部「加载更多」带 before=最旧一条的 id 续读，读完不再显示", async () => {
    const many = Array.from({ length: 130 }, (_, i) => pull(i + 1, i % 2 ? "src1" : "src2", "failed")); // 交替来源、全失败：不折叠
    const s = server(many);
    mount(s.impl);
    await screen.findAllByText(/拉取/);
    expect(lines()).toHaveLength(100);
    await userEvent.click(screen.getByRole("button", { name: "加载更多" }));
    await waitFor(() => expect(lines()).toHaveLength(130));
    expect(s.calls).toContain("/v1/runs?limit=100&before=p31");
    expect(screen.queryByRole("button", { name: "加载更多" })).not.toBeInTheDocument();
  });

  it("空列表与读取失败（显示服务端原因，可重新加载）", async () => {
    let fail = true;
    const s = server([]);
    const impl = ((url: string) =>
      url.startsWith("/v1/runs?") ? Promise.resolve(fail ? json(403, { code: "permission_denied", why: "credential kind cannot read the ledger", next: "use an owner credential" }) : json(200, { runs: [] })) : s.impl(url)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("credential kind cannot read the ledger");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重新加载" }));
    expect(await screen.findByText("还没有运行记录")).toBeInTheDocument();
  });
});
