import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/why 的 GET /v1/audit/events 契约应答（actor/object/type 精确匹配，after/limit 分页，升序，错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const A = (type: string, principal: string, via: string) => ({ type, principal, via });
interface Ev { seq: number; type: string; object: string; actor: ReturnType<typeof A>; at: string; reason?: string; chain_depth: number; redacted?: boolean; grant?: string }

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
    const u = new URL(url, "http://x");
    if (u.pathname !== "/v1/audit/events") return json(404, { code: "not_found", why: "no such route", next: "" });
    if ((init.method ?? "GET") !== "GET") throw new Error("audit page must be read-only");
    queries.push(u.searchParams);
    const failed = fail?.();
    if (failed) return failed;
    const q = u.searchParams;
    const after = Number(q.get("after") ?? 0);
    const limit = Number(q.get("limit") ?? 100);
    const out = events.filter((e) =>
      e.seq > after &&
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
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
const rows = async () => within(await screen.findByRole("list", { name: "审计事件" }));
const count = async () => (await rows()).queryAllByRole("listitem").length;

beforeEach(() => sessionStorage.clear());

describe("审计页", () => {
  it("flow-why-audit: 默认列出全量 Event，每条带操作者徽章", async () => {
    mount(ledger().impl);
    const r = await rows();
    await waitFor(() => expect(r.getAllByRole("listitem")).toHaveLength(3));
    expect(r.getAllByLabelText("操作者").map((b) => b.textContent)).toEqual([
      "agent · 命令行 · del-1", "你 · 网页", "agent · 命令行 · del-2", // 本人与系统不重复显示身份串（共用的 ActorBadge）
    ]);
    expect(r.getByText(/ship it/)).toBeInTheDocument();
  });

  it("flow-why-audit: 按操作者、对象、事件类型筛选（服务端过滤，可组合）", async () => {
    const l = ledger();
    mount(l.impl);
    await waitFor(async () => expect(await count()).toBe(3));

    await userEvent.type(screen.getByLabelText("操作者筛选"), "owner");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(async () => expect(await count()).toBe(1));
    expect((await rows()).getByText(/改了阶段/)).toBeInTheDocument();

    await userEvent.clear(screen.getByLabelText("操作者筛选"));
    await userEvent.type(screen.getByLabelText("对象筛选"), "item_a");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(async () => expect(await count()).toBe(2));

    await userEvent.type(screen.getByLabelText("事件类型筛选"), "item.created");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(async () => expect(await count()).toBe(1));
    const last = l.queries.at(-1)!;
    expect([last.get("object"), last.get("type"), last.get("actor")]).toEqual(["item_a", "item.created", null]);
  });

  it("flow-why-audit: 被拒绝的命令可一键查出，并显示拒绝原因", async () => {
    mount(ledger().impl);
    await waitFor(async () => expect(await count()).toBe(3));
    await userEvent.click(screen.getByRole("button", { name: "只看被拒绝的命令" }));
    await waitFor(async () => expect(await count()).toBe(1));
    const r = await rows();
    expect(r.getByText(/操作被拒绝/)).toBeInTheDocument();
    expect(r.getByText(/high-risk command needs web confirmation/)).toBeInTheDocument();
  });

  it("flow-why-audit: 没有匹配时显示空态；分页用 after 游标加载更多", async () => {
    const many = Array.from({ length: 101 }, (_, i) => ev(4 + i, "item.updated", "item_z", A("system", "clock", "clock")));
    const l = ledger(many);
    mount(l.impl);
    await waitFor(async () => expect(await count()).toBe(100));
    await userEvent.click(screen.getByRole("button", { name: "加载更多" }));
    await waitFor(async () => expect(await count()).toBe(104));
    expect(l.queries.at(-1)!.get("after")).toBe("100");
    expect(screen.queryByRole("button", { name: "加载更多" })).toBeNull();

    await userEvent.type(screen.getByLabelText("对象筛选"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    expect(await screen.findByText("没有符合条件的事件")).toBeInTheDocument();
  });

  it("flow-web-only: 只读且只调 HTTP API；服务端错误原因原样显示，不留假数据", async () => {
    let bad = false;
    const l = ledger([], () => bad ? json(400, { code: "invalid_request", why: "after or limit is out of range", next: "use after >= 0" }) : null);
    mount(l.impl);
    await waitFor(async () => expect(await count()).toBe(3));
    expect(screen.queryByRole("button", { name: /撤销|回退|删除/ })).toBeNull();

    bad = true;
    await userEvent.click(screen.getByRole("button", { name: "筛选" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("after or limit is out of range");
    expect(screen.queryByRole("list", { name: "审计事件" })).toBeNull();
  });
});

describe("授权单依据（ACT-AUTH-03、flow-why-audit）", () => {
  it("依据授权单的变更显示授权单 id；其他事件不带", async () => {
    const l = ledger([{ ...ev(4, "decision.resolved", "item_g", A("agent", "del-1", "cli")), grant: "grt_ok" }]);
    mount(l.impl);
    const row = (await screen.findByText(/#4 ·/)).closest("li")!;
    expect(row).toHaveTextContent("授权单 grt_ok");
    expect(screen.getByText(/#1 ·/).closest("li")).not.toHaveTextContent("授权单");
  });
});
