import { withSession } from "../../app/test-session";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/why 的 GET /v1/audit/events 契约应答（actor/object/type 精确匹配，order=desc 时最新在前、before/limit 分页（缺省升序、after 游标），错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const A = (type: string, principal: string, via: string) => ({ type, principal, via });
interface Ev { seq: number; type: string; object: string; actor: ReturnType<typeof A>; at: string; reason?: string; chain_depth: number; redacted?: boolean; basis?: { via?: string }; grant?: string; imported?: string; recorded_at?: string }

const ev = (seq: number, type: string, object: string, actor: Ev["actor"], reason?: string): Ev =>
  ({ seq, type, object, actor, at: "2030-01-01T00:00:00Z", reason, chain_depth: 0 });

function ledger(extra: Ev[] = [], fail?: () => Response | null) {
  const events: Ev[] = [
    ev(1, "item.created", "item_a", A("agent", "del-1", "cli")),
    ev(2, "item.phase_changed", "item_a", A("human", "owner", "web"), "ship it"),
    ev(3, "command.rejected", "item_b", A("agent", "del-2", "cli"), "exit 20: high-risk command needs web confirmation"),
    ...extra,
  ];
  const queries: URLSearchParams[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "src_mail_qq", name: "QQ 邮箱" }] });
    const u = new URL(url, "http://x");
    if (u.pathname !== "/v1/audit/events") return json(404, { code: "not_found", why: "no such route", next: "" });
    if ((init.method ?? "GET") !== "GET") throw new Error("audit page must be read-only");
    queries.push(u.searchParams);
    const failed = fail?.();
    if (failed) return failed;
    const q = u.searchParams;
    const after = Number(q.get("after") ?? 0);
    const before = Number(q.get("before") ?? Infinity);
    const limit = Number(q.get("limit") ?? 100);
    const desc = q.get("order") === "desc";
    const out = (desc ? [...events].reverse() : events).filter((e) =>
      e.seq > after && e.seq < before &&
      (!q.get("actor") || e.actor.principal === q.get("actor")) &&
      (!q.get("object") || e.object === q.get("object")) &&
      (!q.get("type") || e.type === q.get("type")));
    return json(200, { events: out.slice(0, limit) });
  }) as unknown as typeof fetch;
  return { impl, queries };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/audit"]}>
      <App features={[feature]} fetchImpl={withSession(fetchImpl)} />
    </MemoryRouter>,
  );
}
const rows = async () => within(await screen.findByRole("list", { name: "操作记录" }));
const count = async () => (await rows()).queryAllByRole("listitem").length;

beforeEach(() => sessionStorage.clear());
afterEach(() => localStorage.removeItem("apa.raw"));

describe("诊断 › 操作记录", () => {
  it("flow-why-audit: 默认列出全量 Event，每行一句话：谁 · 做了什么 · 对象 · 时间", async () => {
    mount(ledger().impl);
    const r = await rows();
    await waitFor(() => expect(r.getAllByRole("listitem")).toHaveLength(3));
    expect(r.getAllByLabelText("操作者").map((b) => b.textContent)).toEqual([
      "agent · 命令行 · del-2", "你 · 网页", "agent · 命令行 · del-1", // 最新在前；本人与系统不重复显示身份串（共用的 ActorBadge）
    ]);
    expect(r.getAllByRole("listitem")[1]).toHaveTextContent(/你 · 网页\s*改了阶段 item_a/);
    expect(r.queryByText(/ship it|#2/)).not.toBeInTheDocument(); // 原始值只在「显示原始值」里
  });

  it("对象是来源时按名字称呼（§9.1）：改名、停用都写「QQ 邮箱」，不出现 src_", async () => {
    mount(ledger([ev(4, "source.renamed", "src_mail_qq", A("human", "owner", "web")), ev(5, "source.disabled", "src_mail_qq", A("system", "kernel:sources", "clock"))]).impl);
    const r = await rows();
    await waitFor(() => expect(r.getAllByRole("listitem")[1]).toHaveTextContent(/改了来源名字 QQ 邮箱/));
    expect(r.getAllByRole("listitem")[0]).toHaveTextContent("QQ 邮箱");
    expect(document.querySelector("main")!.textContent, "主界面不出现来源 id").not.toMatch(/src_/);
  });

  it("flow-why-audit: 打开「显示原始值」后每行带序号、类型与原因", async () => {
    localStorage.setItem("apa.raw", "1");
    mount(ledger().impl);
    const r = await rows();
    expect(await r.findByText(/#2 · item.phase_changed · item_a · ship it/)).toBeInTheDocument();
  });

  it("flow-why-audit: 按谁、做了什么、对象筛选（服务端过滤，可组合）", async () => {
    const l = ledger();
    mount(l.impl);
    await waitFor(async () => expect(await count()).toBe(3));

    await userEvent.selectOptions(screen.getByLabelText("谁"), "你");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(async () => expect(await count()).toBe(1));
    expect((await rows()).getByText(/改了阶段/)).toBeInTheDocument();
    expect(l.queries.at(-1)!.get("actor")).toBe("owner");

    await userEvent.selectOptions(screen.getByLabelText("谁"), "所有人");
    await userEvent.click(screen.getByText("按对象查"));
    await userEvent.type(screen.getByLabelText("对象（事项或材料的 id）"), "item_a");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(async () => expect(await count()).toBe(2));

    await userEvent.selectOptions(screen.getByLabelText("做了什么"), "新建了事项");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(async () => expect(await count()).toBe(1));
    const last = l.queries.at(-1)!;
    expect([last.get("object"), last.get("type"), last.get("actor")]).toEqual(["item_a", "item.created", null]);
  });

  it("「做了什么」按组列出，选项是中文", async () => {
    mount(ledger().impl);
    const sel = await screen.findByLabelText("做了什么");
    expect([...sel.querySelectorAll("optgroup")].map((g) => g.label)).toContain("事项与决策");
    expect(within(sel).getByRole("option", { name: "新建了事项" })).toHaveValue("item.created");
  });

  it("flow-why-audit: 被拒绝的操作可一键查出", async () => {
    mount(ledger().impl);
    await waitFor(async () => expect(await count()).toBe(3));
    await userEvent.click(screen.getByRole("button", { name: "只看被拒绝的操作" }));
    await waitFor(async () => expect(await count()).toBe(1));
    expect((await rows()).getByText(/操作被拒绝/)).toBeInTheDocument();
  });

  it("flow-why-audit: 没有匹配时显示空态；最新的在最前，「加载更多」用 before 游标取更早的", async () => {
    const many = Array.from({ length: 101 }, (_, i) => ev(4 + i, "item.updated", "item_z", A("system", "clock", "clock")));
    const l = ledger(many);
    mount(l.impl);
    await waitFor(async () => expect(await count()).toBe(100));
    const lis = async () => (await rows()).getAllByRole("listitem");
    expect(l.queries[0].get("order")).toBe("desc");
    expect(l.queries[0].get("before")).toBeNull();
    expect((await lis())[0]).toHaveTextContent("item_z"); // 第一条是最新的（seq 104）
    expect((await rows()).queryByText(/item_a/)).toBeNull(); // 最早的两条（seq 1、2）还没加载
    await userEvent.click(screen.getByRole("button", { name: "加载更多" }));
    await waitFor(async () => expect(await count()).toBe(104));
    expect(l.queries.at(-1)!.get("before")).toBe("5"); // 第一页最后一条（seq 5）之前的
    expect(l.queries.at(-1)!.get("after")).toBeNull();
    expect((await lis()).at(-1)).toHaveTextContent(/新建了事项 item_a/); // 更早的接在后面，最早的在最末，不重复
    expect(screen.queryByRole("button", { name: "加载更多" })).toBeNull();

    await userEvent.click(screen.getByText("按对象查"));
    await userEvent.type(screen.getByLabelText("对象（事项或材料的 id）"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    expect(await screen.findByText("没有符合条件的记录")).toBeInTheDocument();
  });

  it("flow-web-only: 只读且只调 HTTP API；服务端错误原因可查看，不留假数据", async () => {
    let bad = false;
    const l = ledger([], () => bad ? json(400, { code: "invalid_request", why: "after or limit is out of range", next: "use after >= 0" }) : null);
    mount(l.impl);
    await waitFor(async () => expect(await count()).toBe(3));
    expect(screen.queryByRole("button", { name: /撤销|回退|删除/ })).toBeNull();

    bad = true;
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("after or limit is out of range");
    expect(screen.queryByRole("list", { name: "操作记录" })).toBeNull();
  });
});

describe("凭什么生效（设计 §14.3）", () => {
  it("新记录按 basis.via 写明依据；切换前带授权单的记录标为旧模型记录，不再给链接", async () => {
    const l = ledger([
      { ...ev(4, "decision.resolved", "item_g", A("agent", "del-1", "cli")), grant: "grt_ok" },
      { ...ev(5, "item.updated", "item_h", A("agent", "del-1", "cli")), basis: { via: "allowance" } },
    ]);
    mount(l.impl);
    const r = await rows();
    await waitFor(() => expect(r.getAllByRole("listitem")).toHaveLength(5));
    const items = r.getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("按它的档位");
    expect(items[1]).toHaveTextContent("旧模型记录");
    expect(within(items[1]).queryByRole("link", { name: /授权/ })).toBeNull();
  });
});

describe("导入批次折叠（ACT-WIM-03、flow-web-import）", () => {
  const imp = (seq: number, type: string, batch: string): Ev => ({ ...ev(seq, type, "item_i" + seq, A("human", "owner", "cli")), imported: batch, recorded_at: "2030-06-01T00:00:00Z" });

  it("一个批次的连续事件折叠成一行「导入 N 条」，展开看逐条；其余事件照常一条一行", async () => {
    mount(ledger([imp(4, "item.created", "imp_1"), imp(5, "item.phase_changed", "imp_1"), imp(6, "item.progress_added", "imp_1"), ev(7, "item.updated", "item_a", A("human", "owner", "web"))]).impl);
    const r = await rows();
    await waitFor(() => expect(r.getByText(/导入 3 条/)).toBeInTheDocument());
    const top = r.getAllByRole("listitem").filter((li) => !li.parentElement!.closest("li"));
    expect(top).toHaveLength(5); // 普通 3 + 新 1 + 折叠 1
    expect(top[1]).toHaveTextContent("导入 3 条"); // seq 7 在最前，折叠组位于 seq 3 之前
    const group = top[1].querySelector("details")!;
    expect(group.open).toBe(false);
    expect(group.querySelectorAll("li")).toHaveLength(3);
    await userEvent.click(within(top[1]).getByText(/导入 3 条/));
    expect(group.open).toBe(true);
    expect(within(top[1]).getByText(/新建了事项 item_i4/)).toBeInTheDocument();
  });

  it("不同批次分开折叠", async () => {
    mount(ledger([imp(4, "item.created", "imp_1"), imp(5, "item.created", "imp_2"), imp(6, "item.created", "imp_2")]).impl);
    const r = await rows();
    await waitFor(() => expect(r.getByText(/导入 2 条/)).toBeInTheDocument());
    expect(r.getByText(/导入 1 条/)).toBeInTheDocument();
  });

  it("没有导入事件时与原来一样，不出现折叠", async () => {
    mount(ledger().impl);
    await waitFor(async () => expect(await count()).toBe(3));
    expect(document.querySelector("details.import-fold")).toBeNull();
  });
});
